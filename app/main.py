from __future__ import annotations

import json
import logging
import time
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import httpx
from fastapi import Depends, FastAPI, Header, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import (
    FileResponse,
    HTMLResponse,
    JSONResponse,
    StreamingResponse,
)
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field

from . import __version__
from .admin import router as admin_router
from .auth import auth_store
from .compat import (
    fold_reasoning_in_completion,
    normalize_messages,
    openai_error,
    transform_sse_bytes,
)
from .config import settings
from .upstream import upstream
from .usage import usage_logger

_STATIC_DIR = Path(__file__).resolve().parent / "static"

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
log = logging.getLogger("grok2api")


@asynccontextmanager
async def lifespan(_app: FastAPI):
    ver = settings.resolved_client_version
    log.info(
        "grok2api %s starting; mode=cli proxy=%s data=%s client_version=%s",
        __version__,
        settings.proxy_base_url,
        settings.resolved_data_dir,
        ver,
    )
    try:
        from .cli_pool import cli_pool

        log.info("CLI pool usable=%s", cli_pool.count(enabled_only=True))
    except Exception:
        log.exception("CLI pool bootstrap failed")

    if settings.ensure_auth_on_start:
        try:
            from .oauth_login import try_refresh

            result = try_refresh(settings)
            if result and result.get("ok"):
                log.info(
                    "CLI auth warm via %s (~%ss left)",
                    result.get("method"),
                    result.get("seconds_left"),
                )
            else:
                st = auth_store.status()
                if not st.get("ok"):
                    log.warning(
                        "CLI auth not ready: %s — run: python -m app login",
                        st.get("error") or st,
                    )
        except Exception:
            log.exception("CLI auth warm-up failed")

    try:
        yield
    finally:
        try:
            from .cli_pool import cli_pool

            cli_pool.close()
        except Exception:
            log.exception("CLI pool shutdown flush failed")
        await upstream.aclose()


app = FastAPI(
    title="Grok2API",
    description=(
        "OpenAI-compatible local proxy over xAI Grok CLI (cli-chat-proxy). "
        "Credentials: OIDC access/refresh via login or CLI pool import."
    ),
    version=__version__,
    lifespan=lifespan,
)

_cors = settings.cors_origin_list()
if _cors:
    app.add_middleware(
        CORSMiddleware,
        allow_origins=_cors if _cors != ["*"] else ["*"],
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

app.include_router(admin_router)

if _STATIC_DIR.is_dir():
    app.mount("/static", StaticFiles(directory=str(_STATIC_DIR)), name="static")


@app.get("/panel", response_class=HTMLResponse)
@app.get("/manager", response_class=HTMLResponse)
async def panel_page() -> FileResponse:
    """Web control panel (CLI pool / billing / playground)."""
    path = _STATIC_DIR / "panel.html"
    if not path.exists():
        raise HTTPException(status_code=404, detail="panel.html missing")
    return FileResponse(path, media_type="text/html; charset=utf-8")


def require_local_key(
    authorization: str | None = Header(default=None),
    x_api_key: str | None = Header(default=None, alias="x-api-key"),
) -> None:
    expected = settings.api_key
    if not expected:
        return
    token = None
    if authorization and authorization.lower().startswith("bearer "):
        token = authorization[7:].strip()
    elif x_api_key:
        token = x_api_key.strip()
    if token != expected:
        raise HTTPException(
            status_code=401,
            detail=openai_error(
                "Invalid API key", status_code=401, code="invalid_api_key"
            )["error"],
        )


class ChatCompletionRequest(BaseModel):
    model: str | None = None
    messages: list[dict[str, Any]] = Field(default_factory=list)
    stream: bool = False
    temperature: float | None = None
    top_p: float | None = None
    max_tokens: int | None = None
    max_completion_tokens: int | None = None
    stop: Any | None = None
    tools: Any | None = None
    tool_choice: Any | None = None
    response_format: Any | None = None
    user: str | None = None
    n: int | None = None
    presence_penalty: float | None = None
    frequency_penalty: float | None = None
    logit_bias: Any | None = None
    seed: int | None = None
    reasoning_effort: str | None = None

    model_config = {"extra": "allow"}


class ResponsesRequest(BaseModel):
    model: str | None = None
    input: Any = None
    stream: bool = False
    instructions: str | None = None
    max_output_tokens: int | None = None
    temperature: float | None = None
    tools: Any | None = None
    reasoning: Any | None = None

    model_config = {"extra": "allow"}


def _normalize_model(model: str | None) -> str:
    if not model:
        return settings.default_model
    m = model.strip()
    aliases = {
        "gpt-4": settings.default_model,
        "gpt-4o": settings.default_model,
        "gpt-4o-mini": settings.default_model,
        "gpt-3.5-turbo": settings.default_model,
        "gpt-4.1": settings.default_model,
        "gpt-4.1-mini": settings.default_model,
        "gpt-5": settings.default_model,
        "o1": settings.default_model,
        "o3": settings.default_model,
        "o3-mini": settings.default_model,
        "o4-mini": settings.default_model,
        "claude-3-5-sonnet": settings.default_model,
        "claude-sonnet-4": settings.default_model,
        "claude-opus-4": settings.default_model,
        "grok-build": settings.default_model,
    }
    return aliases.get(m, m)


def _to_upstream_body(req: ChatCompletionRequest) -> dict[str, Any]:
    data = req.model_dump(exclude_none=True)
    data["model"] = _normalize_model(req.model)
    if "max_completion_tokens" in data and "max_tokens" not in data:
        data["max_tokens"] = data.pop("max_completion_tokens")
    if settings.normalize_content and isinstance(data.get("messages"), list):
        # normalize text parts only; keep tool_calls / tool messages intact
        data["messages"] = normalize_messages(data["messages"])
    # OpenAI clients sometimes send empty string content with tool_calls;
    # upstream accepts null better for some paths
    if isinstance(data.get("messages"), list):
        fixed = []
        for m in data["messages"]:
            if not isinstance(m, dict):
                fixed.append(m)
                continue
            msg = dict(m)
            if msg.get("tool_calls") and msg.get("content") == "":
                msg["content"] = None
            fixed.append(msg)
        data["messages"] = fixed
    return data


def _http_error_response(status: int, message: str, **kwargs: Any) -> JSONResponse:
    return JSONResponse(
        status_code=status,
        content=openai_error(message, status_code=status, **kwargs),
    )


@app.exception_handler(HTTPException)
async def http_exception_handler(_request: Request, exc: HTTPException) -> JSONResponse:
    detail = exc.detail
    if isinstance(detail, dict) and "message" in detail:
        return JSONResponse(status_code=exc.status_code, content={"error": detail})
    if isinstance(detail, dict) and "error" in detail:
        return JSONResponse(status_code=exc.status_code, content=detail)
    return _http_error_response(exc.status_code, str(detail))


@app.get("/health")
async def health() -> dict[str, Any]:
    st = auth_store.status()
    try:
        from .cli_pool import cli_pool

        cli_info = {
            "usable": cli_pool.count(enabled_only=True),
            "total": cli_pool.count(enabled_only=False),
            "accounts": cli_pool.list_public(),
        }
        cli_ok = cli_info["usable"] > 0 or st.get("ok", False)
    except Exception as e:
        cli_info = {"error": str(e)}
        cli_ok = st.get("ok", False)
    return {
        "ok": cli_ok,
        "version": __version__,
        "mode": "cli",
        "client_version": settings.resolved_client_version,
        "auth": st,
        "cli_pool": cli_info,
        "proxy_base_url": settings.proxy_base_url,
        "default_model": settings.default_model,
        "fold_reasoning": settings.fold_reasoning,
        "stream_fallback": settings.stream_fallback,
    }


@app.get("/v1/billing")
async def billing(_: None = Depends(require_local_key)) -> Any:
    try:
        return await upstream.billing()
    except Exception as e:
        raise HTTPException(status_code=502, detail=str(e)) from e


@app.get("/v1/models")
async def list_models(_: None = Depends(require_local_key)) -> Any:
    now = int(time.time())
    try:
        data = await upstream.list_models()
    except Exception as e:
        log.warning("models fetch failed: %s", e)
        return {
            "object": "list",
            "data": [
                {
                    "id": settings.default_model,
                    "object": "model",
                    "created": now,
                    "owned_by": "xai",
                },
                {
                    "id": "grok-composer-2.5-fast",
                    "object": "model",
                    "created": now,
                    "owned_by": "xai",
                },
            ],
        }

    if isinstance(data, dict) and "data" in data:
        items = []
        for m in data["data"]:
            mid = m.get("id") or m.get("model")
            items.append(
                {
                    "id": mid,
                    "object": "model",
                    "created": now,
                    "owned_by": m.get("owned_by") or "xai",
                    **{
                        k: v
                        for k, v in m.items()
                        if k not in {"id", "object", "created", "owned_by"}
                    },
                }
            )
        return {"object": "list", "data": items}
    return data


@app.post("/v1/chat/completions")
async def chat_completions(
    req: ChatCompletionRequest,
    _: None = Depends(require_local_key),
) -> Any:
    if not req.messages:
        raise HTTPException(status_code=400, detail="messages is required")
    body = _to_upstream_body(req)
    t0 = time.perf_counter()
    status = 200
    err: str | None = None
    try:
        result = await upstream.chat_completions(body)
        if req.stream:
            assert isinstance(result, AsyncIterator)

            async def gen() -> AsyncIterator[bytes]:
                async for chunk in transform_sse_bytes(
                    result, fold_reasoning=settings.fold_reasoning
                ):
                    yield chunk

            return StreamingResponse(
                gen(),
                media_type="text/event-stream",
                headers={
                    "Cache-Control": "no-cache",
                    "Connection": "keep-alive",
                    "X-Accel-Buffering": "no",
                },
            )
        assert isinstance(result, dict)
        payload = result
        if settings.fold_reasoning:
            payload = fold_reasoning_in_completion(payload)
        return JSONResponse(payload)
    except httpx.HTTPStatusError as e:
        status = e.response.status_code if e.response is not None else 502
        err = str(e)
        raise HTTPException(status_code=status, detail=err) from e
    except FileNotFoundError as e:
        status = 503
        err = str(e)
        raise HTTPException(status_code=503, detail=err) from e
    except Exception as e:
        status = 502
        err = str(e)
        raise HTTPException(status_code=502, detail=err) from e
    finally:
        usage_logger.log_request(
            mode="cli",
            model=str(body.get("model") or ""),
            stream=req.stream,
            status=status,
            latency_ms=(time.perf_counter() - t0) * 1000,
            error=err,
        )


@app.post("/v1/responses")
async def responses_api(
    req: ResponsesRequest,
    _: None = Depends(require_local_key),
) -> Any:
    body = req.model_dump(exclude_none=True)
    body["model"] = _normalize_model(req.model)
    try:
        result = await upstream.responses(body)
    except httpx.HTTPStatusError as e:
        status = e.response.status_code if e.response is not None else 502
        raise HTTPException(status_code=status, detail=str(e)) from e
    except Exception as e:
        raise HTTPException(status_code=502, detail=str(e)) from e

    if req.stream:
        assert isinstance(result, AsyncIterator)

        async def gen() -> AsyncIterator[bytes]:
            async for chunk in result:
                yield chunk

        return StreamingResponse(
            gen(),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
            },
        )

    assert isinstance(result, httpx.Response)
    try:
        payload = result.json()
    finally:
        await result.aclose()
    return JSONResponse(payload)


@app.post("/v1/messages")
async def anthropic_messages(
    request: Request,
    _: None = Depends(require_local_key),
) -> Any:
    from .anthropic_compat import (
        anthropic_to_openai_body,
        openai_to_anthropic_message,
    )

    payload = await request.json()
    oai = anthropic_to_openai_body(payload)
    oai["model"] = _normalize_model(oai.get("model"))
    if settings.normalize_content and isinstance(oai.get("messages"), list):
        oai["messages"] = normalize_messages(oai["messages"])
    stream = bool(oai.get("stream"))

    if stream:
        from .anthropic_compat import AnthropicStreamConverter

        result = await upstream.chat_completions(oai)
        assert isinstance(result, AsyncIterator)

        async def gen() -> AsyncIterator[bytes]:
            converter = AnthropicStreamConverter(str(oai["model"]))

            def encode_event(ev: dict[str, Any]) -> bytes:
                return (
                    f"event: {ev['type']}\ndata: "
                    f"{json.dumps(ev, ensure_ascii=False)}\n\n"
                ).encode()

            for ev in converter.prelude():
                yield encode_event(ev)
            async for raw in result:
                text = raw.decode("utf-8", "replace")
                for line in text.splitlines():
                    line = line.strip()
                    if not line.startswith("data:"):
                        continue
                    data_s = line[5:].strip()
                    if data_s == "[DONE]":
                        for ev in converter.finish():
                            yield encode_event(ev)
                        return
                    try:
                        chunk = json.loads(data_s)
                    except json.JSONDecodeError:
                        continue
                    for ev in converter.feed(chunk):
                        yield encode_event(ev)
            for ev in converter.finish():
                yield encode_event(ev)

        return StreamingResponse(
            gen(),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
        )

    result = await upstream.chat_completions(oai)
    assert isinstance(result, dict)
    return JSONResponse(openai_to_anthropic_message(result))


@app.post("/chat/completions")
async def chat_completions_alias(
    req: ChatCompletionRequest,
    dep: None = Depends(require_local_key),
) -> Any:
    return await chat_completions(req, dep)


@app.get("/")
async def root() -> dict[str, Any]:
    try:
        from .cli_pool import cli_pool

        usable = cli_pool.count(enabled_only=True)
    except Exception:
        usable = 0
    return {
        "name": "grok2api",
        "version": __version__,
        "mode": "cli",
        "client_version": settings.resolved_client_version,
        "panel": "/panel",
        "endpoints": [
            "GET /panel",
            "GET /health",
            "GET /v1/models",
            "GET /v1/billing",
            "POST /v1/chat/completions",
            "POST /v1/responses",
            "POST /v1/messages",
            "GET /admin/api/cli-accounts",
        ],
        "cli_pool_usable": usable,
        "note": "CLI OIDC only → cli-chat-proxy. Open /panel for UI.",
    }


@app.api_route("/{path:path}", methods=["GET", "POST", "PUT", "DELETE", "PATCH"])
async def catchall(path: str, request: Request) -> Any:
    raise HTTPException(
        status_code=404,
        detail=f"unknown path /{path}. Use /v1/chat/completions.",
    )


def main() -> None:
    import uvicorn

    uvicorn.run(
        "app.main:app",
        host=settings.host,
        port=settings.port,
        reload=False,
        log_level="info",
    )


if __name__ == "__main__":
    main()
