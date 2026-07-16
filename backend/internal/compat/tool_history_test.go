package compat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/compat"
)

func TestAgentMessageOpaqueRedacted(t *testing.T) {
	raw := []any{
		map[string]any{
			"type": "agent_message",
			"content": []any{
				map[string]any{"type": "encrypted_text", "encrypted_content": "secret"},
			},
		},
	}
	out := compat.SanitizeResponsesInput(raw).([]any)
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
	msg := out[0].(map[string]any)
	content := stringifyLocal(msg["content"])
	if !strings.Contains(content, "not portable") {
		t.Fatalf("content=%v", msg["content"])
	}
}

func TestAgentMessageVisiblePreserved(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":      "agent_message",
			"author":    "planner",
			"recipient": "worker",
			"content":   []any{map[string]any{"type": "input_text", "text": "do the thing"}},
		},
	}
	out := compat.SanitizeResponsesInput(raw).([]any)
	msg := out[0].(map[string]any)
	content := stringifyLocal(msg["content"])
	if !strings.Contains(content, "planner") || !strings.Contains(content, "do the thing") {
		t.Fatalf("content=%v", msg["content"])
	}
}

func TestCompactionTriggerBoundary(t *testing.T) {
	raw := []any{map[string]any{"type": "compaction_trigger"}}
	out := compat.SanitizeResponsesInput(raw).([]any)
	msg := out[0].(map[string]any)
	content := stringifyLocal(msg["content"])
	if !strings.Contains(content, "compaction") {
		t.Fatalf("content=%v", msg["content"])
	}
}

func TestMCPDeferLoadingRequiresClientSearch(t *testing.T) {
	raw := []any{
		map[string]any{"type": "mcp", "server_label": "git", "defer_loading": true},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err == nil {
		t.Fatal("expected error")
	}
}

func TestMCPDeferLoadingWithClientSearch(t *testing.T) {
	raw := []any{
		map[string]any{"type": "tool_search", "execution": "client"},
		map[string]any{"type": "mcp", "server_label": "git", "description": "Git MCP", "defer_loading": true},
		map[string]any{"type": "mcp", "server_label": "live", "server_url": "https://example.test"},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err != nil {
		t.Fatalf("normalize: %v", result.Err)
	}
	var sawLive, sawGit, sawSearch bool
	for _, rawTool := range result.Tools {
		tool := rawTool.(map[string]any)
		switch tool["type"] {
		case "mcp":
			if tool["server_label"] == "live" {
				sawLive = true
			}
			if tool["server_label"] == "git" {
				sawGit = true
			}
		case "function":
			if tool["name"] == "grok2api_tool_search" {
				sawSearch = true
				desc, _ := tool["description"].(string)
				if !strings.Contains(desc, "git") {
					t.Fatalf("search desc missing git surface: %q", desc)
				}
			}
		}
	}
	if sawGit {
		t.Fatal("deferred MCP should not be forwarded")
	}
	if !sawLive || !sawSearch {
		t.Fatalf("live=%v search=%v tools=%#v", sawLive, sawSearch, result.Tools)
	}
}

func TestComputerUsePreviewRejected(t *testing.T) {
	raw := []any{map[string]any{"type": "computer_use_preview"}}
	result := compat.NormalizeResponsesToolsDetailed(raw, 8)
	if result.Err == nil {
		t.Fatal("expected reject")
	}
}

func TestUnknownToolTypeRejected(t *testing.T) {
	raw := []any{map[string]any{"type": "totally_unknown", "name": "x"}}
	result := compat.NormalizeResponsesToolsDetailed(raw, 8)
	if result.Err == nil {
		t.Fatal("expected reject for unknown type")
	}
}

func TestVisibleToolsRestoredOnResponse(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"stream":false,
		"input":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"namespace","name":"demo","tools":[{"type":"function","name":"inner","parameters":{"type":"object"}}]},
			{"type":"function","name":"outer","parameters":{"type":"object"}}
		]
	}`)
	finalized, warnings, toolCompat, err := compat.FinalizeResponsesUpstreamDetailed(body, compat.ModelHints{})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	_ = warnings
	var upstream map[string]any
	if err := json.Unmarshal(finalized, &upstream); err != nil {
		t.Fatalf("decode upstream: %v", err)
	}
	// Upstream tools should be flattened aliases.
	upTools := upstream["tools"].([]any)
	if len(upTools) != 2 {
		t.Fatalf("upstream tools=%#v", upTools)
	}
	// Simulate upstream response echoing rewritten tools.
	resp := map[string]any{
		"id":     "resp_1",
		"object": "response",
		"status": "completed",
		"tools":  upTools,
		"output": []any{},
	}
	encoded, _ := json.Marshal(resp)
	restored, err := toolCompat.RewriteResponseJSON(encoded)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(restored, &out); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	clientTools := out["tools"].([]any)
	if len(clientTools) != 2 {
		t.Fatalf("restored tools=%#v", clientTools)
	}
	first := clientTools[0].(map[string]any)
	if first["type"] != "namespace" {
		t.Fatalf("expected original namespace tool, got %#v", first)
	}
}

func stringifyLocal(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	default:
		b, _ := json.Marshal(typed)
		return string(b)
	}
}
