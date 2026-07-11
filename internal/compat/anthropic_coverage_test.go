package compat_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestAnthropicToResponses_ThinkingEffortAndToolChoice(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"max_tokens":8000,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"foo","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"foo"},
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh","format":{"type":"json_schema","schema":{"type":"object"}}}
	}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := out["reasoning"].(map[string]any)
	// xhigh on grok-4.5 clamps to high
	if reasoning["effort"] != "high" {
		t.Fatalf("effort=%v body=%s", reasoning, body)
	}
	include, _ := out["include"].([]any)
	if len(include) != 1 {
		t.Fatalf("include=%v", include)
	}
	tc, _ := out["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["name"] != "foo" {
		t.Fatalf("tool_choice=%v", out["tool_choice"])
	}
	text, _ := out["text"].(map[string]any)
	if text == nil {
		t.Fatalf("missing text.format: %s", body)
	}
}

func TestAnthropicToResponses_DisabledThinking(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"disabled"}
	}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := out["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("disabled maps to low on grok-4.5: %v", reasoning)
	}
}

func TestResponsesToAnthropicStream_CompletedWithOutput(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_f1"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_f1","status":"completed","usage":{"input_tokens":1,"output_tokens":2},"output":[{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"sig1"},{"type":"message","id":"m1","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"c1","name":"tool","arguments":"{}"}]}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(upstreamSSE)), "grok-4.5", true, "summarized")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"type":"thinking"`, `"signature":"sig1"`, `"text":"hello"`, `"type":"tool_use"`, "message_stop"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestResponsesToAnthropic_Incomplete(t *testing.T) {
	raw := []byte(`{"id":"resp_x","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"cut"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	out, err := compat.ResponsesToAnthropic(raw, "grok-4.5", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"stop_reason":"max_tokens"`) {
		t.Fatalf("body=%s", out)
	}
}

func TestAggregateResponsesToAnthropicVariants(t *testing.T) {
	// Bare JSON body
	raw := []byte(`{"id":"resp_1","status":"completed","output_text":"pong","usage":{"input_tokens":1,"output_tokens":1}}`)
	out, err := compat.AggregateResponsesToAnthropic(io.NopCloser(bytes.NewReader(raw)), "grok-4.5", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "pong") {
		t.Fatalf("%s", out)
	}
	// SSE with completed
	sse := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"status\":\"completed\",\"output_text\":\"ok\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	out, err = compat.AggregateResponsesToAnthropic(io.NopCloser(strings.NewReader(sse)), "grok-4.5", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("%s", out)
	}
	// SSE deltas only (synthetic fallback)
	sse2 := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"
	out, err = compat.AggregateResponsesToAnthropic(io.NopCloser(strings.NewReader(sse2)), "grok-4.5", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "x") {
		t.Fatalf("%s", out)
	}
}

func TestAnthropicToResponses_SystemBlocksAndChoices(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"max_tokens":128,
		"system":[{"type":"text","text":"sys-a"},{"type":"text","text":"sys-b"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"tools":[{"type":"function","function":{"name":"A","description":"d","parameters":{"type":"object","$schema":"x"}}}],
		"tool_choice":"any"
	}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sys-a") || !strings.Contains(string(body), "sys-b") {
		t.Fatalf("system blocks: %s", body)
	}
	if !strings.Contains(string(body), `"tool_choice":"required"`) {
		t.Fatalf("any->required: %s", body)
	}
	if strings.Contains(string(body), "$schema") {
		t.Fatalf("$schema leaked: %s", body)
	}
}

func TestAnthropicToResponses_BudgetTiers(t *testing.T) {
	for _, tc := range []struct {
		budget int
		effort string
	}{
		{1500, "low"},
		{8000, "medium"},
		{20000, "high"},
	} {
		raw := []byte(fmt.Sprintf(`{"model":"grok-4.5","max_tokens":50000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":%d}}`, tc.budget))
		body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
		if err != nil {
			t.Fatalf("budget %d: %v", tc.budget, err)
		}
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		r, _ := out["reasoning"].(map[string]any)
		if r["effort"] != tc.effort {
			t.Fatalf("budget %d effort=%v want %s", tc.budget, r["effort"], tc.effort)
		}
	}
}

func TestResponsesToAnthropicStream_IncompleteAndDone(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_i"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``,
		`data: {"type":"response.incomplete","response":{"id":"resp_i","status":"incomplete","usage":{"input_tokens":1,"output_tokens":1},"output":[]}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5", false, "")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"stop_reason":"max_tokens"`) {
		t.Fatalf("%s", data)
	}
	// [DONE] terminal
	sse2 := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_d\"}}\n\ndata: [DONE]\n\n"
	stream = compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(sse2)), "grok-4.5", false, "")
	data, err = io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "message_stop") {
		t.Fatalf("%s", data)
	}
}

func TestResponsesToAnthropicStream_ManyEvents(t *testing.T) {
	// Hit as many processData branches as practical in one stream.
	lines := []string{
		`data: {"type":"response.in_progress","response":{"id":"resp_many","model":"grok-4.5","usage":{"input_tokens":9}}}`,
		``,
		`data: {"type":"response.reasoning_summary_part.added","item_id":"rs_a","output_index":0}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_a","delta":"think1"}`,
		``,
		`data: {"type":"response.reasoning_summary_part.done","item_id":"rs_a"}`,
		``,
		`data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_a","encrypted_content":"encA"}}`,
		``,
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_a","summary":[{"type":"summary_text","text":"think1"}],"encrypted_content":"encA"}}`,
		``,
		`data: {"type":"response.content_part.added","item_id":"m1","part":{"type":"output_text"}}`,
		``,
		`data: {"type":"response.output_text.delta","item_id":"m1","delta":"Hello"}`,
		``,
		`data: {"type":"response.content_part.done","item_id":"m1","part":{"type":"output_text","text":"Hello"}}`,
		``,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc2","call_id":"call_2","name":"bar","arguments":"{"}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc2","call_id":"call_2","name":"bar","delta":"}"}`,
		``,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc2","call_id":"call_2","name":"bar","arguments":"{}"}}`,
		``,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"m1","content":[{"type":"output_text","text":"Hello"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_many","status":"completed","usage":{"input_tokens":9,"output_tokens":3},"output":[]}}`,
		``,
	}
	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(strings.Join(lines, "\n"))), "grok-4.5", true, "summarized")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"thinking_delta", "signature_delta", "text_delta", "tool_use", "message_stop"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestResponsesToAnthropicStream_OmittedThinking(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_o"}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_o","delta":"secret"}`,
		``,
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_o","summary":[{"type":"summary_text","text":"secret"}],"encrypted_content":"encO"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_o","status":"completed","usage":{"input_tokens":1,"output_tokens":1},"output":[]}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5", true, "omitted")
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `"thinking":"secret"`) {
		t.Fatalf("omitted should hide summary: %s", text)
	}
	if !strings.Contains(text, "signature_delta") {
		t.Fatalf("want signature: %s", text)
	}
}

func TestAnthropicToResponses_ComposerDisabledThinking(t *testing.T) {
	raw := []byte(`{"model":"grok-composer-2.5-fast","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if _, ok := out["reasoning"]; ok {
		t.Fatalf("non-reasoning model should skip reasoning: %s", body)
	}
}

func TestAnthropicToResponses_Grok43Disabled(t *testing.T) {
	raw := []byte(`{"model":"grok-4.3","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	r, _ := out["reasoning"].(map[string]any)
	if r["effort"] != "none" {
		t.Fatalf("4.3 disabled => none: %v", r)
	}
}

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, fmt.Errorf("boom") }
func (errReadCloser) Close() error             { return nil }

func TestResponsesToAnthropicStream_EmptyAndError(t *testing.T) {
	stream := compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader("")), "grok-4.5", false, "")
	data, _ := io.ReadAll(stream)
	_ = stream.Close()
	if !strings.Contains(string(data), "error") {
		t.Fatalf("empty stream want error event: %s", data)
	}
	stream = compat.NewResponsesToAnthropicStream(errReadCloser{}, "grok-4.5", false, "")
	data, _ = io.ReadAll(stream)
	_ = stream.Close()
	// may be empty or error depending on timing
	_ = data

	// Truncated after start (no terminal)
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_t\"}}\n\n"
	stream = compat.NewResponsesToAnthropicStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5", false, "")
	data, _ = io.ReadAll(stream)
	_ = stream.Close()
	if !strings.Contains(string(data), "error") {
		t.Fatalf("truncated want error: %s", data)
	}
}

func TestAnthropicToolChoiceVariants(t *testing.T) {
	for _, tc := range []struct {
		choice string
		want   string
	}{
		{`"auto"`, `"tool_choice":"auto"`},
		{`"none"`, `"tool_choice":"none"`},
		{`{"type":"auto"}`, `"tool_choice":"auto"`},
		{`{"type":"none"}`, `"tool_choice":"none"`},
		{`{"type":"any"}`, `"tool_choice":"required"`},
	} {
		raw := []byte(fmt.Sprintf(`{"model":"grok-4.5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"x","input_schema":{"type":"object"}}],"tool_choice":%s}`, tc.choice))
		body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
		if err != nil {
			t.Fatalf("%s: %v", tc.choice, err)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Fatalf("choice %s body=%s want %s", tc.choice, body, tc.want)
		}
	}
}

func TestAnthropicSystemStringAndImageURL(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"max_tokens":32,
		"system":"plain-system",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}},
			{"type":"text","text":"see"}
		]}]
	}`)
	body, _, _, err := compat.AnthropicToResponses(raw, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "plain-system") {
		t.Fatalf("system: %s", body)
	}
	if !strings.Contains(string(body), "input_image") || !strings.Contains(string(body), "https://example.test/a.png") {
		t.Fatalf("image: %s", body)
	}
}

func TestResponsesToAnthropic_EnvelopeAndNoThinking(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_e","model":"grok-4.5","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"x"}],"encrypted_content":"e"},{"type":"message","content":[{"type":"output_text","text":"y"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	out, err := compat.ResponsesToAnthropic(raw, "req-model", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "thinking") {
		t.Fatalf("thinking disabled should drop reasoning: %s", out)
	}
	if !strings.Contains(string(out), `"model":"req-model"`) || !strings.Contains(string(out), "y") {
		t.Fatalf("%s", out)
	}
}
