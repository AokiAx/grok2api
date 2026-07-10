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

func TestChatToResponsesMapsWebSearchTool(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`)
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
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools should be preserved: %s", out)
	}
}

func TestEnsureBackendSearchDefaultsOnAndRespectsExplicit(t *testing.T) {
	enabled, err := compat.EnsureBackendSearch([]byte(`{"model":"grok-4.5","input":[]}`), true)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(enabled, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["backend_search"] != true {
		t.Fatalf("default backend_search = %#v", payload["backend_search"])
	}

	kept, err := compat.EnsureBackendSearch([]byte(`{"model":"grok-4.5","backend_search":false}`), true)
	if err != nil {
		t.Fatalf("ensure explicit: %v", err)
	}
	if err := json.Unmarshal(kept, &payload); err != nil {
		t.Fatalf("decode explicit: %v", err)
	}
	if payload["backend_search"] != false {
		t.Fatalf("explicit false overwritten: %#v", payload["backend_search"])
	}

	mirrored, err := compat.EnsureBackendSearch([]byte(`{"model":"grok-4.5","web_search":true}`), true)
	if err != nil {
		t.Fatalf("ensure web_search: %v", err)
	}
	if err := json.Unmarshal(mirrored, &payload); err != nil {
		t.Fatalf("decode web_search: %v", err)
	}
	if payload["backend_search"] != true {
		t.Fatalf("web_search mirror = %#v", payload["backend_search"])
	}
}

func TestChatToResponsesExpandsNamespaceTools(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"namespace","name":"demo","description":"Demo tools","tools":[{"type":"function","name":"inner","description":"x","strict":false,"parameters":{"type":"object"}}]},{"type":"function","name":"outer","parameters":{"type":"object"}}]}`)
	out, _, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v", payload["tools"])
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["type"] == "namespace" {
			t.Fatalf("namespace should be expanded: %#v", tool)
		}
		if tool["type"] != "function" {
			t.Fatalf("tool type = %#v", tool["type"])
		}
	}
}

func TestNormalizeResponsesToolsDedupsExpandedNames(t *testing.T) {
	raw := []any{
		map[string]any{"type": "namespace", "name": "ns", "tools": []any{
			map[string]any{"type": "function", "name": "a", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "function", "name": "b", "parameters": map[string]any{"type": "object"}},
		}},
		map[string]any{"type": "function", "name": "a", "parameters": map[string]any{"type": "object"}},
	}
	out := compat.NormalizeResponsesTools(raw, 10)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2 (expanded+dedup): %#v", len(out), out)
	}
}

func TestChatToResponsesPreservesToolsAndStripsUnsafeFields(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo"}}],"tool_choice":"auto","metadata":{"a":1},"user":"u1","tool_resources":{}}`)
	out, _, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["tools"]; !ok {
		t.Fatalf("tools should be preserved: %s", out)
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v", payload["tool_choice"])
	}
	for _, key := range []string{"metadata", "user", "tool_resources"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("field %q should be stripped but was present in: %s", key, out)
		}
	}
}

func TestChatToResponsesMapsToolLoopToResponsesItems(t *testing.T) {
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
	if payload["instructions"] != "you are helpful" {
		t.Fatalf("instructions=%#v", payload["instructions"])
	}
	input, ok := payload["input"].([]any)
	if !ok {
		t.Fatalf("input is not array: %#v", payload["input"])
	}
	// system → instructions; remaining: user, function_call, function_call_output
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d: %#v", len(input), input)
	}
	call := input[1].(map[string]any)
	if call["type"] != "function_call" || call["name"] != "foo" || call["call_id"] != "call_1" {
		t.Fatalf("function_call=%#v", call)
	}
	outItem := input[2].(map[string]any)
	if outItem["type"] != "function_call_output" || outItem["call_id"] != "call_1" || outItem["output"] != "result" {
		t.Fatalf("function_call_output=%#v", outItem)
	}
}

func TestResponsesToChatExtractsFunctionCalls(t *testing.T) {
	body := []byte(`{
		"id":"resp_tools",
		"model":"grok-4.5",
		"status":"completed",
		"output":[
			{"type":"function_call","call_id":"call_9","name":"lookup","arguments":"{\"q\":\"x\"}"}
		],
		"usage":{"input_tokens":1,"output_tokens":2}
	}`)
	out, err := compat.ResponsesToChat(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choice := payload["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason=%#v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", message["tool_calls"])
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_9" || fn["name"] != "lookup" || fn["arguments"] != `{"q":"x"}` {
		t.Fatalf("call=%#v", call)
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

func TestNormalizeResponsesToolsHandlesEmptyAndMissingType(t *testing.T) {
	if out := compat.NormalizeResponsesTools(nil, 10); out != nil {
		t.Fatalf("nil tools => %#v", out)
	}
	raw := []any{
		map[string]any{"function": map[string]any{"name": "implicit", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "mcp", "name": "browser"},
		"bad",
	}
	out := compat.NormalizeResponsesTools(raw, 10)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2: %#v", len(out), out)
	}
	first := out[0].(map[string]any)
	if first["type"] != "function" {
		t.Fatalf("implicit function type = %#v", first["type"])
	}
}

func TestNormalizeResponsesToolsSoftCapAndNestedFunctionName(t *testing.T) {
	raw := []any{
		map[string]any{"type": "namespace", "name": "ns", "tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "nested_only", "parameters": map[string]any{"type": "object"}}},
			map[string]any{"type": "function", "name": "keep_me", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "web_search"},
		}},
		map[string]any{"type": "function", "name": "extra1", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "extra2", "parameters": map[string]any{"type": "object"}},
	}
	out := compat.NormalizeResponsesTools(raw, 2)
	if len(out) != 2 {
		t.Fatalf("soft cap len=%d want 2: %#v", len(out), out)
	}
	first := out[0].(map[string]any)
	if first["type"] != "function" || first["name"] != "nested_only" {
		t.Fatalf("first=%#v", first)
	}
}

func TestStripUnknownResponsesFieldsKeepsWhitelist(t *testing.T) {
	input := []byte(`{"model":"grok-4.5","input":[],"stream":true,"backend_search":true,"external_web_access":true,"metadata":{"k":"v"},"user":"u1","tool_resources":{},"temperature":0.7}`)
	out, err := compat.StripUnknownResponsesFields(input)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"model", "input", "stream", "backend_search", "temperature"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("allowed field %q missing", key)
		}
	}
	for _, key := range []string{"external_web_access", "metadata", "user", "tool_resources"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("unexpected field %q present", key)
		}
	}
}

func TestStripUnknownResponsesFieldsNoopWhenClean(t *testing.T) {
	input := []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"hi"}]}`)
	out, err := compat.StripUnknownResponsesFields(input)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if string(out) != string(input) {
		t.Fatalf("expected noop, got %s", out)
	}
}

func TestNormalizeResponsesToolsFlattensChatFunctionShape(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "Read",
				"description": "Read a file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	out := compat.NormalizeResponsesTools(raw, 10)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1: %#v", len(out), out)
	}
	tool := out[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "Read" || tool["description"] != "Read a file" {
		t.Fatalf("tool identity not flattened: %#v", tool)
	}
	if _, ok := tool["parameters"].(map[string]any); !ok {
		t.Fatalf("top-level parameters missing: %#v", tool)
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested Chat function must not reach Responses: %#v", tool)
	}
}

func TestAnthropicToolsReachResponsesAsFlatFunctions(t *testing.T) {
	anthropic := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"inspect"}],
		"tools":[{
			"name":"Inspect",
			"description":"Inspect the workspace",
			"input_schema":{"type":"object","properties":{"path":{"type":"string"}}}
		}]
	}`)

	chat, _, err := compat.AnthropicToOpenAI(anthropic, "grok-4.5")
	if err != nil {
		t.Fatalf("anthropic to chat: %v", err)
	}
	responses, _, err := compat.ChatToResponses(chat)
	if err != nil {
		t.Fatalf("chat to responses: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(responses, &payload); err != nil {
		t.Fatalf("decode responses: %v", err)
	}
	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "Inspect" {
		t.Fatalf("name=%#v payload=%s", tool["name"], responses)
	}
	if _, ok := tool["parameters"].(map[string]any); !ok {
		t.Fatalf("top-level parameters missing: %s", responses)
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested function leaked: %s", responses)
	}
}

func TestNormalizeResponsesToolsFlattensNamespaceChildren(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "namespace",
			"name": "workspace",
			"tools": []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name": "Search",
					},
				},
			},
		},
	}

	out := compat.NormalizeResponsesTools(raw, 10)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1: %#v", len(out), out)
	}
	tool := out[0].(map[string]any)
	if tool["name"] != "Search" {
		t.Fatalf("name=%#v tool=%#v", tool["name"], tool)
	}
	parameters, ok := tool["parameters"].(map[string]any)
	if !ok || parameters["type"] != "object" {
		t.Fatalf("default top-level parameters missing: %#v", tool)
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested namespace function leaked: %#v", tool)
	}
}

func TestChatToResponsesFlattensFunctionToolChoice(t *testing.T) {
	chat := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"inspect"}],
		"tool_choice":{"type":"function","function":{"name":"Inspect"}}
	}`)

	responses, _, err := compat.ChatToResponses(chat)
	if err != nil {
		t.Fatalf("chat to responses: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(responses, &payload); err != nil {
		t.Fatalf("decode responses: %v", err)
	}
	choice := payload["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "Inspect" {
		t.Fatalf("choice not flattened: %#v", choice)
	}
	if _, exists := choice["function"]; exists {
		t.Fatalf("nested function choice leaked: %#v", choice)
	}
}
