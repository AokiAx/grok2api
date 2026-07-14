package bridge_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/bridge"
	"github.com/AokiAx/grok2api/internal/compat"
	"github.com/AokiAx/grok2api/internal/service"
	"github.com/AokiAx/grok2api/internal/upstream"
)

func TestChatJSONContractFromResponsesCompleted(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_contract","model":"grok-4.5","created_at":123,"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
		``,
	}, "\n")
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"text/event-stream"},
			"Content-Length": []string{"999"},
			"X-Upstream":     []string{"preserved"},
		},
		Stream: io.NopCloser(strings.NewReader(upstreamSSE)),
	}}

	result, err := contractPipeline(gateway).Chat(
		context.Background(),
		[]byte(`{"model":"grok-4.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`),
	)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if result.Header.Get("Content-Type") != "application/json" || result.Header.Get("Content-Length") != "" || result.Header.Get("X-Upstream") != "preserved" {
		t.Fatalf("headers=%v", result.Header)
	}

	var completion struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int    `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(result.Body, &completion); err != nil {
		t.Fatalf("decode chat completion: %v; body=%s", err, result.Body)
	}
	if completion.ID != "resp_contract" || completion.Object != "chat.completion" || completion.Created != 123 || completion.Model != "grok-4.5" {
		t.Fatalf("completion metadata=%#v", completion)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Index != 0 || completion.Choices[0].Message.Role != "assistant" || completion.Choices[0].Message.Content != "hello" || completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices=%#v", completion.Choices)
	}
	if completion.Usage.PromptTokens != 4 || completion.Usage.CompletionTokens != 2 || completion.Usage.TotalTokens != 6 {
		t.Fatalf("usage=%#v", completion.Usage)
	}
}

func TestChatSSETerminationContract(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_stream","model":"grok-4.5"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_stream","status":"completed"}}`,
		``,
	}, "\n")
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Length": []string{"999"}},
		Stream: io.NopCloser(strings.NewReader(upstreamSSE)),
	}}

	result, err := contractPipeline(gateway).Chat(
		context.Background(),
		[]byte(`{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	)
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if result.Header.Get("Content-Type") != "text/event-stream" || result.Header.Get("Content-Length") != "" {
		t.Fatalf("headers=%v", result.Header)
	}
	events := readContractSSE(t, result.Stream)
	if len(events) != 4 {
		t.Fatalf("events=%d, want role, content, finish, DONE: %#v", len(events), events)
	}
	if events[3].Data != "[DONE]" {
		t.Fatalf("terminal event=%#v, want [DONE]", events[3])
	}

	chunks := make([]chatStreamChunk, 3)
	for i := range chunks {
		if err := json.Unmarshal([]byte(events[i].Data), &chunks[i]); err != nil {
			t.Fatalf("decode chat event %d: %v; data=%s", i, err, events[i].Data)
		}
		if chunks[i].Object != "chat.completion.chunk" || chunks[i].Model != "grok-4.5" || len(chunks[i].Choices) != 1 {
			t.Fatalf("chunk %d=%#v", i, chunks[i])
		}
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" || chunks[0].Choices[0].FinishReason != nil {
		t.Fatalf("start chunk=%#v", chunks[0])
	}
	if chunks[1].Choices[0].Delta.Content != "hello" || chunks[1].Choices[0].FinishReason != nil {
		t.Fatalf("content chunk=%#v", chunks[1])
	}
	if chunks[2].Choices[0].Delta.Role != "" || chunks[2].Choices[0].Delta.Content != "" || valueOrEmpty(chunks[2].Choices[0].FinishReason) != "stop" {
		t.Fatalf("finish chunk=%#v", chunks[2])
	}
	if chunks[0].ID == "" || chunks[0].ID != chunks[1].ID || chunks[1].ID != chunks[2].ID {
		t.Fatalf("chunk IDs are not stable: %q %q %q", chunks[0].ID, chunks[1].ID, chunks[2].ID)
	}
}

func TestResponsesJSONAndSSETerminationContracts(t *testing.T) {
	completed := `{"type":"response.completed","response":{"id":"resp_native","object":"response","model":"grok-4.5","status":"completed","output_text":"done"}}`

	t.Run("non-stream extracts completed response object", func(t *testing.T) {
		gateway := &fakeGateway{result: service.ChatResult{
			Status: http.StatusOK,
			Header: http.Header{"Content-Length": []string{"999"}},
			Stream: io.NopCloser(strings.NewReader("event: response.completed\ndata: " + completed + "\n\n")),
		}}
		result, err := contractPipeline(gateway).Responses(context.Background(), []byte(`{"model":"grok-4.5","input":"hi","stream":false}`))
		if err != nil {
			t.Fatalf("responses: %v", err)
		}
		if result.Header.Get("Content-Type") != "application/json" || result.Header.Get("Content-Length") != "" {
			t.Fatalf("headers=%v", result.Header)
		}
		var got map[string]any
		if err := json.Unmarshal(result.Body, &got); err != nil {
			t.Fatalf("decode response body: %v; body=%s", err, result.Body)
		}
		want := map[string]any{
			"id":          "resp_native",
			"object":      "response",
			"model":       "grok-4.5",
			"status":      "completed",
			"output_text": "done",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("response=%#v, want %#v", got, want)
		}
	})

	t.Run("stream passes through response.completed without chat DONE", func(t *testing.T) {
		upstreamSSE := "event: response.completed\ndata: " + completed + "\n\n"
		gateway := &fakeGateway{result: service.ChatResult{
			Status: http.StatusOK,
			Header: http.Header{"Content-Length": []string{"999"}},
			Stream: io.NopCloser(strings.NewReader(upstreamSSE)),
		}}
		result, err := contractPipeline(gateway).Responses(context.Background(), []byte(`{"model":"grok-4.5","input":"hi","stream":true}`))
		if err != nil {
			t.Fatalf("responses stream: %v", err)
		}
		if result.Header.Get("Content-Type") != "text/event-stream" || result.Header.Get("Content-Length") != "" {
			t.Fatalf("headers=%v", result.Header)
		}
		body, readErr := io.ReadAll(result.Stream)
		_ = result.Stream.Close()
		if readErr != nil {
			t.Fatalf("read responses stream: %v", readErr)
		}
		if string(body) != upstreamSSE {
			t.Fatalf("responses stream changed:\ngot:  %q\nwant: %q", body, upstreamSSE)
		}
		if strings.Contains(string(body), "[DONE]") {
			t.Fatalf("native Responses stream gained Chat terminal marker: %s", body)
		}
	})
}

func TestAnthropicJSONAndSSETerminationContracts(t *testing.T) {
	completedResponse := `{"id":"resp_anthropic","model":"grok-4.5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`

	t.Run("non-stream message envelope", func(t *testing.T) {
		gateway := &fakeGateway{result: service.ChatResult{
			Status: http.StatusOK,
			Stream: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completedResponse + "}\n\n")),
		}}
		result, err := contractPipeline(gateway).Messages(
			context.Background(),
			[]byte(`{"model":"claude-contract","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		)
		if err != nil {
			t.Fatalf("messages: %v", err)
		}
		if result.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers=%v", result.Header)
		}
		var message struct {
			ID           string `json:"id"`
			Type         string `json:"type"`
			Role         string `json:"role"`
			Model        string `json:"model"`
			StopReason   string `json:"stop_reason"`
			StopSequence any    `json:"stop_sequence"`
			Content      []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(result.Body, &message); err != nil {
			t.Fatalf("decode Anthropic response: %v; body=%s", err, result.Body)
		}
		if message.ID != "msg_anthropic" || message.Type != "message" || message.Role != "assistant" || message.Model != "claude-contract" || message.StopReason != "end_turn" || message.StopSequence != nil {
			t.Fatalf("message metadata=%#v", message)
		}
		if len(message.Content) != 1 || message.Content[0].Type != "text" || message.Content[0].Text != "hello" {
			t.Fatalf("content=%#v", message.Content)
		}
		if message.Usage.InputTokens != 3 || message.Usage.OutputTokens != 2 {
			t.Fatalf("usage=%#v", message.Usage)
		}
	})

	t.Run("stream ends with message_delta then message_stop", func(t *testing.T) {
		upstreamSSE := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_anthropic","model":"grok-4.5"}}`,
			``,
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			``,
			`data: {"type":"response.completed","response":` + completedResponse + `}`,
			``,
		}, "\n")
		gateway := &fakeGateway{result: service.ChatResult{
			Status: http.StatusOK,
			Header: http.Header{"Content-Length": []string{"999"}},
			Stream: io.NopCloser(strings.NewReader(upstreamSSE)),
		}}
		result, err := contractPipeline(gateway).Messages(
			context.Background(),
			[]byte(`{"model":"claude-contract","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		)
		if err != nil {
			t.Fatalf("messages stream: %v", err)
		}
		if result.Header.Get("Content-Type") != "text/event-stream" || result.Header.Get("Content-Length") != "" {
			t.Fatalf("headers=%v", result.Header)
		}
		events := readContractSSE(t, result.Stream)
		gotNames := make([]string, len(events))
		for i, event := range events {
			gotNames[i] = event.Name
		}
		wantNames := []string{
			"message_start",
			"content_block_start",
			"content_block_delta",
			"content_block_stop",
			"message_delta",
			"message_stop",
		}
		if !reflect.DeepEqual(gotNames, wantNames) {
			t.Fatalf("event names=%#v, want %#v; events=%#v", gotNames, wantNames, events)
		}
		if events[len(events)-2].Type != "message_delta" || stringFromNestedMap(events[len(events)-2].Payload, "delta", "stop_reason") != "end_turn" {
			t.Fatalf("penultimate event=%#v", events[len(events)-2])
		}
		if events[len(events)-1].Type != "message_stop" {
			t.Fatalf("terminal event=%#v", events[len(events)-1])
		}
	})
}

type chatStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
}

func contractPipeline(gateway *fakeGateway) *bridge.Pipeline {
	return &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		DefaultModel:    "grok-4.5",
		PreferResponses: true,
	}
}

func readContractSSE(t *testing.T, stream io.ReadCloser) []compat.SSEEvent {
	t.Helper()
	defer stream.Close()
	reader := bufio.NewReader(stream)
	var events []compat.SSEEvent
	for {
		event, err := compat.ReadSSEEvent(reader)
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		events = append(events, event)
	}
}

func valueOrEmpty(value any) string {
	text, _ := value.(string)
	return text
}

func stringFromNestedMap(root map[string]any, key, nestedKey string) string {
	nested, _ := root[key].(map[string]any)
	value, _ := nested[nestedKey].(string)
	return value
}
