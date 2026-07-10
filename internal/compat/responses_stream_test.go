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

func TestResponsesToChatStreamEmitsToolCallDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","call_id":"call_1","delta":"{\"q\":"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","call_id":"call_1","delta":"\"x\"}"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}]}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToChatStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = stream.Close()
	body := string(data)
	if !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("missing tool_calls: %s", body)
	}
	if !strings.Contains(body, `"name":"lookup"`) {
		t.Fatalf("missing name: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing tool_calls finish: %s", body)
	}
}

func TestAggregateResponsesStreamFromToolCallEvents(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_z","name":"search","arguments":"{\"q\":1}"}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_z","name":"search","arguments":"{\"q\":2}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_tools","model":"grok-4.5","status":"completed","output":[{"type":"function_call","call_id":"call_z","name":"search","arguments":"{\"q\":2}"}],"usage":{"input_tokens":1,"output_tokens":2}}}`,
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
	choice := payload["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason=%#v body=%s", choice["finish_reason"], out)
	}
	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", message["tool_calls"])
	}
}

func TestResponsesToChatStreamUnknownCallIDDelta(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","call_id":"orphan","delta":"{}"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToChatStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = stream.Close()
	if !strings.Contains(string(data), `"tool_calls"`) {
		t.Fatalf("body=%s", data)
	}
}

func TestAggregateResponsesStreamCompletedEnvelopeJSON(t *testing.T) {
	raw := `{"type":"response.completed","response":{"id":"resp_wrap","model":"grok-4.5","status":"completed","output_text":"wrapped","usage":{"input_tokens":1,"output_tokens":1}}}`
	out, err := compat.AggregateResponsesStream(io.NopCloser(strings.NewReader(raw)), "grok-4.5")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !strings.Contains(string(out), `"content":"wrapped"`) {
		t.Fatalf("out=%s", out)
	}
}
