package compat_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/compat"
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
	// Mirrors live Grok SSE: added (name, empty args) → arguments.delta (full JSON) → done.
	sse := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"Read","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"file_path\":\"/tmp/t\",\"limit\":10}"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","output_index":1}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"/tmp/t\",\"limit\":10}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"/tmp/t\",\"limit\":10}"}]}}`,
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
	if !strings.Contains(body, `"name":"Read"`) {
		t.Fatalf("missing name: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing tool_calls finish: %s", body)
	}
	// Reconstruct OpenAI-style tool call accumulation by index.
	type toolAcc struct {
		id, name, args string
	}
	acc := map[int]*toolAcc{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			idx := int(tc["index"].(float64))
			if acc[idx] == nil {
				acc[idx] = &toolAcc{}
			}
			if id, _ := tc["id"].(string); id != "" {
				acc[idx].id = id
			}
			fn, _ := tc["function"].(map[string]any)
			if n, _ := fn["name"].(string); n != "" {
				acc[idx].name = n
			}
			if a, _ := fn["arguments"].(string); a != "" {
				acc[idx].args += a
			}
		}
	}
	if len(acc) != 1 {
		t.Fatalf("expected single tool index, got %d body=%s", len(acc), body)
	}
	t0 := acc[0]
	if t0.name != "Read" || !strings.Contains(t0.args, "file_path") || t0.args == "{}" {
		t.Fatalf("reconstructed tool=%+v body=%s", t0, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(t0.args), &parsed); err != nil {
		t.Fatalf("arguments must be valid JSON, got %q: %v", t0.args, err)
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

func TestResponsesToChatStreamDoneWithoutDeltasUsesFinalArgs(t *testing.T) {
	// Some backends skip arguments.delta and only send full args on output_item.done.
	sse := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_x","name":"Lookup","arguments":""}}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_x","name":"Lookup","arguments":"{\"q\":\"hi\"}"}}`,
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
	body := string(data)
	if strings.Count(body, `"index":0`) < 1 {
		t.Fatalf("expected tool index 0: %s", body)
	}
	if strings.Contains(body, `"index":1`) {
		t.Fatalf("must not invent second tool index: %s", body)
	}
	if !strings.Contains(body, `\"q\":\"hi\"`) && !strings.Contains(body, `"q":"hi"`) {
		// arguments appear escaped inside JSON string
		if !strings.Contains(body, "hi") {
			t.Fatalf("missing final args: %s", body)
		}
	}
}

func TestResponsesToChatStreamPrematureEOFEmitsErrorNotStop(t *testing.T) {
	// Upstream closes after partial text without response.completed.
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_cut"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToChatStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `"content":"partial"`) {
		t.Fatalf("missing partial content: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("must not forge stop on truncated stream: %s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("must not emit [DONE] after truncated stream: %s", body)
	}
	if !strings.Contains(body, `"upstream_stream_truncated"`) || !strings.Contains(body, "terminal event") {
		t.Fatalf("expected stream error payload: %s", body)
	}
}

func TestResponsesToChatStreamIncompleteUsesLength(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"cut"}`,
		``,
		`event: response.incomplete`,
		`data: {"type":"response.incomplete","response":{"id":"resp_i","status":"incomplete","output_text":"cut"}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToChatStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("want length finish: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("want [DONE] after incomplete: %s", body)
	}
}

func TestResponsesToChatStreamFailedEmitsError(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_f"}}`,
		``,
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"error":{"message":"quota boom"}}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToChatStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "quota boom") {
		t.Fatalf("missing error message: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("must not stop-finish after failed: %s", body)
	}
}

func TestResponsesToChatObjectArgumentsBecomeJSONString(t *testing.T) {
	// Upstream occasionally returns structured arguments objects.
	body := []byte(`{
		"id":"resp_obj",
		"model":"grok-4.5",
		"status":"completed",
		"output":[{"type":"function_call","call_id":"call_1","name":"Read","arguments":{"file_path":"/tmp/a","limit":3}}]
	}`)
	out, err := compat.ResponsesToChat(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	call := msg["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments should be string, got %#v", fn["arguments"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments not JSON: %q err=%v", args, err)
	}
	if parsed["file_path"] != "/tmp/a" {
		t.Fatalf("parsed=%#v", parsed)
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
