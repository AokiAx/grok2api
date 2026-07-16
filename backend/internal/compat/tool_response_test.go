package compat_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/compat"
)

func TestNamespaceAliasRoundTripOnResponseJSON(t *testing.T) {
	rawTools := []any{
		map[string]any{
			"type": "namespace",
			"name": "demo",
			"tools": []any{
				map[string]any{
					"type":       "function",
					"name":       "inner",
					"parameters": map[string]any{"type": "object"},
				},
			},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(rawTools, 16)
	if result.Err != nil {
		t.Fatalf("normalize: %v", result.Err)
	}
	if result.Compat == nil || !result.Compat.HasRewrites() {
		t.Fatal("expected rewrite state for namespace tools")
	}
	tool := result.Tools[0].(map[string]any)
	if tool["name"] != "demo__inner" {
		t.Fatalf("upstream name=%#v", tool["name"])
	}

	upstream := []byte(`{
		"id":"resp_1",
		"status":"completed",
		"output":[{"type":"function_call","call_id":"c1","name":"demo__inner","arguments":"{}"}]
	}`)
	rewritten, err := result.Compat.RewriteResponseJSON(upstream)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(rewritten, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	item := body["output"].([]any)[0].(map[string]any)
	if item["name"] != "inner" {
		t.Fatalf("restored name=%#v", item["name"])
	}
	if item["namespace"] != "demo" {
		t.Fatalf("restored namespace=%#v", item["namespace"])
	}
}

func TestCustomToolRestoredOnResponseJSON(t *testing.T) {
	rawTools := []any{
		map[string]any{"type": "custom", "name": "apply_patch", "description": "patch"},
	}
	result := compat.NormalizeResponsesToolsDetailed(rawTools, 16)
	if result.Compat == nil {
		t.Fatal("expected compat state")
	}
	upstreamName := result.Tools[0].(map[string]any)["name"].(string)
	upstream := []byte(`{
		"output":[{"type":"function_call","call_id":"c1","name":"` + upstreamName + `","arguments":"{\"input\":\"diff\"}"}]
	}`)
	rewritten, err := result.Compat.RewriteResponseJSON(upstream)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(rewritten, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	item := body["output"].([]any)[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("type=%#v", item["type"])
	}
	if item["name"] != "apply_patch" {
		t.Fatalf("name=%#v", item["name"])
	}
	if item["input"] != "diff" {
		t.Fatalf("input=%#v", item["input"])
	}
}

func TestRewriteResponseStreamRestoresFunctionName(t *testing.T) {
	rawTools := []any{
		map[string]any{
			"type": "namespace",
			"name": "ws",
			"tools": []any{
				map[string]any{"type": "function", "name": "Read", "parameters": map[string]any{"type": "object"}},
			},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(rawTools, 16)
	sse := strings.Join([]string{
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"c1","name":"ws__Read","arguments":"{}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"r1","output":[{"type":"function_call","call_id":"c1","name":"ws__Read","arguments":"{}"}]}}`,
		``,
	}, "\n")
	stream := result.Compat.RewriteResponseStream(io.NopCloser(strings.NewReader(sse)))
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if strings.Contains(body, `"name":"ws__Read"`) {
		t.Fatalf("alias leaked: %s", body)
	}
	if !strings.Contains(body, `"name":"Read"`) || !strings.Contains(body, `"namespace":"ws"`) {
		t.Fatalf("missing restore: %s", body)
	}
}

func TestApplyPatchToolRoundTrip(t *testing.T) {
	rawTools := []any{map[string]any{"type": "apply_patch"}}
	result := compat.NormalizeResponsesToolsDetailed(rawTools, 16)
	if result.Err != nil {
		t.Fatalf("normalize: %v", result.Err)
	}
	tool := result.Tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "grok2api_apply_patch" {
		t.Fatalf("tool=%#v", tool)
	}
	upstream := []byte(`{"output":[{"type":"function_call","call_id":"c1","name":"grok2api_apply_patch","arguments":"{\"operation\":{\"type\":\"update_file\",\"path\":\"a.go\",\"diff\":\"+x\"}}"}]}`)
	rewritten, err := result.Compat.RewriteResponseJSON(upstream)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(rewritten, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	item := body["output"].([]any)[0].(map[string]any)
	if item["type"] != "apply_patch_call" {
		t.Fatalf("type=%#v", item["type"])
	}
	op := item["operation"].(map[string]any)
	if op["type"] != "update_file" || op["path"] != "a.go" {
		t.Fatalf("operation=%#v", op)
	}
}

func TestLocalShellUpgradesToNativeShell(t *testing.T) {
	rawTools := []any{map[string]any{"type": "local_shell"}}
	result := compat.NormalizeResponsesToolsDetailed(rawTools, 16)
	if result.Err != nil {
		t.Fatalf("normalize: %v", result.Err)
	}
	tool := result.Tools[0].(map[string]any)
	if tool["type"] != "shell" {
		t.Fatalf("type=%#v want shell", tool["type"])
	}
	env, _ := tool["environment"].(map[string]any)
	if env["type"] != "local" {
		t.Fatalf("environment=%#v", env)
	}
	// Response shell_call → local_shell_call when upgraded from legacy local_shell.
	upstream := []byte(`{"output":[{"type":"shell_call","call_id":"c1","action":{"type":"exec","command":"ls"}}]}`)
	rewritten, err := result.Compat.RewriteResponseJSON(upstream)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !strings.Contains(string(rewritten), `"type":"local_shell_call"`) {
		t.Fatalf("expected local_shell_call restore: %s", rewritten)
	}
}

func TestRequestErrorSurfacesParam(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":    "web_search",
			"filters": map[string]any{"allowed_domains": []any{"example.com"}},
		},
	}
	result := compat.NormalizeResponsesToolsDetailed(raw, 16)
	if result.Err == nil {
		t.Fatal("expected error")
	}
	re, ok := compat.AsRequestError(result.Err)
	if !ok {
		t.Fatalf("want RequestError, got %T %v", result.Err, result.Err)
	}
	if re.Param == "" || re.Code != "unsupported_parameter" {
		t.Fatalf("param=%q code=%q", re.Param, re.Code)
	}
}
