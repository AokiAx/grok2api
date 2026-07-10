package compat_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestAnthropicToOpenAIConvertsToolsAndToolResults(t *testing.T) {
	payload := []byte(`{
		"model":"grok-4.5",
		"system":[{"type":"text","text":"be brief"}],
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Tokyo"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"{\"temp\":28}"}]}
		],
		"tools":[{"name":"weather","description":"Get weather","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any"},
		"max_tokens":64
	}`)

	converted, stream, err := compat.AnthropicToOpenAI(payload, "grok-default")
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	if stream {
		t.Fatal("stream should be false")
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body["model"] != "grok-4.5" || body["tool_choice"] != "required" {
		t.Fatalf("body = %#v", body)
	}
	messages := body["messages"].([]any)
	if len(messages) != 3 || messages[1].(map[string]any)["tool_calls"] == nil || messages[2].(map[string]any)["role"] != "tool" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestOpenAIToAnthropicConvertsToolCallsAndUsage(t *testing.T) {
	converted, err := compat.OpenAIToAnthropic([]byte(`{
		"model":"grok-4.5",
		"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Tokyo\"}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}
	}`))
	if err != nil {
		t.Fatalf("convert response: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["stop_reason"] != "tool_use" {
		t.Fatalf("body = %#v", body)
	}
	content := body["content"].([]any)
	if content[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("content = %#v", content)
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicStreamConvertsTextToolsAndStopsOnce(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Tokyo\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	stream := compat.NewAnthropicStream(io.NopCloser(strings.NewReader(input)), "grok-4.5")
	output, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close converted stream: %v", err)
	}
	text := string(output)
	for _, event := range []string{"event: message_start", "event: content_block_delta", "event: content_block_start", "event: message_stop"} {
		if !strings.Contains(text, event) {
			t.Fatalf("missing %q in %s", event, text)
		}
	}
	if strings.Count(text, "event: message_stop") != 1 {
		t.Fatalf("message_stop count in %s", text)
	}
}

func TestAnthropicToOpenAICoversContentAndChoiceVariants(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		check   func(t *testing.T, body map[string]any)
	}{
		{
			name: "default model and mixed user content",
			payload: `{"system":"rules","messages":[{"role":"user","content":["hello",{"type":"text","text":" world"},{"type":"image","source":"x"}]}],` +
				`"temperature":0.2,"top_p":0.8,"stop_sequences":["STOP"]}`,
			check: func(t *testing.T, body map[string]any) {
				t.Helper()
				if body["model"] != "grok-default" || body["temperature"] != 0.2 || body["stop"] == nil {
					t.Fatalf("body = %#v", body)
				}
			},
		},
		{
			name: "named tool choice and openai tool",
			payload: `{"messages":[],"tools":[{"type":"function","function":{"name":"ready"}},{"name":""}],` +
				`"tool_choice":{"type":"tool","name":"ready"}}`,
			check: func(t *testing.T, body map[string]any) {
				t.Helper()
				choice := body["tool_choice"].(map[string]any)
				if choice["type"] != "function" || len(body["tools"].([]any)) != 1 {
					t.Fatalf("body = %#v", body)
				}
			},
		},
		{
			name:    "assistant text and tool result variants",
			payload: `{"messages":[{"role":"assistant","content":[{"type":"text","text":"thinking"}]},{"role":"user","content":[{"type":"tool_result","id":"call","content":[{"type":"text","text":"ok"}]}]},{"role":"user","content":[]}],"tool_choice":{"type":"auto"}}`,
			check: func(t *testing.T, body map[string]any) {
				t.Helper()
				messages := body["messages"].([]any)
				if len(messages) != 3 || body["tool_choice"] != "auto" {
					t.Fatalf("body = %#v", body)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converted, _, err := compat.AnthropicToOpenAI([]byte(test.payload), "grok-default")
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(converted, &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			test.check(t, body)
		})
	}
	if _, _, err := compat.AnthropicToOpenAI([]byte(`{invalid}`), "grok"); err == nil {
		t.Fatal("invalid JSON should fail")
	}
}

func TestOpenAIToAnthropicCoversEmptyLengthAndInvalidArguments(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantReason string
		wantType   string
	}{
		{
			name:       "empty completion",
			payload:    `{}`,
			wantReason: "end_turn",
			wantType:   "text",
		},
		{
			name: "length and invalid arguments",
			payload: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"x","arguments":"not-json"}}]},` +
				`"finish_reason":"length"}]}`,
			wantReason: "max_tokens",
			wantType:   "tool_use",
		},
		{
			name: "scalar tool arguments",
			payload: `{"choices":[{"message":{"tool_calls":[{"id":"call","function":{"name":"x","arguments":"[1,2]"}}]},` +
				`"finish_reason":"tool_calls"}]}`,
			wantReason: "tool_use",
			wantType:   "tool_use",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converted, err := compat.OpenAIToAnthropic([]byte(test.payload))
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(converted, &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["stop_reason"] != test.wantReason {
				t.Fatalf("body = %#v", body)
			}
			content := body["content"].([]any)
			if content[0].(map[string]any)["type"] != test.wantType {
				t.Fatalf("content = %#v", content)
			}
		})
	}
	if _, err := compat.OpenAIToAnthropic([]byte(`{invalid}`)); err == nil {
		t.Fatal("invalid JSON should fail")
	}
}

func TestAnthropicStreamFinishesAtEOFAndIgnoresInvalidEvents(t *testing.T) {
	input := "event: ignored\n\ndata: not-json\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"
	stream := compat.NewAnthropicStream(io.NopCloser(strings.NewReader(input)), "grok")
	output, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(output)
	if !strings.Contains(text, `"stop_reason":"max_tokens"`) || strings.Count(text, "event: message_stop") != 1 {
		t.Fatalf("output = %s", text)
	}
}
