package compat_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/compat"
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

func TestResponsesToAnthropicFromAggregatedResponses(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"pong\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	anth, err := compat.AggregateResponsesToAnthropic(io.NopCloser(strings.NewReader(sse)), "claude-sonnet-5", false, "")
	if err != nil {
		t.Fatalf("aggregate anthropic: %v", err)
	}
	if !strings.Contains(string(anth), "pong") {
		t.Fatalf("anth=%s", anth)
	}
	if !strings.Contains(string(anth), `"model":"claude-sonnet-5"`) {
		t.Fatalf("want client model echo: %s", anth)
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

func TestSanitizeResponsesInputMapsCodexShellAndPreservesBuiltins(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "run"}},
		},
		map[string]any{
			"type": "local_shell_call", "call_id": "c1",
			"action": map[string]any{"type": "exec", "command": []any{"echo", "hi"}},
		},
		map[string]any{"type": "local_shell_call_output", "call_id": "c1", "output": "hi\n"},
		map[string]any{"type": "item_reference", "id": "msg_x"},
		map[string]any{"type": "web_search_call", "id": "ws1", "status": "completed", "query": "golang"},
		map[string]any{"type": "message", "role": "user", "content": nil},
		map[string]any{
			"type": "custom_tool_call", "call_id": "c2", "name": "apply_patch", "input": "{\"p\":1}",
		},
		map[string]any{
			"type": "computer_call", "call_id": "c3",
			"action": map[string]any{"type": "click", "x": 1, "y": 2},
		},
	}
	out := compat.SanitizeResponsesInput(raw).([]any)
	var (
		fnCalls, fnOuts           int
		webSearch, computer, note bool
		foundEmpty                bool
	)
	for _, item := range out {
		m := item.(map[string]any)
		switch m["type"] {
		case "function_call":
			fnCalls++
			switch m["name"] {
			case "web_search":
				webSearch = true
				if !strings.Contains(stringValueTest(m["arguments"]), "golang") {
					t.Fatalf("web_search args missing query: %#v", m)
				}
			case "computer":
				computer = true
			case "shell_command", "apply_patch":
				// ok
			}
		case "function_call_output":
			fnOuts++
		default:
			if m["role"] == "user" && m["content"] == "" {
				foundEmpty = true
			}
			if content, _ := m["content"].(string); strings.Contains(content, "item_reference") {
				note = true
			}
		}
		// raw OpenAI built-in types must not leak
		if typ := stringValueTest(m["type"]); strings.Contains(typ, "local_shell") ||
			typ == "item_reference" || typ == "web_search_call" || typ == "computer_call" || typ == "custom_tool_call" {
			t.Fatalf("unsupported type leaked: %#v", m)
		}
	}
	if fnCalls < 4 || fnOuts < 1 {
		t.Fatalf("expected mapped function calls/outputs, got calls=%d outs=%d out=%#v", fnCalls, fnOuts, out)
	}
	if !webSearch || !computer || !note || !foundEmpty {
		t.Fatalf("web=%v computer=%v note=%v empty=%v out=%#v", webSearch, computer, note, foundEmpty, out)
	}

	// Full finalize path must not 422-shape
	body, _ := json.Marshal(map[string]any{
		"model": "grok-4.5", "stream": false, "input": raw,
		"external_web_access": false, "store": false,
	})
	finalized, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{SupportsBackendSearch: true})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	encoded := string(finalized)
	for _, bad := range []string{`"type":"local_shell_call"`, `"type":"item_reference"`, `"type":"web_search_call"`, `"type":"custom_tool_call"`, `"type":"computer_call"`} {
		if strings.Contains(encoded, bad) {
			t.Fatalf("finalized still has %s: %s", bad, encoded)
		}
	}
}

func stringValueTest(v any) string {
	s, _ := v.(string)
	return s
}

func TestNormalizeResponsesToolsMapsCodexBuiltinsToFunctions(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":                 "web_search",
			"external_web_access":  true,
			"indexed_web_access":   true,
			"search_context_size":  "medium",
			"search_content_types": []any{"text"},
			// filters/allowed_domains hard-reject; use strip-only extras here.
			"user_location": map[string]any{"type": "approximate", "country": "US"},
		},
		map[string]any{"type": "local_shell"},
		map[string]any{"type": "tool_search", "execution": "x", "description": "Search tools", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "shell"}, // missing environment → shell_command function
		map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch"},
		map[string]any{
			"type": "function",
			"name": "shell_command",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string"}},
			},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	// web_search + shell_command (deduped from local_shell/shell/function) + tool_search + apply_patch
	if len(result.Tools) != 4 {
		t.Fatalf("tools=%#v want 4", result.Tools)
	}
	first := result.Tools[0].(map[string]any)
	if first["type"] != "web_search" || len(first) != 1 {
		t.Fatalf("web_search not bare: %#v", first)
	}
	if result.BackendSearch == nil || !*result.BackendSearch {
		t.Fatalf("BackendSearch=%v want true from external_web_access", result.BackendSearch)
	}
	byName := map[string]map[string]any{}
	for _, rawTool := range result.Tools {
		tool := rawTool.(map[string]any)
		if tool["type"] == "function" {
			byName[stringValueTest(tool["name"])] = tool
		}
	}
	for _, name := range []string{"shell_command", "tool_search", "apply_patch"} {
		if byName[name] == nil {
			t.Fatalf("missing function %s in %#v", name, result.Tools)
		}
	}
}

func TestFinalizeResponsesUpstreamStripsNestedToolExternalWebAccess(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"stream":false,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[
			{"type":"web_search","external_web_access":true,"search_context_size":"medium"},
			{"type":"local_shell"},
			{"type":"function","name":"shell_command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}
		],
		"external_web_access":false,
		"store":false,
		"parallel_tool_calls":true
	}`)
	out, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{SupportsBackendSearch: true})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["external_web_access"]; ok {
		t.Fatalf("top-level external_web_access leaked: %#v", payload)
	}
	// Nested tool external_web_access wins when top-level was only a rejected field
	// (mapped first); tool-level true should still set backend_search if not set —
	// but top-level false was mapped first. Either way tools must be clean.
	tools, _ := payload["tools"].([]any)
	if len(tools) < 2 {
		t.Fatalf("tools=%#v", tools)
	}
	var sawWeb, sawShell, sawLocalShell bool
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if _, ok := tool["external_web_access"]; ok {
			t.Fatalf("nested external_web_access leaked: %#v", tool)
		}
		if _, ok := tool["search_context_size"]; ok {
			t.Fatalf("search_context_size leaked: %#v", tool)
		}
		if tool["type"] == "local_shell" {
			sawLocalShell = true
		}
		if tool["type"] == "web_search" {
			sawWeb = true
		}
		if tool["type"] == "function" && tool["name"] == "shell_command" {
			sawShell = true
		}
	}
	if sawLocalShell {
		t.Fatalf("local_shell type must be converted, not forwarded: %#v", tools)
	}
	if !sawWeb || !sawShell {
		t.Fatalf("expected web_search + shell_command function, got %#v", tools)
	}
	encoded := string(out)
	if strings.Contains(encoded, "external_web_access") || strings.Contains(encoded, `"type":"local_shell"`) {
		t.Fatalf("forbidden tokens remain: %s", encoded)
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

	responses, _, _, err := compat.AnthropicToResponses(anthropic, "grok-4.5")
	if err != nil {
		t.Fatalf("anthropic to responses: %v", err)
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

func TestSanitizeDeveloperRoleMapsToSystem(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "message",
			"role": "developer",
			"content": []any{
				map[string]any{"type": "input_text", "text": "permissions"},
			},
		},
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "hi"},
			},
		},
	}
	out := compat.SanitizeResponsesInput(raw).([]any)
	first := out[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role not mapped: %#v", first)
	}
	if first["type"] != "message" {
		t.Fatalf("type=%#v", first["type"])
	}
}

func TestCollapseSearchToolNameCollisionsDropsFunctionWebSearch(t *testing.T) {
	raw := []any{
		map[string]any{"type": "function", "name": "web_search", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "Read", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "web_search"},
	}
	out := compat.NormalizeResponsesTools(raw, 16)
	var sawBare, sawFnWeb, sawRead bool
	for _, item := range out {
		tool := item.(map[string]any)
		if tool["type"] == "web_search" {
			sawBare = true
		}
		if tool["type"] == "function" && tool["name"] == "web_search" {
			sawFnWeb = true
		}
		if tool["type"] == "function" && tool["name"] == "Read" {
			sawRead = true
		}
	}
	if !sawBare || sawFnWeb || !sawRead {
		t.Fatalf("bare=%v fnWeb=%v read=%v tools=%#v", sawBare, sawFnWeb, sawRead, out)
	}
}

func TestFinalizeDropsReasoningEffortNoneAndInjectsSearchWithoutDup(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"stream":false,
		"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]}],
		"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}},{"type":"function","name":"shell_command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}],
		"reasoning_effort":"none"
	}`)
	out, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{SupportsBackendSearch: true})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort none must be stripped: %#v", payload["reasoning_effort"])
	}
	input := payload["input"].([]any)
	first := input[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer→system failed: %#v", first)
	}
	tools := payload["tools"].([]any)
	names := map[string]int{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		key := stringValueTest(tool["type"]) + ":" + stringValueTest(tool["name"])
		names[key]++
		if tool["type"] == "function" && tool["name"] == "web_search" {
			t.Fatalf("function web_search must not coexist with bare: %#v", tools)
		}
	}
	if names["web_search:"] != 1 {
		t.Fatalf("want one bare web_search, got %#v tools=%#v", names, tools)
	}
	if names["x_search:"] != 1 {
		t.Fatalf("want injected x_search: %#v", tools)
	}
}

func TestSanitizeCompactionAndEncryptedContent(t *testing.T) {
	raw := []any{
		map[string]any{"type": "compaction", "blob": "opaque-openai-blob"},
		map[string]any{
			"type":              "reasoning",
			"id":                "rs_1",
			"summary":           []any{map[string]any{"type": "summary_text", "text": "plan"}},
			"encrypted_content": "foreign-enc",
		},
	}
	out := compat.SanitizeResponsesInput(raw).([]any)
	if len(out) != 2 {
		t.Fatalf("len=%d %#v", len(out), out)
	}
	note := out[0].(map[string]any)
	if content, _ := note["content"].(string); !strings.Contains(content, "compaction") {
		t.Fatalf("compaction not converted: %#v", note)
	}
	rs := out[1].(map[string]any)
	if rs["type"] != "reasoning" {
		t.Fatalf("reasoning type=%#v", rs["type"])
	}
	if _, has := rs["encrypted_content"]; has {
		t.Fatalf("encrypted_content leaked: %#v", rs)
	}
}

func TestNormalizeWebSearchExternalAccessFalseDropsTool(t *testing.T) {
	raw := []any{
		map[string]any{"type": "web_search", "external_web_access": false},
		map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err != nil {
		t.Fatalf("err=%v", result.Err)
	}
	if result.BackendSearch == nil || *result.BackendSearch {
		t.Fatalf("BackendSearch=%v want false", result.BackendSearch)
	}
	for _, tool := range result.Tools {
		m, _ := tool.(map[string]any)
		if m["type"] == "web_search" {
			t.Fatalf("web_search should be dropped: %#v", result.Tools)
		}
	}
	found := false
	for _, code := range result.Warnings {
		if code == "web_search_disabled_no_external_access" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings=%v", result.Warnings)
	}
	if len(result.Tools) != 1 {
		t.Fatalf("tools=%#v", result.Tools)
	}
}

func TestNormalizeWebSearchFiltersHardReject(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":    "web_search",
			"filters": map[string]any{"allowed_domains": []any{"example.com"}},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err == nil {
		t.Fatal("expected hard reject for filters")
	}
	if !strings.Contains(result.Err.Error(), "filters") {
		t.Fatalf("err=%v", result.Err)
	}
}

func TestNormalizeWebSearchAllowedDomainsHardReject(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":            "web_search",
			"allowed_domains": []any{"example.com"},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err == nil {
		t.Fatal("expected hard reject for allowed_domains")
	}
}

func TestNormalizeResponsesRequestRejectsWebSearchFilters(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"input":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search","filters":{"foo":true}}]
	}`)
	_, _, _, err := compat.NormalizeResponsesRequest(body, "grok-4.5")
	if err == nil {
		t.Fatal("expected error")
	}
}
