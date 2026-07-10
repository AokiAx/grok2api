package compat_test

import (
	"encoding/json"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestDetectPayloadResponsesOnWrongPath(t *testing.T) {
	// Codex-style body often lands on /v1/messages via gateways.
	body := []byte(`{
		"model":"grok-4.5",
		"stream":true,
		"instructions":"You are Codex",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}],
		"tool_choice":"auto",
		"parallel_tool_calls":false,
		"store":false
	}`)
	if kind := compat.DetectPayload(body); kind != compat.KindResponses {
		t.Fatalf("kind=%v want Responses", kind)
	}
}

func TestDetectPayloadChat(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if kind := compat.DetectPayload(body); kind != compat.KindChat {
		t.Fatalf("kind=%v want Chat", kind)
	}
}

func TestDetectPayloadAnthropic(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"tool_result","tool_use_id":"1","content":"ok"}]}]
	}`)
	if kind := compat.DetectPayload(body); kind != compat.KindAnthropic {
		t.Fatalf("kind=%v want Anthropic", kind)
	}
}

func TestNormalizeResponsesRequestKeepsCodexInputText(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"stream":true,
		"instructions":"You are Codex",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hello codex"}]}],
		"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}}]
	}`)
	out, _, _, err := compat.NormalizeResponsesRequest(body, "fallback")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	input, _ := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input should be preserved, got %#v", payload["input"])
	}
	item := input[0].(map[string]any)
	content, _ := item["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("content wiped: %#v", item)
	}
	part := content[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hello codex" {
		t.Fatalf("content part=%#v", part)
	}
}
