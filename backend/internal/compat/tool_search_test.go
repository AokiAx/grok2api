package compat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/compat"
)

func TestClientToolSearchDefersLoading(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":        "tool_search",
			"execution":   "client",
			"description": "Find tools",
		},
		map[string]any{
			"type":          "function",
			"name":          "heavy_tool",
			"description":   "Heavy deferred tool",
			"defer_loading": true,
			"parameters":    map[string]any{"type": "object"},
		},
		map[string]any{
			"type":       "function",
			"name":       "always_on",
			"parameters": map[string]any{"type": "object"},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 32)
	if result.Err != nil {
		t.Fatalf("normalize: %v", result.Err)
	}
	names := map[string]bool{}
	for _, rawTool := range result.Tools {
		tool := rawTool.(map[string]any)
		if tool["type"] == "function" {
			names[tool["name"].(string)] = true
		}
	}
	if !names["always_on"] {
		t.Fatalf("always_on missing: %#v", result.Tools)
	}
	if names["heavy_tool"] {
		t.Fatalf("deferred heavy_tool should not be sent: %#v", result.Tools)
	}
	if !names["grok2api_tool_search"] {
		t.Fatalf("client tool_search function missing: %#v", result.Tools)
	}
	// Description should mention deferred surface.
	var searchDesc string
	for _, rawTool := range result.Tools {
		tool := rawTool.(map[string]any)
		if tool["name"] == "grok2api_tool_search" {
			searchDesc, _ = tool["description"].(string)
		}
	}
	if !strings.Contains(searchDesc, "heavy_tool") {
		t.Fatalf("search description missing deferred surface: %q", searchDesc)
	}
	found := false
	for _, w := range result.Warnings {
		if w == "client_tool_search_emulated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings=%v", result.Warnings)
	}
}

func TestDeferLoadingRequiresClientToolSearch(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":          "function",
			"name":          "heavy",
			"defer_loading": true,
			"parameters":    map[string]any{"type": "object"},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err == nil {
		t.Fatal("expected error for defer_loading without tool_search")
	}
	if re, ok := compat.AsRequestError(result.Err); !ok || re.Param == "" {
		t.Fatalf("want RequestError with param, got %v", result.Err)
	}
}

func TestServerToolSearchRejected(t *testing.T) {
	raw := []any{
		map[string]any{"type": "tool_search", "execution": "server"},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err == nil {
		t.Fatal("expected reject for server tool_search")
	}
}

func TestToolSearchOutputLoadsToolsIntoRequest(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"stream":false,
		"input":[
			{"type":"tool_search_call","call_id":"c1","arguments":"{\"q\":\"x\"}"},
			{"type":"tool_search_output","call_id":"c1","execution":"client","tools":[
				{"type":"function","name":"loaded","parameters":{"type":"object"}}
			]}
		],
		"tools":[
			{"type":"tool_search","execution":"client"}
		]
	}`)
	out, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["type"] == "function" {
			names[stringValueAnyLocal(tool["name"])] = true
		}
	}
	if !names["loaded"] {
		t.Fatalf("loaded tool missing after tool_search_output: %#v", tools)
	}
	if !names["grok2api_tool_search"] {
		t.Fatalf("search function missing: %#v", tools)
	}
	// History rewritten to function_call / function_call_output.
	input := payload["input"].([]any)
	if len(input) < 2 {
		t.Fatalf("input=%#v", input)
	}
	call := input[0].(map[string]any)
	if call["type"] != "function_call" || call["name"] != "grok2api_tool_search" {
		t.Fatalf("call=%#v", call)
	}
	outItem := input[1].(map[string]any)
	if outItem["type"] != "function_call_output" || outItem["call_id"] != "c1" {
		t.Fatalf("output=%#v", outItem)
	}
}

func TestParallelToolCallsRejectedWithClientToolSearch(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"parallel_tool_calls":true,
		"tools":[{"type":"tool_search","execution":"client"}],
		"input":[{"role":"user","content":"hi"}]
	}`)
	_, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{})
	if err == nil {
		t.Fatal("expected parallel_tool_calls reject")
	}
	if re, ok := compat.AsRequestError(err); !ok || !strings.Contains(re.Param, "parallel") {
		// May be wrapped
		if !strings.Contains(err.Error(), "parallel") {
			t.Fatalf("err=%v", err)
		}
	}
}

func stringValueAnyLocal(v any) string {
	s, _ := v.(string)
	return s
}
