package compat_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestChatToResponsesMapsMessagesAndReasoning(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"reasoning":{"effort":"HIGH"},"stream":false}`)
	out, stream, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if stream {
		t.Fatal("expected non-stream")
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["input"]; !ok {
		t.Fatalf("missing input: %#v", payload)
	}
	if payload["max_output_tokens"] != float64(64) {
		t.Fatalf("max_output_tokens = %#v", payload["max_output_tokens"])
	}
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v", payload["reasoning_effort"])
	}
}

func TestResponsesToChatExtractsOutputText(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"grok-4.5","status":"completed","output_text":"hello","usage":{"input_tokens":3,"output_tokens":2}}`)
	out, err := compat.ResponsesToChat(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choices := payload["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("content = %#v", message)
	}
	usage := payload["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(3) || usage["completion_tokens"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAggregateResponsesStreamBuildsChatCompletion(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hel"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_9","model":"grok-4.5","status":"completed","output_text":"hello","usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
	}, "\n")
	out, err := compat.AggregateResponsesStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	content := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	if content != "hello" {
		t.Fatalf("content = %#v payload=%s", content, string(out))
	}
}
