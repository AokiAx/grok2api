package compat_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestResponsesToChatStreamConvertsDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","output_text":"hi"}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToChatStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = stream.Close()
	body := string(data)
	if !strings.Contains(body, `"content":"hi"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body=%s", body)
	}
}

func TestAggregateResponsesStreamAcceptsBareJSONBody(t *testing.T) {
	raw := `{"id":"resp_json","model":"grok-4.5","object":"response","status":"completed","output_text":"PONG","usage":{"input_tokens":2,"output_tokens":1}}`
	out, err := compat.AggregateResponsesStream(io.NopCloser(strings.NewReader(raw)), "grok-4.5")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	content := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	if content != "PONG" {
		t.Fatalf("content=%#v body=%s", content, string(out))
	}
}

func TestSetJSONStreamFlagForcesTrue(t *testing.T) {
	out, err := compat.SetJSONStreamFlag([]byte(`{"model":"grok-4.5","stream":false,"input":[]}`), true)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream=%#v", payload["stream"])
	}
}
