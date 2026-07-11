package compat_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestAnthropicToResponses_ToolUseSignatureAndStrippedFields(t *testing.T) {
	raw := []byte(`{
		"model": "claude-sonnet-4",
		"max_tokens": 1024,
		"system": "You are helpful.",
		"metadata": {"user_id":"claude-code"},
		"top_k": 40,
		"stop_sequences": ["END"],
		"messages": [
			{"role":"user","content":"what time?"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"private","signature":"opaque-anthropic-signature"},
				{"type":"redacted_thinking","data":"opaque-redacted-thinking"},
				{"type":"tool_use","id":"toolu_1","name":"get_time","input":{"tz":"UTC"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"12:00"}]}
		],
		"tools": [{"name":"get_time","description":"get time","input_schema":{"type":"object","$schema":"http://json-schema.org/draft-07/schema#","properties":{"tz":{"type":"string"}}}}],
		"stream": true,
		"thinking": {"type":"enabled","budget_tokens":1024}
	}`)

	body, model, stream, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	// Returned model is the client-facing id (for response echo).
	if model != "claude-sonnet-4" {
		t.Fatalf("client model=%q", model)
	}
	if !stream {
		t.Fatal("expected stream")
	}

	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	// No model alias rewrite — client model is forwarded as-is.
	if out["model"] != "claude-sonnet-4" {
		t.Fatalf("model=%v", out["model"])
	}
	if out["instructions"] != "You are helpful." {
		t.Fatalf("instructions=%v", out["instructions"])
	}
	if out["max_output_tokens"].(float64) != 1024 {
		t.Fatalf("max_output_tokens=%v", out["max_output_tokens"])
	}
	for _, banned := range []string{"metadata", "top_k", "stop_sequences", "thinking", "stop"} {
		if _, ok := out[banned]; ok {
			t.Fatalf("%s must not reach Responses: %s", banned, body)
		}
	}
	if bytes.Contains(body, []byte("opaque-redacted-thinking")) {
		t.Fatalf("redacted thinking leaked: %s", body)
	}
	if bytes.Contains(body, []byte("$schema")) {
		t.Fatalf("$schema must be stripped: %s", body)
	}

	reasoning, _ := out["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("reasoning=%v", reasoning)
	}
	include, _ := out["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include=%v", include)
	}

	input, ok := out["input"].([]any)
	if !ok || len(input) < 3 {
		t.Fatalf("input len=%d body=%s", len(input), body)
	}

	// Find reasoning replay + function_call + function_call_output.
	var sawReasoning, sawCall, sawOutput bool
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		switch item["type"] {
		case "reasoning":
			sawReasoning = true
			if item["encrypted_content"] != "opaque-anthropic-signature" {
				t.Fatalf("replay=%v", item)
			}
		case "function_call":
			sawCall = true
			if item["call_id"] != "toolu_1" || item["name"] != "get_time" {
				t.Fatalf("fc=%v", item)
			}
		case "function_call_output":
			sawOutput = true
			if item["call_id"] != "toolu_1" || item["output"] != "12:00" {
				t.Fatalf("fo=%v", item)
			}
		}
	}
	if !sawReasoning || !sawCall || !sawOutput {
		t.Fatalf("missing items reasoning=%v call=%v output=%v body=%s", sawReasoning, sawCall, sawOutput, body)
	}

	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "get_time" {
		t.Fatalf("tool=%v", tool)
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested function leaked: %v", tool)
	}
}

func TestAnthropicToResponses_ServerWebSearch(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-5",
		"max_tokens":512,
		"messages":[{"role":"user","content":"Search current information."}],
		"tools":[{
			"type":"web_search_20260318",
			"name":"web_search",
			"max_uses":3,
			"allowed_domains":["go.dev"]
		}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice any              `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0]["type"] != "web_search" {
		t.Fatalf("tools=%v body=%s", out.Tools, body)
	}
	if _, ok := out.Tools[0]["name"]; ok {
		t.Fatalf("server web search became client function: %v", out.Tools[0])
	}
	if out.ToolChoice != "auto" {
		t.Fatalf("tool_choice=%v want auto", out.ToolChoice)
	}
}

func TestAnthropicToResponses_ImageBlock(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"max_tokens":64,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc123"}}
		]}]
	}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"type":"input_image"`)) {
		t.Fatalf("expected input_image: %s", body)
	}
	if !bytes.Contains(body, []byte("data:image/png;base64,abc123")) {
		t.Fatalf("expected data url: %s", body)
	}
}

func TestPrepareResponsesFromAnthropic_DirectPath(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"Inspect","description":"d","input_schema":{"type":"object","properties":{}}}],
		"thinking":{"type":"enabled","budget_tokens":2000}
	}`)
	body, model, _, err := compat.PrepareResponsesFromAnthropic(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if model != "grok-4.5" {
		t.Fatalf("model=%q", model)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "Inspect" {
		t.Fatalf("name=%#v", tool["name"])
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested function: %s", body)
	}
	include, _ := payload["include"].([]any)
	if len(include) != 1 {
		t.Fatalf("include=%v", include)
	}
}

func TestResponsesToAnthropic_FunctionCallAndThinking(t *testing.T) {
	raw := []byte(`{
		"id":"resp_abc123",
		"model":"grok-4.5",
		"status":"completed",
		"output":[
			{
				"type":"reasoning",
				"id":"rs_1",
				"summary":[{"type":"summary_text","text":"I should inspect."}],
				"encrypted_content":"enc_reasoning_1"
			},
			{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"output_text","text":"Let me check."}]
			},
			{
				"type":"function_call",
				"call_id":"call_xyz",
				"name":"get_time",
				"arguments":"{\"tz\":\"UTC\"}"
			}
		],
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	msgRaw, err := compat.ResponsesToAnthropic(raw, "claude-sonnet-4", true, "summarized")
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["model"] != "claude-sonnet-4" {
		t.Fatalf("model=%v", msg["model"])
	}
	if !strings.HasPrefix(stringValueAny(msg["id"]), "msg_") {
		t.Fatalf("id=%v", msg["id"])
	}
	if msg["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v", msg["stop_reason"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content=%v", content)
	}
	thinking := content[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["thinking"] != "I should inspect." || thinking["signature"] != "enc_reasoning_1" {
		t.Fatalf("thinking=%v", thinking)
	}
	text := content[1].(map[string]any)
	if text["type"] != "text" || text["text"] != "Let me check." {
		t.Fatalf("text=%v", text)
	}
	tool := content[2].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_xyz" {
		t.Fatalf("tool=%v", tool)
	}
}

func TestResponsesToAnthropic_OmittedThinkingKeepsSignature(t *testing.T) {
	raw := []byte(`{
		"id":"resp_thinking",
		"output":[{
			"type":"reasoning",
			"summary":[{"type":"summary_text","text":"hidden summary"}],
			"encrypted_content":"enc_reasoning_2"
		}]
	}`)
	msgRaw, err := compat.ResponsesToAnthropic(raw, "grok-4.5", true, "omitted")
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "thinking" || block["thinking"] != "" || block["signature"] != "enc_reasoning_2" {
		t.Fatalf("block=%v", block)
	}
}

func TestResponsesToAnthropicStream_TextAndTool(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_s1","model":"grok-4.5"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"Hel"}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		``,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc1","call_id":"call_1","name":"get_time","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc1","call_id":"call_1","name":"get_time","delta":"{\"tz\":"}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc1","call_id":"call_1","name":"get_time","delta":"\"UTC\"}"}`,
		``,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc1","call_id":"call_1","name":"get_time","arguments":"{\"tz\":\"UTC\"}"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_s1","model":"grok-4.5","status":"completed","usage":{"input_tokens":3,"output_tokens":2},"output":[]}}`,
		``,
	}, "\n")

	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(upstreamSSE)), "claude-sonnet-4", false, "")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"event: message_start",
		`"model":"claude-sonnet-4"`,
		"event: content_block_delta",
		`"type":"text_delta"`,
		`"text":"Hel"`,
		`"text":"lo"`,
		`"type":"tool_use"`,
		`"partial_json"`,
		"event: message_delta",
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestResponsesToAnthropicStream_ThinkingSignature(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_t1","model":"grok-4.5"}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"plan"}`,
		``,
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"enc_sig"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"done"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_t1","status":"completed","usage":{"input_tokens":1,"output_tokens":1},"output":[]}}`,
		``,
	}, "\n")

	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(upstreamSSE)), "grok-4.5", true, "summarized")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`"type":"thinking"`,
		`"type":"thinking_delta"`,
		`"thinking":"plan"`,
		`"type":"signature_delta"`,
		`"signature":"enc_sig"`,
		`"type":"text_delta"`,
		"event: message_stop",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestResponsesToAnthropicStream_FailedNoMessageStop(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_e1"}}`,
		``,
		`data: {"type":"response.failed","response":{"error":{"message":"boom"}}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(upstreamSSE)), "grok-4.5", false, "")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `event: error`) {
		t.Fatalf("expected error event: %s", text)
	}
	if strings.Contains(text, `event: message_stop`) {
		t.Fatalf("must not emit message_stop after failure: %s", text)
	}
}

func TestAnthropicThinkingBridge(t *testing.T) {
	enabled, display := compat.AnthropicThinkingBridge([]byte(`{"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"}}`))
	if !enabled || display != "omitted" {
		t.Fatalf("enabled=%v display=%q", enabled, display)
	}
	enabled, _ = compat.AnthropicThinkingBridge([]byte(`{"messages":[]}`))
	if enabled {
		t.Fatal("expected disabled")
	}
}

func stringValueAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
