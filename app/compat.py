"""OpenAI client compatibility helpers for the Grok CLI proxy."""

from __future__ import annotations

import json
import time
import uuid
from typing import Any, AsyncIterator


def normalize_content(content: Any) -> Any:
    """Flatten multimodal content arrays to a plain string when possible.

    OpenAI clients often send content as a list of parts
    (``[{"type":"text","text":"..."}]``). Upstream accepts that, but some
    older clusters prefer a plain string — and logging is cleaner.
    """
    if content is None or isinstance(content, str):
        return content
    if isinstance(content, list):
        texts: list[str] = []
        non_text = False
        for part in content:
            if isinstance(part, str):
                texts.append(part)
                continue
            if not isinstance(part, dict):
                non_text = True
                break
            ptype = part.get("type")
            if ptype in (None, "text", "input_text", "output_text"):
                t = part.get("text")
                if t is None and isinstance(part.get("content"), str):
                    t = part["content"]
                if t is not None:
                    texts.append(str(t))
                continue
            # image_url / input_image / etc — keep original structure
            non_text = True
            break
        if not non_text:
            return "".join(texts)
    return content


def normalize_messages(messages: list[Any]) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for m in messages:
        if not isinstance(m, dict):
            continue
        msg = dict(m)
        if "content" in msg:
            msg["content"] = normalize_content(msg["content"])
        out.append(msg)
    return out


def openai_error(
    message: str,
    *,
    status_code: int = 500,
    err_type: str | None = None,
    code: str | None = None,
    param: str | None = None,
) -> dict[str, Any]:
    """Shape errors like the OpenAI API so SDKs parse them cleanly."""
    if err_type is None:
        if status_code == 401:
            err_type = "invalid_request_error"
        elif status_code == 403:
            err_type = "permission_error"
        elif status_code == 404:
            err_type = "invalid_request_error"
        elif status_code == 429:
            err_type = "rate_limit_error"
        elif status_code >= 500:
            err_type = "api_error"
        else:
            err_type = "invalid_request_error"
    return {
        "error": {
            "message": message,
            "type": err_type,
            "param": param,
            "code": code or str(status_code),
        }
    }


def fold_reasoning_into_message(message: dict[str, Any]) -> dict[str, Any]:
    """Merge ``reasoning_content`` into ``content`` for clients that ignore it."""
    msg = dict(message)
    reasoning = msg.get("reasoning_content")
    content = msg.get("content")
    if not reasoning:
        return msg
    if content:
        msg["content"] = f"<think>\n{reasoning}\n</think>\n{content}"
    else:
        msg["content"] = str(reasoning)
    # keep original field; some UIs still want it
    return msg


def fold_reasoning_in_completion(payload: dict[str, Any]) -> dict[str, Any]:
    data = dict(payload)
    choices = data.get("choices")
    if not isinstance(choices, list):
        return data
    new_choices = []
    for ch in choices:
        if not isinstance(ch, dict):
            new_choices.append(ch)
            continue
        c = dict(ch)
        if isinstance(c.get("message"), dict):
            c["message"] = fold_reasoning_into_message(c["message"])
        if isinstance(c.get("delta"), dict):
            c["delta"] = fold_reasoning_into_message(c["delta"])
        new_choices.append(c)
    data["choices"] = new_choices
    return data


async def aggregate_sse_to_completion(
    chunks: AsyncIterator[bytes],
    *,
    model: str,
) -> dict[str, Any]:
    """Collect a streaming chat.completion SSE into one chat.completion object.

    Used when the client wants ``stream=false`` but upstream only streams,
    or as a forced path for stream-only models.
    """
    content_parts: list[str] = []
    reasoning_parts: list[str] = []
    role = "assistant"
    finish_reason: str | None = None
    usage: dict[str, Any] | None = None
    completion_id = f"chatcmpl-{uuid.uuid4().hex[:24]}"
    created = int(time.time())
    response_model = model
    tool_calls: dict[int, dict[str, Any]] = {}
    buffer = ""

    async for raw in chunks:
        if not raw:
            continue
        try:
            text = raw.decode("utf-8", "replace")
        except Exception:
            continue
        buffer += text
        while "\n" in buffer:
            line, buffer = buffer.split("\n", 1)
            line = line.strip()
            if not line or line.startswith(":"):
                continue
            if not line.startswith("data:"):
                continue
            data_s = line[5:].strip()
            if data_s == "[DONE]":
                buffer = ""
                break
            try:
                event = json.loads(data_s)
            except json.JSONDecodeError:
                continue
            if not isinstance(event, dict):
                continue
            if event.get("id"):
                completion_id = str(event["id"])
            if event.get("model"):
                response_model = str(event["model"])
            if event.get("created"):
                try:
                    created = int(event["created"])
                except (TypeError, ValueError):
                    pass
            if isinstance(event.get("usage"), dict):
                usage = event["usage"]
            for ch in event.get("choices") or []:
                if not isinstance(ch, dict):
                    continue
                if ch.get("finish_reason"):
                    finish_reason = ch["finish_reason"]
                delta = ch.get("delta") or ch.get("message") or {}
                if not isinstance(delta, dict):
                    continue
                if delta.get("role"):
                    role = str(delta["role"])
                if delta.get("content"):
                    content_parts.append(str(delta["content"]))
                if delta.get("reasoning_content"):
                    reasoning_parts.append(str(delta["reasoning_content"]))
                # tool_calls streaming merge
                for tc in delta.get("tool_calls") or []:
                    if not isinstance(tc, dict):
                        continue
                    idx = int(tc.get("index") or 0)
                    slot = tool_calls.setdefault(
                        idx,
                        {
                            "id": tc.get("id") or f"call_{idx}",
                            "type": tc.get("type") or "function",
                            "function": {"name": "", "arguments": ""},
                        },
                    )
                    if tc.get("id"):
                        slot["id"] = tc["id"]
                    if tc.get("type"):
                        slot["type"] = tc["type"]
                    fn = tc.get("function") or {}
                    if isinstance(fn, dict):
                        if fn.get("name"):
                            slot["function"]["name"] = (
                                slot["function"].get("name") or ""
                            ) + str(fn["name"])
                        if fn.get("arguments"):
                            slot["function"]["arguments"] = (
                                slot["function"].get("arguments") or ""
                            ) + str(fn["arguments"])

    message: dict[str, Any] = {
        "role": role,
        "content": "".join(content_parts) or None,
    }
    if reasoning_parts:
        message["reasoning_content"] = "".join(reasoning_parts)
    if tool_calls:
        message["tool_calls"] = [tool_calls[i] for i in sorted(tool_calls)]
        if message["content"] is None:
            message["content"] = None

    out: dict[str, Any] = {
        "id": completion_id,
        "object": "chat.completion",
        "created": created,
        "model": response_model,
        "choices": [
            {
                "index": 0,
                "message": message,
                "finish_reason": finish_reason or "stop",
            }
        ],
    }
    if usage:
        out["usage"] = usage
    return out


async def transform_sse_bytes(
    chunks: AsyncIterator[bytes],
    *,
    fold_reasoning: bool = False,
) -> AsyncIterator[bytes]:
    """Optionally rewrite SSE deltas (e.g. fold reasoning into content)."""
    if not fold_reasoning:
        async for chunk in chunks:
            yield chunk
        return

    buffer = ""
    async for raw in chunks:
        try:
            text = raw.decode("utf-8", "replace")
        except Exception:
            yield raw
            continue
        buffer += text
        while "\n" in buffer:
            line, buffer = buffer.split("\n", 1)
            # preserve blank lines that separate SSE events
            if line == "" or line == "\r":
                yield b"\n"
                continue
            stripped = line.rstrip("\r")
            if not stripped.startswith("data:"):
                yield (stripped + "\n").encode("utf-8")
                continue
            data_s = stripped[5:].strip()
            if data_s == "[DONE]":
                yield b"data: [DONE]\n"
                continue
            try:
                event = json.loads(data_s)
            except json.JSONDecodeError:
                yield (stripped + "\n").encode("utf-8")
                continue
            if isinstance(event, dict):
                event = fold_reasoning_in_completion(event)
            out_line = "data: " + json.dumps(event, ensure_ascii=False) + "\n"
            yield out_line.encode("utf-8")
    if buffer:
        yield buffer.encode("utf-8")
