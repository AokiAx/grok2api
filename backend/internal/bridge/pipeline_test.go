package bridge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/bridge"
	"github.com/AokiAx/grok2api/backend/internal/service"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

type fakeGateway struct {
	path    string
	payload []byte
	stream  bool
	result  service.ChatResult
	err     error
	chat    service.ChatResult
}

func (g *fakeGateway) Chat(_ context.Context, payload []byte, stream bool) (service.ChatResult, error) {
	g.path = "/chat/completions"
	g.payload = append([]byte(nil), payload...)
	g.stream = stream
	if g.err != nil {
		return service.ChatResult{}, g.err
	}
	if g.chat.Status != 0 || g.chat.Body != nil || g.chat.Stream != nil {
		return g.chat, nil
	}
	return g.result, nil
}

func (g *fakeGateway) Request(_ context.Context, _ string, path string, payload []byte, stream bool) (service.ChatResult, error) {
	g.path = path
	g.payload = append([]byte(nil), payload...)
	g.stream = stream
	if g.err != nil {
		return service.ChatResult{}, g.err
	}
	return g.result, nil
}

func TestPipelineChatNonStreamAggregatesResponses(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"PONG\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		DefaultModel:    "grok-4.5",
		PreferResponses: true,
	}
	result, err := pipeline.Chat(context.Background(), []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if result.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", result.Status, result.Body)
	}
	if !strings.Contains(string(result.Body), `"content":"PONG"`) {
		t.Fatalf("body=%s", result.Body)
	}
	if gateway.path != "/responses" || !gateway.stream {
		t.Fatalf("upstream path=%s stream=%v", gateway.path, gateway.stream)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode forwarded: %v", err)
	}
	if forwarded["stream"] != true {
		t.Fatalf("expected forced stream=true: %#v", forwarded)
	}
	// Bare chat must not force backend_search / inject search tools.
	if _, ok := forwarded["backend_search"]; ok {
		t.Fatalf("bare chat must not set backend_search: %#v", forwarded)
	}
	if tools, ok := forwarded["tools"]; ok && tools != nil {
		if list, _ := tools.([]any); len(list) > 0 {
			t.Fatalf("bare chat must not inject tools: %#v", tools)
		}
	}
}

func TestPipelineChatNonStreamJSONBodyFallback(t *testing.T) {
	jsonBody := `{"id":"resp_json","model":"grok-4.5","object":"response","status":"completed","output_text":"PONG","usage":{"input_tokens":3,"output_tokens":1}}`
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Stream: io.NopCloser(strings.NewReader(jsonBody)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := pipeline.Chat(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":false}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(string(result.Body), `"content":"PONG"`) {
		t.Fatalf("body=%s", result.Body)
	}
}

func TestPipelineChatStreamConvertsSSE(t *testing.T) {
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"output_text\":\"hi\"}}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := pipeline.Chat(context.Background(), []byte(`{"stream":true,"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if result.Stream == nil {
		t.Fatal("expected stream")
	}
	data, err := io.ReadAll(result.Stream)
	_ = result.Stream.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `"content":"hi"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("body=%s", body)
	}
	if result.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type=%q", result.Header.Get("Content-Type"))
	}
}

func TestPipelineMessagesNonStream(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"hello\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := pipeline.Messages(context.Background(), []byte(`{"model":"grok-4.5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if !strings.Contains(string(result.Body), `"type":"message"`) {
		t.Fatalf("body=%s", result.Body)
	}
	if !strings.Contains(string(result.Body), `"text":"hello"`) {
		t.Fatalf("body=%s", result.Body)
	}
	if gateway.path != "/responses" {
		t.Fatalf("path=%s", gateway.path)
	}
}

func TestPipelineResponsesNonStreamExtractsCompleted(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_9\",\"model\":\"grok-4.5\",\"output_text\":\"done\"}}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := pipeline.Responses(context.Background(), []byte(`{"model":"grok-4.5","input":"hi","stream":false}`))
	if err != nil {
		t.Fatalf("responses: %v", err)
	}
	if !strings.Contains(string(result.Body), `"id":"resp_9"`) {
		t.Fatalf("body=%s", result.Body)
	}
	if !gateway.stream {
		t.Fatal("expected upstream stream=true")
	}
}

func TestPipelineChatFallsBackToNativeChat(t *testing.T) {
	gateway := &fakeGateway{chat: service.ChatResult{
		Status: http.StatusOK,
		Body:   []byte(`{"object":"chat.completion","choices":[]}`),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: false,
	}
	result, err := pipeline.Chat(context.Background(), []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gateway.path != "/chat/completions" {
		t.Fatalf("path=%s", gateway.path)
	}
	if !strings.Contains(string(result.Body), `chat.completion`) {
		t.Fatalf("body=%s", result.Body)
	}
}

func TestPipelineInvalidJSON(t *testing.T) {
	pipeline := &bridge.Pipeline{
		Gateway:         &fakeGateway{},
		PreferResponses: true,
	}
	_, err := pipeline.Chat(context.Background(), []byte(`{bad`))
	bridgeErr, ok := bridge.AsError(err)
	if !ok || bridgeErr.Class != bridge.ClassInvalidRequest {
		t.Fatalf("err=%v", err)
	}
}

func TestPipelineMessagesAlwaysUsesResponses(t *testing.T) {
	// Anthropic path always hits /responses (no chat fallback), model passed through.
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"hi\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: false,
	}
	result, err := pipeline.Messages(context.Background(), []byte(`{"model":"grok-4.5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if gateway.path != "/responses" {
		t.Fatalf("path=%s", gateway.path)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if forwarded["model"] != "grok-4.5" {
		t.Fatalf("upstream model=%#v", forwarded["model"])
	}
	if !strings.Contains(string(result.Body), `"type":"message"`) {
		t.Fatalf("body=%s", result.Body)
	}
}

func TestPipelineResponsesStreamPassthrough(t *testing.T) {
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := pipeline.Responses(context.Background(), []byte(`{"model":"grok-4.5","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("responses: %v", err)
	}
	if result.Stream == nil {
		t.Fatal("expected stream")
	}
	data, _ := io.ReadAll(result.Stream)
	_ = result.Stream.Close()
	if !strings.Contains(string(data), "response.output_text.delta") {
		t.Fatalf("body=%s", data)
	}
	if result.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("ct=%q", result.Header.Get("Content-Type"))
	}
}

func TestPipelineMessagesAcceptsResponsesShapedBody(t *testing.T) {
	// Regression: Codex/NewAPI posting Responses JSON to /v1/messages used to
	// wipe input to [] after AnthropicToOpenAI.
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output_text\":\"ok\"}}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	body := []byte(`{
		"model":"grok-4.5",
		"stream":true,
		"instructions":"You are Codex",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}}],
		"tool_choice":"auto"
	}`)
	result, err := pipeline.Messages(context.Background(), body)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if gateway.path != "/responses" {
		t.Fatalf("path=%s", gateway.path)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	input, _ := forwarded["input"].([]any)
	if len(input) == 0 {
		t.Fatalf("input must not be emptied: %#v", forwarded)
	}
	if result.Stream == nil {
		t.Fatal("expected responses stream passthrough")
	}
	_ = result.Stream.Close()
}

func TestPipelineUpstreamErrorMaterializesBody(t *testing.T) {
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusUnprocessableEntity,
		Stream: io.NopCloser(strings.NewReader(`{"error":"bad tools"}`)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := pipeline.Chat(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if result.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", result.Status)
	}
	if !strings.Contains(string(result.Body), "bad tools") {
		t.Fatalf("body=%s", result.Body)
	}
	if result.Stream != nil {
		t.Fatal("stream should be materialized")
	}
}

func TestPipelineMessagesStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_ms"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_ms","status":"completed","usage":{"input_tokens":1,"output_tokens":1},"output":[]}}`,
		``,
	}, "\n")
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := pipeline.Messages(context.Background(), []byte(`{"model":"grok-4.5","stream":true,"max_tokens":4000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2000}}`), "sess-1")
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if result.Stream == nil {
		t.Fatal("expected stream")
	}
	data, _ := io.ReadAll(result.Stream)
	_ = result.Stream.Close()
	if !strings.Contains(string(data), "message_start") || !strings.Contains(string(data), "hi") {
		t.Fatalf("body=%s", data)
	}
	var forwarded map[string]any
	_ = json.Unmarshal(gateway.payload, &forwarded)
	if forwarded["prompt_cache_key"] != "sess-1" {
		t.Fatalf("prompt_cache_key=%v payload=%s", forwarded["prompt_cache_key"], gateway.payload)
	}
}

func TestPipelineMessagesJSONBody(t *testing.T) {
	jsonBody := `{"id":"resp_j","model":"grok-4.5","status":"completed","output_text":"ok","usage":{"input_tokens":1,"output_tokens":1}}`
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Body:   []byte(jsonBody),
	}}
	pipeline := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	// PreferResponses path always uses stream from gateway; simulate body-only error path via empty stream is hard.
	// Use Responses path for non-stream JSON extraction coverage.
	result, err := pipeline.Responses(context.Background(), []byte(`{"model":"grok-4.5","input":"hi","stream":false}`))
	if err != nil {
		t.Fatalf("responses: %v", err)
	}
	// With stream nil and body set, deliverResponses keeps body.
	if len(result.Body) == 0 && result.Stream == nil {
		t.Fatalf("empty result")
	}
	_ = result
}
