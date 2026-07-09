from __future__ import annotations

from app.anthropic_compat import (
    anthropic_to_openai_body,
    anthropic_tools_to_openai,
    openai_to_anthropic_message,
)
from app.compat import normalize_messages


def test_anthropic_to_openai():
    body = anthropic_to_openai_body(
        {
            "model": "claude-sonnet-4",
            "system": "be brief",
            "messages": [{"role": "user", "content": "hi"}],
            "max_tokens": 64,
            "stream": False,
        }
    )
    assert body["messages"][0]["role"] == "system"
    assert body["messages"][1]["content"] == "hi"
    assert body["max_tokens"] == 64


def test_anthropic_tools_forwarded():
    body = anthropic_to_openai_body(
        {
            "model": "grok-4.5",
            "messages": [
                {"role": "user", "content": "weather in Tokyo? use tool"},
            ],
            "max_tokens": 64,
            "tools": [
                {
                    "name": "get_weather",
                    "description": "Get weather",
                    "input_schema": {
                        "type": "object",
                        "properties": {"city": {"type": "string"}},
                        "required": ["city"],
                    },
                }
            ],
            "tool_choice": {"type": "any"},
        }
    )
    assert body["tools"][0]["type"] == "function"
    assert body["tools"][0]["function"]["name"] == "get_weather"
    assert body["tool_choice"] == "required"


def test_anthropic_tool_result_roundtrip_messages():
    body = anthropic_to_openai_body(
        {
            "model": "grok-4.5",
            "messages": [
                {"role": "user", "content": "weather?"},
                {
                    "role": "assistant",
                    "content": [
                        {
                            "type": "tool_use",
                            "id": "toolu_1",
                            "name": "get_weather",
                            "input": {"city": "Tokyo"},
                        }
                    ],
                },
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "tool_result",
                            "tool_use_id": "toolu_1",
                            "content": '{"temp":28}',
                        }
                    ],
                },
            ],
            "tools": [
                {
                    "name": "get_weather",
                    "input_schema": {"type": "object", "properties": {}},
                }
            ],
        }
    )
    roles = [m["role"] for m in body["messages"]]
    assert roles == ["user", "assistant", "tool"]
    assert body["messages"][1]["tool_calls"][0]["function"]["name"] == "get_weather"
    assert body["messages"][2]["tool_call_id"] == "toolu_1"


def test_openai_to_anthropic():
    msg = openai_to_anthropic_message(
        {
            "model": "grok-4.5",
            "choices": [
                {
                    "message": {"role": "assistant", "content": "pong"},
                    "finish_reason": "stop",
                }
            ],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1},
        }
    )
    assert msg["type"] == "message"
    assert msg["content"][0]["text"] == "pong"


def test_openai_tool_calls_to_anthropic_tool_use():
    msg = openai_to_anthropic_message(
        {
            "model": "grok-4.5",
            "choices": [
                {
                    "message": {
                        "role": "assistant",
                        "content": "",
                        "tool_calls": [
                            {
                                "id": "call-1",
                                "type": "function",
                                "function": {
                                    "name": "get_weather",
                                    "arguments": '{"city":"Tokyo"}',
                                },
                            }
                        ],
                    },
                    "finish_reason": "tool_calls",
                }
            ],
            "usage": {"prompt_tokens": 10, "completion_tokens": 5},
        }
    )
    assert msg["stop_reason"] == "tool_use"
    blocks = msg["content"]
    assert any(b.get("type") == "tool_use" for b in blocks)
    tu = next(b for b in blocks if b["type"] == "tool_use")
    assert tu["name"] == "get_weather"
    assert tu["input"]["city"] == "Tokyo"


def test_normalize_messages():
    out = normalize_messages(
        [
            {"role": "user", "content": [{"type": "text", "text": "hello"}]},
        ]
    )
    assert out[0]["content"] == "hello"


def test_anthropic_tools_to_openai_helper():
    tools = anthropic_tools_to_openai(
        [{"name": "x", "description": "d", "input_schema": {"type": "object"}}]
    )
    assert tools[0]["function"]["name"] == "x"
