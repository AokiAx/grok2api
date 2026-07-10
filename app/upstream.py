from __future__ import annotations

import asyncio
import logging
from typing import Any, AsyncIterator

import httpx

from .auth import AuthStore, auth_store
from .compat import aggregate_sse_to_completion
from .config import Settings, settings

log = logging.getLogger("grok2api.upstream")

# Rate-limit / auth issues that warrant switching CLI pool account
_ROTATE_STATUSES = {401, 403, 429}


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

    async def _token_pair(
        self,
        force_refresh: bool = False,
        exclude_ids: set[str] | None = None,
    ) -> tuple[str, str | None]:
        return await asyncio.to_thread(
            self.store.get_access_token_with_account,
            force_refresh,
            exclude_ids,
        )

    def _release(self, account_id: str | None) -> None:
        if not account_id:
            return
        try:
            from .cli_pool import cli_pool

            cli_pool.release(account_id)
        except Exception:
            log.debug("release failed for %s", account_id, exc_info=True)

    def _report_success(self, account_id: str | None) -> None:
        if not account_id:
            return
        try:
            from .cli_pool import cli_pool

            cli_pool.report_success(account_id)
        except Exception:
            log.debug("report_success failed for %s", account_id, exc_info=True)

    def _report_failure(
        self,
        account_id: str | None,
        *,
        cooldown_secs: float,
    ) -> None:
        if not account_id:
            return
        try:
            from .cli_pool import cli_pool

            cli_pool.report_failure(account_id, cooldown_secs=cooldown_secs)
        except Exception:
            log.debug("report_failure failed for %s", account_id, exc_info=True)

    def _pool_max_tries(self) -> int:
        try:
            from .cli_pool import cli_pool

            n = cli_pool.count(enabled_only=True)
        except Exception:
            n = 0
        return max(1, min(n or 1, 5))

    async def _request_with_auth_retry(
        self,
        method: str,
        path: str,
        *,
        model: str,
        stream: bool = False,
        json_body: dict[str, Any] | None = None,
    ) -> tuple[httpx.Response, str | None]:
        """Return ``(response, account_id)``. Caller must ``release(account_id)``."""

        async def once(token: str) -> httpx.Response:
            headers = self._headers(model, token, stream=stream)
            req = self._client.build_request(
                method,
                path,
                headers=headers,
                json=json_body,
            )
            return await self._client.send(req, stream=stream)

        max_tries = self._pool_max_tries()
        last_code = 0
        held_id: str | None = None
        attempted_ids: set[str] = set()

        try:
            for attempt in range(max_tries):
                # drop previous slot before acquiring a new account
                if held_id:
                    self._release(held_id)
                    held_id = None

                force = attempt > 0 and last_code in (401, 403)
                token, account_id = await self._token_pair(
                    force_refresh=force,
                    exclude_ids=attempted_ids,
                )
                held_id = account_id

                resp = await once(token)
                code = resp.status_code
                last_code = code

                if code not in _ROTATE_STATUSES:
                    # transfer ownership of held_id to caller
                    out_id, held_id = held_id, None
                    return resp, out_id

                body_preview = ""
                try:
                    body_preview = (await resp.aread())[:240].decode(
                        "utf-8", "replace"
                    )
                except Exception:
                    pass
                await resp.aclose()

                if account_id:
                    attempted_ids.add(account_id)
                    try:
                        from .cli_pool import cli_pool

                        if code == 429:
                            cli_pool.report_failure(
                                account_id, cooldown_secs=45, disable=False
                            )
                            log.warning(
                                "upstream 429 on %s; cool down account=%s try=%s/%s %s",
                                path,
                                account_id,
                                attempt + 1,
                                max_tries,
                                body_preview[:120],
                            )
                        else:
                            cli_pool.report_failure(
                                account_id, cooldown_secs=90, disable=False
                            )
                            log.warning(
                                "upstream %s on %s; rotate account=%s try=%s/%s %s",
                                code,
                                path,
                                account_id,
                                attempt + 1,
                                max_tries,
                                body_preview[:120],
                            )
                    except Exception:
                        log.debug("report_failure failed", exc_info=True)

            raise RuntimeError(
                f"all eligible CLI accounts failed for {path}; "
                f"attempted={len(attempted_ids)} last_status={last_code}"
            )
        except Exception:
            if held_id:
                self._release(held_id)
            raise

    async def list_models(self) -> dict[str, Any]:
        r, acc = await self._request_with_auth_retry(
            "GET",
            "/models",
            model=self.cfg.default_model,
            stream=False,
        )
        try:
            r.raise_for_status()
            self._report_success(acc)
            return r.json()
        finally:
            await r.aclose()
            self._release(acc)

    async def billing(self) -> dict[str, Any]:
        r, acc = await self._request_with_auth_retry(
            "GET",
            "/billing",
            model=self.cfg.default_model,
            stream=False,
        )
        try:
            r.raise_for_status()
            self._report_success(acc)
            return r.json()
        finally:
            await r.aclose()
            self._release(acc)

    async def responses(self, body: dict[str, Any]) -> httpx.Response | AsyncIterator[bytes]:
        model = str(body.get("model") or self.cfg.default_model)
        stream = bool(body.get("stream"))
        body = {**body, "model": model}
        resp, acc = await self._request_with_auth_retry(
            "POST",
            "/responses",
            model=model,
            stream=stream,
            json_body=body,
        )
        if stream:
            return self._iter_sse(resp, release_account=acc)
        try:
            if resp.status_code >= 400:
                text = (await resp.aread()).decode("utf-8", "replace")
                raise httpx.HTTPStatusError(
                    f"upstream {resp.status_code}: {text[:800]}",
                    request=resp.request,
                    response=resp,
                )
            self._report_success(acc)
            return resp
        except Exception:
            await resp.aclose()
            raise
        finally:
            # free concurrency slot; response body remains readable by caller
            self._release(acc)

    async def chat_completions(
        self, body: dict[str, Any]
    ) -> dict[str, Any] | AsyncIterator[bytes]:
        model = str(body.get("model") or self.cfg.default_model)
        body = {**body, "model": model}
        want_stream = bool(body.get("stream"))

        if want_stream:
            return await self._chat_stream(body, model)

        force = self.cfg.force_upstream_stream
        if force:
            return await self._chat_nonstream_via_stream(body, model)

        try:
            return await self._chat_nonstream(body, model)
        except httpx.HTTPStatusError as e:
            status = e.response.status_code if e.response is not None else 0
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
        resp, acc = await self._request_with_auth_retry(
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
            payload = resp.json()
            self._report_success(acc)
            return payload
        except Exception:
            if resp.status_code not in _ROTATE_STATUSES:
                self._report_failure(acc, cooldown_secs=30)
            raise
        finally:
            await resp.aclose()
            self._release(acc)

    async def _chat_stream(
        self, body: dict[str, Any], model: str
    ) -> AsyncIterator[bytes]:
        payload = {**body, "stream": True}
        resp, acc = await self._request_with_auth_retry(
            "POST",
            "/chat/completions",
            model=model,
            stream=True,
            json_body=payload,
        )
        return self._iter_sse(resp, release_account=acc)

    async def _chat_nonstream_via_stream(
        self, body: dict[str, Any], model: str
    ) -> dict[str, Any]:
        stream_iter = await self._chat_stream(body, model)
        return await aggregate_sse_to_completion(stream_iter, model=model)

    async def _iter_sse(
        self,
        resp: httpx.Response,
        *,
        release_account: str | None = None,
    ) -> AsyncIterator[bytes]:
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
            self._report_success(release_account)
        except Exception:
            if resp.status_code not in _ROTATE_STATUSES:
                self._report_failure(release_account, cooldown_secs=30)
            raise
        finally:
            await resp.aclose()
            self._release(release_account)


upstream = UpstreamClient()
