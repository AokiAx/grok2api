from __future__ import annotations

import json

import pytest

from app.compat import (
    aggregate_sse_to_completion,
    fold_reasoning_in_completion,
    normalize_content,
    normalize_messages,
    openai_error,
)


def test_normalize_content_string():
    assert normalize_content("hi") == "hi"
    assert normalize_content(None) is None


def test_normalize_content_parts():
    parts = [
        {"type": "text", "text": "Hello "},
        {"type": "text", "text": "world"},
    ]
    assert normalize_content(parts) == "Hello world"


def test_normalize_content_keeps_images():
    parts = [
        {"type": "text", "text": "see"},
        {"type": "image_url", "image_url": {"url": "http://x/a.png"}},
    ]
    assert normalize_content(parts) == parts


def test_normalize_messages():
    msgs = [
        {"role": "user", "content": [{"type": "text", "text": "ping"}]},
        {"role": "assistant", "content": "pong"},
    ]
    out = normalize_messages(msgs)
    assert out[0]["content"] == "ping"
    assert out[1]["content"] == "pong"


def test_openai_error_shape():
    err = openai_error("nope", status_code=401, code="invalid_api_key")
    assert err["error"]["message"] == "nope"
    assert err["error"]["type"] == "invalid_request_error"
    assert err["error"]["code"] == "invalid_api_key"


def test_fold_reasoning():
    payload = {
        "choices": [
            {
                "message": {
                    "role": "assistant",
                    "content": "answer",
                    "reasoning_content": "think hard",
                }
            }
        ]
    }
    folded = fold_reasoning_in_completion(payload)
    content = folded["choices"][0]["message"]["content"]
    assert "think hard" in content
    assert "answer" in content
    assert "<think>" in content


@pytest.mark.asyncio
async def test_aggregate_sse():
    async def gen():
        events = [
            {
                "id": "cmpl-1",
                "model": "grok-4.5",
                "choices": [
                    {
                        "delta": {
                            "role": "assistant",
                            "reasoning_content": "hmm ",
                        }
                    }
                ],
            },
            {
                "choices": [{"delta": {"content": "hel"}}],
            },
            {
                "choices": [{"delta": {"content": "lo"}, "finish_reason": "stop"}],
                "usage": {"total_tokens": 3},
            },
        ]
        for e in events:
            yield f"data: {json.dumps(e)}\n\n".encode()
        yield b"data: [DONE]\n\n"

    out = await aggregate_sse_to_completion(gen(), model="grok-4.5")
    assert out["object"] == "chat.completion"
    assert out["choices"][0]["message"]["content"] == "hello"
    assert out["choices"][0]["message"]["reasoning_content"] == "hmm "
    assert out["choices"][0]["finish_reason"] == "stop"
    assert out["usage"]["total_tokens"] == 3
