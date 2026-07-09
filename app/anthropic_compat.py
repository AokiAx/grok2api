"""Minimal Anthropic Messages API → OpenAI chat.completions adapter."""

from __future__ import annotations

import time
import uuid
from typing import Any


def anthropic_to_openai_body(payload: dict[str, Any]) -> dict[str, Any]:
    model = payload.get("model")
    messages: list[dict[str, Any]] = []
    system = payload.get("system")
    if isinstance(system, str) and system.strip():
        messages.append({"role": "system", "content": system})
    elif isinstance(system, list):
        texts = []
        for part in system:
            if isinstance(part, dict) and part.get("type") == "text":
                texts.append(str(part.get("text") or ""))
            elif isinstance(part, str):
                texts.append(part)
        if texts:
            messages.append({"role": "system", "content": "\n".join(texts)})

    for m in payload.get("messages") or []:
        if not isinstance(m, dict):
            continue
        role = m.get("role") or "user"
        content = m.get("content")
        if isinstance(content, list):
            # keep structure; OpenAI path will normalize
            messages.append({"role": role, "content": content})
        else:
            messages.append({"role": role, "content": content})

    out: dict[str, Any] = {
        "model": model,
        "messages": messages,
        "stream": bool(payload.get("stream")),
    }
    if payload.get("max_tokens") is not None:
        out["max_tokens"] = payload["max_tokens"]
    if payload.get("temperature") is not None:
        out["temperature"] = payload["temperature"]
    if payload.get("top_p") is not None:
        out["top_p"] = payload["top_p"]
    if payload.get("stop_sequences"):
        out["stop"] = payload["stop_sequences"]
    return out


def openai_to_anthropic_message(completion: dict[str, Any]) -> dict[str, Any]:
    model = completion.get("model") or "grok"
    choices = completion.get("choices") or []
    content = ""
    stop_reason = "end_turn"
    if choices:
        msg = choices[0].get("message") or {}
        content = msg.get("content") or ""
        fr = choices[0].get("finish_reason")
        if fr == "length":
            stop_reason = "max_tokens"
        elif fr == "tool_calls":
            stop_reason = "tool_use"
    usage = completion.get("usage") or {}
    return {
        "id": f"msg_{uuid.uuid4().hex[:24]}",
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": [{"type": "text", "text": content}],
        "stop_reason": stop_reason,
        "stop_sequence": None,
        "usage": {
            "input_tokens": usage.get("prompt_tokens") or 0,
            "output_tokens": usage.get("completion_tokens") or 0,
        },
    }


def openai_sse_to_anthropic_events(chunk: dict[str, Any]) -> list[dict[str, Any]]:
    """Convert one OpenAI chat chunk into Anthropic stream events (simplified)."""
    events: list[dict[str, Any]] = []
    choices = chunk.get("choices") or []
    if not choices:
        return events
    delta = choices[0].get("delta") or {}
    text = delta.get("content")
    if text:
        events.append(
            {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "text_delta", "text": text},
            }
        )
    if choices[0].get("finish_reason"):
        events.append(
            {
                "type": "message_delta",
                "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                "usage": {"output_tokens": 0},
            }
        )
        events.append({"type": "message_stop"})
    return events


def anthropic_stream_prelude(model: str) -> list[dict[str, Any]]:
    msg_id = f"msg_{uuid.uuid4().hex[:24]}"
    return [
        {
            "type": "message_start",
            "message": {
                "id": msg_id,
                "type": "message",
                "role": "assistant",
                "model": model,
                "content": [],
                "stop_reason": None,
                "stop_sequence": None,
                "usage": {"input_tokens": 0, "output_tokens": 0},
            },
        },
        {
            "type": "content_block_start",
            "index": 0,
            "content_block": {"type": "text", "text": ""},
        },
        {"type": "ping"},
    ]
