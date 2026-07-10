"""Anthropic Messages API ↔ OpenAI chat.completions adapter (incl. tools)."""

from __future__ import annotations

import json
import uuid
from typing import Any


def _text_from_parts(parts: list[Any]) -> str:
    texts: list[str] = []
    for part in parts:
        if isinstance(part, str):
            texts.append(part)
        elif isinstance(part, dict):
            if part.get("type") in (None, "text"):
                texts.append(str(part.get("text") or ""))
            elif part.get("type") == "tool_result":
                # nested rare; stringify content
                c = part.get("content")
                if isinstance(c, str):
                    texts.append(c)
                elif c is not None:
                    texts.append(json.dumps(c, ensure_ascii=False))
    return "".join(texts)


def _anthropic_content_to_openai_messages(
    role: str, content: Any
) -> list[dict[str, Any]]:
    """Expand one Anthropic message into one or more OpenAI messages.

    Handles tool_use / tool_result content blocks.
    """
    if not isinstance(content, list):
        return [{"role": role, "content": content}]

    # Split tool_result blocks into role=tool messages; tool_use into assistant.tool_calls
    text_parts: list[str] = []
    tool_calls: list[dict[str, Any]] = []
    out: list[dict[str, Any]] = []

    def flush_assistant() -> None:
        nonlocal text_parts, tool_calls
        if not text_parts and not tool_calls:
            return
        msg: dict[str, Any] = {
            "role": "assistant",
            "content": "".join(text_parts) if text_parts else None,
        }
        if tool_calls:
            msg["tool_calls"] = tool_calls
            if msg["content"] == "":
                msg["content"] = None
        out.append(msg)
        text_parts = []
        tool_calls = []

    for part in content:
        if not isinstance(part, dict):
            if isinstance(part, str):
                text_parts.append(part)
            continue
        ptype = part.get("type")
        if ptype in (None, "text"):
            text_parts.append(str(part.get("text") or ""))
        elif ptype == "tool_use":
            tool_calls.append(
                {
                    "id": str(part.get("id") or f"call_{uuid.uuid4().hex[:12]}"),
                    "type": "function",
                    "function": {
                        "name": str(part.get("name") or ""),
                        "arguments": json.dumps(
                            part.get("input") if part.get("input") is not None else {},
                            ensure_ascii=False,
                        ),
                    },
                }
            )
        elif ptype == "tool_result":
            # tool results are user-turn blocks in Anthropic; map to OpenAI tool msgs
            flush_assistant()
            c = part.get("content")
            if isinstance(c, list):
                c = _text_from_parts(c)
            elif c is not None and not isinstance(c, str):
                c = json.dumps(c, ensure_ascii=False)
            out.append(
                {
                    "role": "tool",
                    "tool_call_id": str(part.get("tool_use_id") or part.get("id") or ""),
                    "content": c if c is not None else "",
                }
            )
        else:
            # image etc — keep as multimodal later; stringify for now
            text_parts.append(json.dumps(part, ensure_ascii=False))

    if role == "assistant":
        flush_assistant()
    elif role == "user":
        # pure text user (tool_results already flushed to out)
        if text_parts:
            out.append({"role": "user", "content": "".join(text_parts)})
        elif not out:
            out.append({"role": "user", "content": ""})
    else:
        flush_assistant()
        if not out:
            out.append({"role": role, "content": _text_from_parts(content)})

    return out


def anthropic_tools_to_openai(tools: Any) -> list[dict[str, Any]] | None:
    if not isinstance(tools, list) or not tools:
        return None
    out: list[dict[str, Any]] = []
    for t in tools:
        if not isinstance(t, dict):
            continue
        # already OpenAI-shaped
        if t.get("type") == "function" and isinstance(t.get("function"), dict):
            out.append(t)
            continue
        name = t.get("name")
        if not name:
            continue
        params = t.get("input_schema") or t.get("parameters") or {
            "type": "object",
            "properties": {},
        }
        out.append(
            {
                "type": "function",
                "function": {
                    "name": str(name),
                    "description": str(t.get("description") or ""),
                    "parameters": params,
                },
            }
        )
    return out or None


def anthropic_tool_choice_to_openai(tool_choice: Any) -> Any:
    if tool_choice is None:
        return None
    if isinstance(tool_choice, str):
        # auto | any | none | tool
        if tool_choice == "any":
            return "required"
        if tool_choice in ("auto", "none", "required"):
            return tool_choice
        return tool_choice
    if isinstance(tool_choice, dict):
        # {"type":"tool","name":"foo"} or {"type":"auto"}
        t = tool_choice.get("type")
        if t in ("auto", "none"):
            return t
        if t == "any":
            return "required"
        if t == "tool" and tool_choice.get("name"):
            return {
                "type": "function",
                "function": {"name": str(tool_choice["name"])},
            }
        if t == "function":
            return tool_choice
    return tool_choice


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
        role = str(m.get("role") or "user")
        content = m.get("content")
        messages.extend(_anthropic_content_to_openai_messages(role, content))

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

    tools = anthropic_tools_to_openai(payload.get("tools"))
    if tools:
        out["tools"] = tools
    tc = anthropic_tool_choice_to_openai(payload.get("tool_choice"))
    if tc is not None:
        out["tool_choice"] = tc

    return out


def openai_to_anthropic_message(completion: dict[str, Any]) -> dict[str, Any]:
    model = completion.get("model") or "grok"
    choices = completion.get("choices") or []
    content_blocks: list[dict[str, Any]] = []
    stop_reason = "end_turn"
    if choices:
        msg = choices[0].get("message") or {}
        text = msg.get("content")
        if text:
            content_blocks.append({"type": "text", "text": str(text)})
        for tc in msg.get("tool_calls") or []:
            if not isinstance(tc, dict):
                continue
            fn = tc.get("function") or {}
            raw_args = fn.get("arguments") or "{}"
            try:
                inp = json.loads(raw_args) if isinstance(raw_args, str) else raw_args
            except json.JSONDecodeError:
                inp = {"_raw": raw_args}
            if not isinstance(inp, dict):
                inp = {"value": inp}
            content_blocks.append(
                {
                    "type": "tool_use",
                    "id": str(tc.get("id") or f"toolu_{uuid.uuid4().hex[:12]}"),
                    "name": str(fn.get("name") or ""),
                    "input": inp,
                }
            )
        fr = choices[0].get("finish_reason")
        if fr == "length":
            stop_reason = "max_tokens"
        elif fr == "tool_calls":
            stop_reason = "tool_use"
        elif fr == "stop":
            stop_reason = "end_turn"

    if not content_blocks:
        content_blocks = [{"type": "text", "text": ""}]

    usage = completion.get("usage") or {}
    return {
        "id": f"msg_{uuid.uuid4().hex[:24]}",
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": content_blocks,
        "stop_reason": stop_reason,
        "stop_sequence": None,
        "usage": {
            "input_tokens": usage.get("prompt_tokens") or 0,
            "output_tokens": usage.get("completion_tokens") or 0,
        },
    }


def openai_sse_to_anthropic_events(chunk: dict[str, Any]) -> list[dict[str, Any]]:
    """Convert one OpenAI chat chunk into Anthropic stream events.

    Supports text deltas and tool_calls → tool_use blocks.
    Caller must emit message_start / content_block_start prelude.
    """
    events: list[dict[str, Any]] = []
    choices = chunk.get("choices") or []
    if not choices:
        return events
    ch0 = choices[0]
    delta = ch0.get("delta") or {}

    text = delta.get("content")
    if text:
        events.append(
            {
                "type": "content_block_delta",
                "index": 0,
                "delta": {"type": "text_delta", "text": text},
            }
        )

    for tc in delta.get("tool_calls") or []:
        if not isinstance(tc, dict):
            continue
        # OpenAI streams tool calls with index; map index+1 if text block at 0
        idx = int(tc.get("index") or 0) + 1  # leave 0 for text block
        fn = tc.get("function") or {}
        # first chunk often has id+name; subsequent only arguments
        if tc.get("id") or fn.get("name"):
            events.append(
                {
                    "type": "content_block_start",
                    "index": idx,
                    "content_block": {
                        "type": "tool_use",
                        "id": str(tc.get("id") or f"toolu_{idx}"),
                        "name": str(fn.get("name") or ""),
                        "input": {},
                    },
                }
            )
        if fn.get("arguments"):
            events.append(
                {
                    "type": "content_block_delta",
                    "index": idx,
                    "delta": {
                        "type": "input_json_delta",
                        "partial_json": str(fn["arguments"]),
                    },
                }
            )

    fr = ch0.get("finish_reason")
    if fr:
        stop = "end_turn"
        if fr == "tool_calls":
            stop = "tool_use"
        elif fr == "length":
            stop = "max_tokens"
        # close open blocks roughly
        events.append(
            {
                "type": "message_delta",
                "delta": {"stop_reason": stop, "stop_sequence": None},
                "usage": {"output_tokens": 0},
            }
        )
        events.append({"type": "message_stop"})
    return events


class AnthropicStreamConverter:
    """Stateful OpenAI SSE to Anthropic event lifecycle converter."""

    def __init__(self, model: str) -> None:
        self.model = model
        self._tool_blocks: set[int] = set()
        self._blocks_closed = False
        self._message_stopped = False

    def prelude(self) -> list[dict[str, Any]]:
        return anthropic_stream_prelude(self.model)

    def feed(self, chunk: dict[str, Any]) -> list[dict[str, Any]]:
        events: list[dict[str, Any]] = []
        choices = chunk.get("choices") or []
        if not choices:
            return events
        choice = choices[0]
        delta = choice.get("delta") or {}
        text = delta.get("content")
        if text:
            events.append(
                {
                    "type": "content_block_delta",
                    "index": 0,
                    "delta": {"type": "text_delta", "text": text},
                }
            )
        for tool_call in delta.get("tool_calls") or []:
            if not isinstance(tool_call, dict):
                continue
            index = int(tool_call.get("index") or 0) + 1
            function = tool_call.get("function") or {}
            if index not in self._tool_blocks:
                self._tool_blocks.add(index)
                events.append(
                    {
                        "type": "content_block_start",
                        "index": index,
                        "content_block": {
                            "type": "tool_use",
                            "id": str(tool_call.get("id") or f"toolu_{index}"),
                            "name": str(function.get("name") or ""),
                            "input": {},
                        },
                    }
                )
            if function.get("arguments"):
                events.append(
                    {
                        "type": "content_block_delta",
                        "index": index,
                        "delta": {
                            "type": "input_json_delta",
                            "partial_json": str(function["arguments"]),
                        },
                    }
                )
        finish_reason = choice.get("finish_reason")
        if finish_reason:
            events.extend(self._close_blocks(finish_reason))
        return events

    def _close_blocks(self, finish_reason: str = "stop") -> list[dict[str, Any]]:
        if self._blocks_closed:
            return []
        self._blocks_closed = True
        events = [{"type": "content_block_stop", "index": 0}]
        events.extend(
            {"type": "content_block_stop", "index": index}
            for index in sorted(self._tool_blocks)
        )
        stop_reason = "end_turn"
        if finish_reason == "tool_calls":
            stop_reason = "tool_use"
        elif finish_reason == "length":
            stop_reason = "max_tokens"
        events.append(
            {
                "type": "message_delta",
                "delta": {"stop_reason": stop_reason, "stop_sequence": None},
                "usage": {"output_tokens": 0},
            }
        )
        return events

    def finish(self) -> list[dict[str, Any]]:
        events = self._close_blocks()
        if not self._message_stopped:
            self._message_stopped = True
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
