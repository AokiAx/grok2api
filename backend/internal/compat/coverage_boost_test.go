package compat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/compat"
)

func TestSanitizeApplyPatchAndMCPHistory(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":    "apply_patch_call",
			"call_id": "ap1",
			"operation": map[string]any{
				"type": "update_file",
				"path": "a.go",
				"diff": "+x",
			},
		},
		map[string]any{
			"type":    "apply_patch_call_output",
			"call_id": "ap1",
			"status":  "completed",
			"output":  "ok",
		},
		map[string]any{
			"type":    "mcp_tool_call_output",
			"call_id": "m1",
			"output":  map[string]any{"result": 1},
		},
		map[string]any{
			"type":    "local_shell_call",
			"call_id": "s1",
			"action":  map[string]any{"type": "exec", "command": []any{"echo", "hi"}},
		},
		map[string]any{
			"type":    "local_shell_call_output",
			"call_id": "s1",
			"output":  "hi\n",
		},
	}
	out := compat.SanitizeResponsesInput(raw).([]any)
	if len(out) < 4 {
		t.Fatalf("len=%d %#v", len(out), out)
	}
	// apply_patch becomes function_call with registered alias
	fc := out[0].(map[string]any)
	if fc["type"] != "function_call" || !strings.Contains(strAny(fc["name"]), "apply_patch") {
		t.Fatalf("apply_patch call=%#v", fc)
	}
	fo := out[1].(map[string]any)
	if fo["type"] != "function_call_output" || fo["call_id"] != "ap1" {
		t.Fatalf("apply_patch out=%#v", fo)
	}
	// mcp orphan output becomes developer/system message
	mcp := out[2].(map[string]any)
	if mcp["role"] != "system" && mcp["role"] != "developer" {
		// sanitizeMessageItem maps developer→system
		t.Fatalf("mcp role=%#v", mcp)
	}
}

func TestAlignToolChoiceApplyPatchAndToolSearch(t *testing.T) {
	raw := []any{
		map[string]any{"type": "tool_search", "execution": "client"},
		map[string]any{"type": "apply_patch"},
		map[string]any{"type": "function", "name": "Read", "parameters": map[string]any{"type": "object"}},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 32)
	if result.Err != nil || result.Compat == nil {
		t.Fatalf("normalize: %v", result.Err)
	}
	tools := result.Tools
	aligned, _ := result.Compat.AlignToolChoice(map[string]any{"type": "apply_patch"}, tools)
	obj, _ := aligned.(map[string]any)
	if obj["type"] != "function" || !strings.Contains(strAny(obj["name"]), "apply_patch") {
		t.Fatalf("apply_patch choice=%#v", aligned)
	}
	aligned2, _ := result.Compat.AlignToolChoice(map[string]any{"type": "tool_search"}, tools)
	obj2, _ := aligned2.(map[string]any)
	if obj2["type"] != "function" || !strings.Contains(strAny(obj2["name"]), "tool_search") {
		t.Fatalf("tool_search choice=%#v", aligned2)
	}
	// Hosted web_search when bare tool present → required
	withWeb := append([]any{map[string]any{"type": "web_search"}}, tools...)
	aligned3, warns := compat.AlignResponsesToolChoice(map[string]any{"type": "web_search"}, withWeb, false)
	if aligned3 != "required" {
		t.Fatalf("web_search choice=%#v warns=%v", aligned3, warns)
	}
}

func TestShellWithoutEnvBecomesFunction(t *testing.T) {
	raw := []any{map[string]any{"type": "shell", "description": "run"}}
	result := compat.NormalizeResponsesToolsDetailed(raw, 8)
	if result.Err != nil {
		t.Fatalf("err=%v", result.Err)
	}
	tool := result.Tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "shell_command" {
		t.Fatalf("tool=%#v", tool)
	}
}

func TestCustomFreeformToolParameters(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":        "custom",
			"name":        "fmt",
			"description": "format",
			"format":      map[string]any{"type": "text", "syntax": "diff"},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 8)
	if result.Err != nil {
		t.Fatalf("err=%v", result.Err)
	}
	tool := result.Tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "fmt" {
		t.Fatalf("tool=%#v", tool)
	}
	params, _ := tool["parameters"].(map[string]any)
	if params == nil {
		t.Fatalf("missing parameters: %#v", tool)
	}
}

func TestEncodeFunctionArgumentsViaToolSearchCall(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":      "tool_search_call",
			"call_id":   "c9",
			"arguments": map[string]any{"query": "x"},
		},
	}
	out := compat.SanitizeResponsesInput(raw).([]any)
	fc := out[0].(map[string]any)
	args, _ := fc["arguments"].(string)
	if !strings.Contains(args, "query") {
		t.Fatalf("args=%q", args)
	}
}

func TestShouldInjectDefaultSearchToolsViaFinalize(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"hi"}],"stream":false}`)
	out, err := compat.FinalizeResponsesUpstream(body, compat.ModelHints{SupportsBackendSearch: true})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["backend_search"] != true {
		t.Fatalf("backend_search=%#v", payload["backend_search"])
	}
	tools, _ := payload["tools"].([]any)
	have := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		have[strAny(tool["type"])] = true
	}
	if !have["web_search"] || !have["x_search"] {
		t.Fatalf("tools=%#v", tools)
	}
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}
