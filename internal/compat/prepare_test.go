package compat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestNormalizeResponsesRequestMapsChatShape(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":16,
		"stream":false,
		"tools":[{"type":"namespace","name":"demo","tools":[{"type":"function","name":"inner","parameters":{"type":"object"}}]}],
		"web_search_options":{"search_context_size":"medium"}
	}`)
	out, model, stream, err := compat.NormalizeResponsesRequest(body, "fallback")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if model != "grok-4.5" || stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["input"]; !ok {
		t.Fatalf("missing input: %#v", payload)
	}
	if _, ok := payload["messages"]; ok {
		t.Fatalf("messages should be removed: %#v", payload)
	}
	if payload["max_output_tokens"] != float64(16) {
		t.Fatalf("max_output_tokens=%#v", payload["max_output_tokens"])
	}
	if payload["backend_search"] != true {
		t.Fatalf("backend_search=%#v", payload["backend_search"])
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", payload["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "inner" {
		t.Fatalf("tool=%#v", tool)
	}
}

func TestFinalizeResponsesUpstreamForcesStreamAndStrips(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","stream":false,"input":[],"external_web_access":true,"metadata":{"x":1}}`)
	out, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{SupportsBackendSearch: true})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream=%#v", payload["stream"])
	}
	if payload["backend_search"] != true {
		t.Fatalf("backend_search=%#v", payload["backend_search"])
	}
	if _, ok := payload["external_web_access"]; ok {
		t.Fatalf("external_web_access should be stripped: %#v", payload)
	}
	if _, ok := payload["metadata"]; ok {
		t.Fatalf("metadata should be stripped: %#v", payload)
	}
}

func TestExtractCompletedResponseFromSSE(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_9\",\"output_text\":\"done\"}}\n\n"
	out := compat.ExtractCompletedResponse([]byte(sse))
	if !strings.Contains(string(out), `"id":"resp_9"`) {
		t.Fatalf("out=%s", string(out))
	}
}

func TestPrepareResponsesFromAnthropic(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	out, model, stream, err := compat.PrepareResponsesFromAnthropic(body, "fallback")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if model != "grok-4.5" || stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["input"]; !ok {
		t.Fatalf("missing input: %#v", payload)
	}
	if payload["max_output_tokens"] != float64(32) {
		t.Fatalf("max_output_tokens=%#v", payload["max_output_tokens"])
	}
}
