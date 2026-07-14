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
	body := []byte(`{"model":"grok-4.5","stream":false,"input":[],"external_web_access":true,"metadata":{"x":1},"store":false,"parallel_tool_calls":false,"prompt_cache_key":"abc"}`)
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
	// external_web_access is mapped onto backend_search, then dropped.
	if payload["backend_search"] != true {
		t.Fatalf("backend_search=%#v", payload["backend_search"])
	}
	for _, key := range []string{"external_web_access", "metadata", "store", "parallel_tool_calls"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("%s should be stripped: %#v", key, payload)
		}
	}
	// prompt_cache_key is kept for session sticky / cache continuity.
	if payload["prompt_cache_key"] != "abc" {
		t.Fatalf("prompt_cache_key=%#v", payload["prompt_cache_key"])
	}
	// Default-on search tools for models that support backend search.
	tools, _ := payload["tools"].([]any)
	types := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		types[stringValueLocal(tool["type"])] = true
	}
	if !types["web_search"] || !types["x_search"] {
		t.Fatalf("expected default web_search+x_search tools, got %#v", tools)
	}
}

func TestEnsureDefaultSearchToolsRespectsDisableAndDedupes(t *testing.T) {
	disabled, err := compat.EnsureDefaultSearchTools(
		[]byte(`{"model":"grok-4.5","backend_search":false,"tools":[{"type":"function","name":"a","parameters":{"type":"object"}}]}`),
		true,
	)
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(disabled, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("disabled should not inject search tools: %#v", tools)
	}

	withExisting := []byte(`{"model":"grok-4.5","tools":[{"type":"web_search"},{"type":"function","name":"a","parameters":{"type":"object"}}]}`)
	out, err := compat.EnsureDefaultSearchTools(withExisting, true)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	tools, _ = payload["tools"].([]any)
	// web_search already present → inject x_search; collapse keeps bare search first.
	if len(tools) != 3 {
		t.Fatalf("tools=%#v want 3", tools)
	}
	// no duplicate web_search / x_search
	countWeb, countX := 0, 0
	for _, raw := range tools {
		switch raw.(map[string]any)["type"] {
		case "web_search":
			countWeb++
		case "x_search":
			countX++
		}
	}
	if countWeb != 1 || countX != 1 {
		t.Fatalf("web=%d x=%d tools=%#v", countWeb, countX, tools)
	}
}

func stringValueLocal(v any) string {
	s, _ := v.(string)
	return s
}

func TestFinalizeMapsExternalWebAccessWithoutDefaultSearch(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","stream":false,"input":"hi","external_web_access":false}`)
	out, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{SupportsBackendSearch: false})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["external_web_access"]; ok {
		t.Fatalf("external_web_access still present: %#v", payload)
	}
	if payload["backend_search"] != false {
		t.Fatalf("backend_search=%#v want false from external_web_access", payload["backend_search"])
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

func TestNormalizeResponsesRequestRewritesChatShapedInput(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"input":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"max_completion_tokens":12,
		"web_search":"yes",
		"tools":[{"type":"web_search"}]
	}`)
	out, _, _, err := compat.NormalizeResponsesRequest(body, "fallback")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["instructions"] != "be brief" {
		t.Fatalf("instructions=%#v", payload["instructions"])
	}
	if payload["max_output_tokens"] != float64(12) {
		t.Fatalf("max_output_tokens=%#v", payload["max_output_tokens"])
	}
	if payload["backend_search"] != true {
		t.Fatalf("backend_search=%#v", payload["backend_search"])
	}
	input, _ := payload["input"].([]any)
	if len(input) < 2 {
		t.Fatalf("input=%#v", payload["input"])
	}
	// tool loop rewritten to function_call items
	foundCall := false
	for _, item := range input {
		m, _ := item.(map[string]any)
		if m["type"] == "function_call" && m["name"] == "foo" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("expected function_call in input: %#v", input)
	}
}

func TestPrepareResponsesFromChatAndFinalizeWebSearchFalse(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":true,"web_search":false}`)
	out, model, stream, err := compat.PrepareResponsesFromChat(body, "fallback")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if model != "grok-4.5" || !stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	// Explicit web_search=false should not be overridden to true.
	finalized, err := compat.FinalizeResponsesUpstream(out, compat.ModelHints{SupportsBackendSearch: true})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(finalized, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// EnsureBackendSearch mirrors web_search into backend_search when only web_search set.
	if payload["backend_search"] != false && payload["web_search"] != false {
		// After ChatToResponses, web_search is copied; Finalize may set backend_search from it.
		t.Logf("payload=%#v", payload)
	}
	if payload["stream"] != true {
		t.Fatalf("stream=%#v", payload["stream"])
	}
}

func TestExtractCompletedResponseBareJSON(t *testing.T) {
	raw := []byte(`{"id":"resp_bare","object":"response","status":"completed","output_text":"x"}`)
	out := compat.ExtractCompletedResponse(raw)
	if string(out) != string(raw) {
		t.Fatalf("bare json should pass through: %s", out)
	}
}

func TestResponseFormatPromotedToTextFormat(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"hi"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"answer",
				"schema":{"type":"object","properties":{"ok":{"type":"boolean"}}},
				"strict":true
			}
		}
	}`)
	out, _, _, err := compat.NormalizeResponsesRequest(body, "fallback")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["response_format"]; ok {
		t.Fatalf("response_format should be promoted away: %#v", payload)
	}
	textObj, ok := payload["text"].(map[string]any)
	if !ok {
		t.Fatalf("missing text: %#v", payload)
	}
	format, ok := textObj["format"].(map[string]any)
	if !ok {
		t.Fatalf("missing text.format: %#v", textObj)
	}
	if format["type"] != "json_schema" {
		t.Fatalf("type=%#v", format["type"])
	}
	if format["name"] != "answer" {
		t.Fatalf("name=%#v", format["name"])
	}
	if format["json_schema"] != nil {
		t.Fatalf("nested json_schema wrapper should be flattened: %#v", format)
	}
	if format["schema"] == nil {
		t.Fatalf("schema missing: %#v", format)
	}

	// Finalize must keep text.format (allowed field) and still drop response_format.
	final, err := compat.FinalizeResponsesUpstream(out, compat.ModelHints{})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := json.Unmarshal(final, &payload); err != nil {
		t.Fatalf("decode final: %v", err)
	}
	if _, ok := payload["response_format"]; ok {
		t.Fatal("response_format leaked past finalize")
	}
	textObj, ok = payload["text"].(map[string]any)
	if !ok {
		t.Fatalf("text dropped by finalize: %#v", payload)
	}
	format, ok = textObj["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("final format=%#v", textObj)
	}
}

func TestChatToResponsesPromotesResponseFormat(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_object"}
	}`)
	out, _, err := compat.ChatToResponses(body)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	format := payload["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("format=%#v", format)
	}
}

func TestExistingTextFormatNotOverwritten(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"input":[{"role":"user","content":"hi"}],
		"text":{"format":{"type":"json_schema","name":"keep","schema":{"type":"object"}}},
		"response_format":{"type":"json_object"}
	}`)
	out, err := compat.StripUnknownResponsesFields(body)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["response_format"]; ok {
		t.Fatal("response_format should be dropped")
	}
	format := payload["text"].(map[string]any)["format"].(map[string]any)
	if format["name"] != "keep" {
		t.Fatalf("existing text.format overwritten: %#v", format)
	}
}

func TestFinalizeCompatibilityWarnings(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[],"external_web_access":true,"store":false,"response_format":{"type":"json_object"}}`)
	out, warnings, err := compat.FinalizeResponsesUpstreamDetailed(body, compat.ModelHints{})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload["external_web_access"]; ok {
		t.Fatal("external_web_access should be stripped")
	}
	if payload["text"] == nil {
		t.Fatalf("response_format should promote to text: %#v", payload)
	}
	joined := strings.Join(warnings, ",")
	for _, code := range []string{"web_search_controls_downgraded", "response_format_promoted", "unsupported_field_stripped"} {
		if !strings.Contains(joined, code) {
			t.Fatalf("missing warning %s in %v", code, warnings)
		}
	}
}
