from __future__ import annotations

import logging
from typing import Any, AsyncIterator

import httpx

from .auth import AuthStore, auth_store
from .compat import aggregate_sse_to_completion
from .config import Settings, settings

log = logging.getLogger("grok2api.upstream")


class UpstreamClient:
    def __init__(
        self,
        cfg: Settings | None = None,
        store: AuthStore | None = None,
    ) -> None:
        self.cfg = cfg or settings
        self.store = store or auth_store
        self._client = httpx.AsyncClient(
            base_url=self.cfg.proxy_base_url.rstrip("/"),
            timeout=httpx.Timeout(self.cfg.timeout_secs, connect=30.0),
            follow_redirects=True,
        )

    async def aclose(self) -> None:
        await self._client.aclose()

    def _headers(self, model: str, token: str, *, stream: bool = False) -> dict[str, str]:
        ver = self.cfg.resolved_client_version
        headers = {
            "Authorization": f"Bearer {token}",
            "X-XAI-Token-Auth": self.cfg.token_auth,
            "x-grok-client-version": ver,
            "x-grok-model-override": model,
            "User-Agent": self.cfg.resolved_user_agent,
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if stream else "application/json",
        }
        return headers

    async def _token(self, force_refresh: bool = False) -> str:
        import asyncio

        return await asyncio.to_thread(
            self.store.get_access_token, force_refresh
        )

    async def _request_with_auth_retry(
        self,
        method: str,
        path: str,
        *,
        model: str,
        stream: bool = False,
        json_body: dict[str, Any] | None = None,
    ) -> httpx.Response:
        async def once(token: str) -> httpx.Response:
            headers = self._headers(model, token, stream=stream)
            req = self._client.build_request(
                method,
                path,
                headers=headers,
                json=json_body,
            )
            return await self._client.send(req, stream=stream)

        token = await self._token()
        resp = await once(token)
        if resp.status_code in (401, 403):
            await resp.aclose()
            token = await self._token(force_refresh=True)
            resp = await once(token)
        return resp

    async def list_models(self) -> dict[str, Any]:
        r = await self._request_with_auth_retry(
            "GET",
            "/models",
            model=self.cfg.default_model,
            stream=False,
        )
        try:
            r.raise_for_status()
            return r.json()
        finally:
            await r.aclose()

    async def billing(self) -> dict[str, Any]:
        r = await self._request_with_auth_retry(
            "GET",
            "/billing",
            model=self.cfg.default_model,
            stream=False,
        )
        try:
            r.raise_for_status()
            return r.json()
        finally:
            await r.aclose()

    async def responses(self, body: dict[str, Any]) -> httpx.Response | AsyncIterator[bytes]:
        """Passthrough OpenAI Responses API if upstream supports it."""
        model = str(body.get("model") or self.cfg.default_model)
        stream = bool(body.get("stream"))
        body = {**body, "model": model}
        resp = await self._request_with_auth_retry(
            "POST",
            "/responses",
            model=model,
            stream=stream,
            json_body=body,
        )
        if stream:
            return self._iter_sse(resp)
        try:
            if resp.status_code >= 400:
                text = (await resp.aread()).decode("utf-8", "replace")
                raise httpx.HTTPStatusError(
                    f"upstream {resp.status_code}: {text[:800]}",
                    request=resp.request,
                    response=resp,
                )
            return resp
        except Exception:
            await resp.aclose()
            raise

    async def chat_completions(
        self, body: dict[str, Any]
    ) -> dict[str, Any] | AsyncIterator[bytes]:
        """
        Returns:
          - dict for non-stream (already parsed JSON)
          - AsyncIterator[bytes] for stream SSE bytes
        """
        model = str(body.get("model") or self.cfg.default_model)
        body = {**body, "model": model}
        want_stream = bool(body.get("stream"))

        # Client wants SSE: always stream upstream
        if want_stream:
            return await self._chat_stream(body, model)

        # Client wants a single JSON object
        force = self.cfg.force_upstream_stream
        if force:
            return await self._chat_nonstream_via_stream(body, model)

        try:
            return await self._chat_nonstream(body, model)
        except httpx.HTTPStatusError as e:
            status = e.response.status_code if e.response is not None else 0
            # Stream-only clusters often return 4xx for non-stream
            if self.cfg.stream_fallback and status in (400, 404, 405, 415, 422, 501):
                log.warning(
                    "non-stream failed (%s); falling back to stream aggregation",
                    status,
                )
                return await self._chat_nonstream_via_stream(body, model)
            raise

    async def _chat_nonstream(
        self, body: dict[str, Any], model: str
    ) -> dict[str, Any]:
        payload = {**body, "stream": False}
        resp = await self._request_with_auth_retry(
            "POST",
            "/chat/completions",
            model=model,
            stream=False,
            json_body=payload,
        )
        try:
            if resp.status_code >= 400:
                text = (await resp.aread()).decode("utf-8", "replace")
                raise httpx.HTTPStatusError(
                    f"upstream {resp.status_code}: {text[:800]}",
                    request=resp.request,
                    response=resp,
                )
            return resp.json()
        finally:
            await resp.aclose()

    async def _chat_stream(
        self, body: dict[str, Any], model: str
    ) -> AsyncIterator[bytes]:
        payload = {**body, "stream": True}
        resp = await self._request_with_auth_retry(
            "POST",
            "/chat/completions",
            model=model,
            stream=True,
            json_body=payload,
        )
        return self._iter_sse(resp)

    async def _chat_nonstream_via_stream(
        self, body: dict[str, Any], model: str
    ) -> dict[str, Any]:
        stream_iter = await self._chat_stream(body, model)
        return await aggregate_sse_to_completion(stream_iter, model=model)

    async def _iter_sse(self, resp: httpx.Response) -> AsyncIterator[bytes]:
        try:
            if resp.status_code >= 400:
                text = (await resp.aread()).decode("utf-8", "replace")
                raise httpx.HTTPStatusError(
                    f"upstream {resp.status_code}: {text[:800]}",
                    request=resp.request,
                    response=resp,
                )
            async for chunk in resp.aiter_bytes():
                if chunk:
                    yield chunk
        finally:
            await resp.aclose()


upstream = UpstreamClient()
