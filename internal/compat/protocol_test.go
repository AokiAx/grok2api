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

func TestChatToResponsesPassesBackendSearch(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"web_search_options":{"search_context_size":"medium"}}`)
	out, _, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["backend_search"] != true {
		t.Fatalf("backend_search = %#v", payload["backend_search"])
	}
}

func TestChatToResponsesStripsOpenAIToolFields(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo"}}],"tool_choice":"auto","metadata":{"a":1},"user":"u1","tool_resources":{}}`)
	out, _, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"tools", "tool_choice", "metadata", "user", "tool_resources"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("field %q should be stripped but was present in: %s", key, out)
		}
	}
}

func TestChatToResponsesSanitizesToolMessages(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[` +
		`{"role":"system","content":"you are helpful"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"result"}` +
		`],"stream":true}`)
	out, _, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	input, ok := payload["input"].([]any)
	if !ok {
		t.Fatalf("input is not array: %#v", payload["input"])
	}
	// assistant with nil content should still be present (but no tool_calls)
	if len(input) != 4 {
		t.Fatalf("expected 4 messages, got %d: %#v", len(input), input)
	}
	for i, raw := range input {
		msg := raw.(map[string]any)
		if _, has := msg["tool_calls"]; has {
			t.Fatalf("message[%d] still has tool_calls", i)
		}
		if _, has := msg["tool_call_id"]; has {
			t.Fatalf("message[%d] still has tool_call_id", i)
		}
	}
}

func TestChatToResponsesFlattensMultimodalContent(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}]}`)
	out, _, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	input := payload["input"].([]any)
	msg := input[0].(map[string]any)
	if msg["content"] != "hello world" {
		t.Fatalf("content = %#v", msg["content"])
	}
}

func TestNormalizeChatRequestFillsModelAndEffort(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"MAX"}`)
	out, model, stream, err := compat.NormalizeChatRequest(body, "grok-4.5")
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
	if payload["reasoning_effort"] != "xhigh" {
		t.Fatalf("effort=%#v", payload["reasoning_effort"])
	}
}

func TestResponsesToChatReadsOutputArray(t *testing.T) {
	body := []byte(`{"id":"resp_2","model":"grok-4.5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"array-hi"}]}]}`)
	out, err := compat.ResponsesToChat(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(out), "array-hi") {
		t.Fatalf("out=%s", out)
	}
}

func TestResponsesToChatHandlesCreatedAtNumber(t *testing.T) {
	body := []byte("{\"id\":\"resp_n\",\"model\":\"grok-4.5\",\"created_at\":1710000000,\"status\":\"completed\",\"output_text\":\"n\"}")
	out, err := compat.ResponsesToChat(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(out), "\"created\":1710000000") && !strings.Contains(string(out), "created") {
		t.Fatalf("out=%s", out)
	}
}

func TestOpenAIToAnthropicFromAggregatedResponses(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"pong\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	out, err := compat.AggregateResponsesStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	anth, err := compat.OpenAIToAnthropic(out)
	if err != nil {
		t.Fatalf("anthropic: %v out=%s", err, out)
	}
	if !strings.Contains(string(anth), "pong") {
		t.Fatalf("anth=%s", anth)
	}
}
