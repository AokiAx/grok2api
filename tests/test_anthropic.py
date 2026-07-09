from __future__ import annotations

from app.anthropic_compat import (
    anthropic_to_openai_body,
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


def test_normalize_messages():
    out = normalize_messages(
        [
            {"role": "user", "content": [{"type": "text", "text": "hello"}]},
        ]
    )
    assert out[0]["content"] == "hello"
