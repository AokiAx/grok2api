package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

type fakeGateway struct {
	result        service.ChatResult
	err           error
	payload       []byte
	requestResult service.ChatResult
	requestErr    error
	method        string
	path          string
	stream        bool
}

func (g *fakeGateway) Chat(_ context.Context, payload []byte, _ bool) (service.ChatResult, error) {
	g.payload = append([]byte(nil), payload...)
	return g.result, g.err
}

func (g *fakeGateway) Request(
	_ context.Context,
	method string,
	path string,
	payload []byte,
	stream bool,
) (service.ChatResult, error) {
	g.method = method
	g.path = path
	g.payload = append([]byte(nil), payload...)
	g.stream = stream
	return g.requestResult, g.requestErr
}

type fakeStatus struct{}

func (fakeStatus) PoolStatus() api.PoolStatus {
	return api.PoolStatus{
		Ready:       3,
		Unavailable: 7,
		Reasons: map[string]int{
			"quota": 5,
			"auth":  2,
		},
	}
}

func TestHealthReturnsTwoPoolSummary(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	pool := payload["account_pool"].(map[string]any)
	if pool["ready"] != float64(3) || pool["unavailable"] != float64(7) {
		t.Fatalf("pool = %#v", pool)
	}
}

func TestChatReturns429WithRetryAfterWhenReadyPoolEmpty(t *testing.T) {
	gateway := &fakeGateway{requestErr: &service.PoolUnavailableError{
		Status:     http.StatusTooManyRequests,
		RetryAfter: 30 * time.Minute,
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d; want 429", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing")
	}
}

func TestChatStreamsSSEAndFlushesContent(t *testing.T) {
	sse := `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","output_text":"hello"}}

`
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"content":"hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body = %q", body)
	}
}

func TestChatRoutesResponsesBackendAndEnablesSearch(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"searched\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"grok-4.5","messages":[{"role":"user","content":"latest news"}],"stream":false}`),
		),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gateway.method != http.MethodPost || gateway.path != "/responses" || !gateway.stream {
		t.Fatalf("upstream = %s %s stream=%v", gateway.method, gateway.path, gateway.stream)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode forwarded: %v", err)
	}
	if forwarded["backend_search"] != true {
		t.Fatalf("backend_search missing/false: %#v", forwarded)
	}
	if forwarded["stream"] != true {
		t.Fatalf("expected upstream stream=true even for non-stream clients: %#v", forwarded)
	}
	if _, ok := forwarded["input"]; !ok {
		t.Fatalf("expected responses input: %#v", forwarded)
	}
	if !strings.Contains(recorder.Body.String(), "searched") {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}

func TestChatNonStreamAggregatesJSONBodyFallback(t *testing.T) {
	// Simulate the production bug: gateway returns a non-SSE Responses JSON body
	// because the upstream request carried stream:false.
	jsonBody := `{"id":"resp_json","model":"grok-4.5","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PONG"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Stream: io.NopCloser(strings.NewReader(jsonBody)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"PONG"`) {
		t.Fatalf("body=%s", recorder.Body.String())
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode forwarded: %v", err)
	}
	if forwarded["stream"] != true {
		t.Fatalf("expected forced upstream stream=true: %#v", forwarded)
	}
}

func TestChatStreamsConvertedResponsesSSE(t *testing.T) {
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"model\":\"grok-4.5\",\"output_text\":\"hello\"}}\n\n"
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		),
	)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), body)
	}
	if !strings.Contains(body, `"content":"hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body=%s", body)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if forwarded["backend_search"] != true {
		t.Fatalf("backend_search = %#v", forwarded["backend_search"])
	}
}

func TestChatRespectsExplicitBackendSearchFalse(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"ok\"}}\n\n"
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"grok-4.5","backend_search":false,"messages":[{"role":"user","content":"hi"}]}`),
		),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if forwarded["backend_search"] != false {
		t.Fatalf("backend_search = %#v", forwarded["backend_search"])
	}
}

func TestChatAddsConfiguredDefaultModel(t *testing.T) {
	sse := `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-default","output_text":"ok"}}

`
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(
		gateway,
		fakeStatus{},
		"",
		api.WithDefaultModel("grok-default"),
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`),
	)
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	var payload map[string]any
	if err := json.Unmarshal(gateway.payload, &payload); err != nil {
		t.Fatalf("decode forwarded payload: %v", err)
	}
	if payload["model"] != "grok-default" {
		t.Fatalf("model = %v; want grok-default", payload["model"])
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestChatRequiresConfiguredAPIKey(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Stream: io.NopCloser(strings.NewReader(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","output_text":"ok"}}

`)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "secret")

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		unauthorized,
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)),
	)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	authorizedRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{}`),
	)
	authorizedRequest.Header.Set("Authorization", "Bearer secret")
	authorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d", authorized.Code)
	}
}

func TestChatRejectsInvalidJSON(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{invalid}`)),
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", recorder.Code)
	}
}

func TestChatAcceptsXAPIKey(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Stream: io.NopCloser(strings.NewReader(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","output_text":"ok"}}

`)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "secret")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	request.Header.Set("x-api-key", "secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestModelsNormalizesUpstreamList(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"data":[{"model":"grok-4.5"},{"id":"grok-fast","owned_by":"xai-cli"}]}`),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/v1/models", nil),
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if gateway.method != http.MethodGet || gateway.path != "/models" {
		t.Fatalf("upstream request = %s %s", gateway.method, gateway.path)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if payload.Object != "list" || len(payload.Data) != 2 {
		t.Fatalf("models = %#v", payload)
	}
	if payload.Data[0].ID != "grok-4.5" || payload.Data[0].Object != "model" || payload.Data[0].OwnedBy != "xai" {
		t.Fatalf("first model = %#v", payload.Data[0])
	}
}

func TestModelsFallsBackToConfiguredDefaults(t *testing.T) {
	gateway := &fakeGateway{requestErr: context.DeadlineExceeded}
	server := api.NewServer(
		gateway,
		fakeStatus{},
		"",
		api.WithDefaultModel("grok-custom"),
	)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/v1/models", nil),
	)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "grok-custom") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBillingAndResponsesProxyThroughAccountGateway(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"X-Upstream": []string{"yes"}},
		Body:   []byte(`{"limit":100,"used":20}`),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")

	billing := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		billing,
		httptest.NewRequest(http.MethodGet, "/v1/billing", nil),
	)
	if billing.Code != http.StatusOK || gateway.path != "/billing" {
		t.Fatalf("billing status=%d path=%q body=%s", billing.Code, gateway.path, billing.Body.String())
	}

	gateway.requestResult.Body = []byte(`{"id":"resp_1","object":"response"}`)
	responses := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		responses,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.5","input":"hi"}`)),
	)
	if responses.Code != http.StatusOK || gateway.method != http.MethodPost || gateway.path != "/responses" {
		t.Fatalf("responses status=%d request=%s %s", responses.Code, gateway.method, gateway.path)
	}
}

func TestResponsesRouteFlattensFunctionToolsAndChoice(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"id":"resp_1","object":"response"}`),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{
				"model":"grok-4.5",
				"input":"inspect",
				"tools":[{"type":"function","function":{"name":"Inspect"}}],
				"tool_choice":{"type":"function","function":{"name":"Inspect"}}
			}`),
		),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode forwarded: %v", err)
	}
	tools, _ := forwarded["tools"].([]any)
	var tool map[string]any
	haveSearch := map[string]bool{}
	for _, raw := range tools {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "web_search", "x_search":
			haveSearch[item["type"].(string)] = true
		case "function":
			if item["name"] == "Inspect" {
				tool = item
			}
		}
	}
	if tool == nil {
		t.Fatalf("Inspect function missing: %#v", tools)
	}
	if !haveSearch["web_search"] || !haveSearch["x_search"] {
		t.Fatalf("default search tools missing: %#v", tools)
	}
	if _, ok := tool["parameters"].(map[string]any); !ok {
		t.Fatalf("top-level parameters missing: %#v", tool)
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested function leaked: %#v", tool)
	}
	choice := forwarded["tool_choice"].(map[string]any)
	if choice["name"] != "Inspect" {
		t.Fatalf("tool choice not flattened: %#v", choice)
	}
}

func TestAnthropicMessagesConvertsRequestAndResponse(t *testing.T) {
	// Anthropic messages for catalog models go through /responses (same as OpenAI chat).
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.5\",\"output_text\":\"pong\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","system":"be brief","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`)),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gateway.method != http.MethodPost || gateway.path != "/responses" || !gateway.stream {
		t.Fatalf("upstream = %s %s stream=%v", gateway.method, gateway.path, gateway.stream)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode forwarded: %v", err)
	}
	if _, ok := forwarded["input"]; !ok {
		t.Fatalf("forwarded missing input: %#v", forwarded)
	}
	if forwarded["backend_search"] != true {
		t.Fatalf("backend_search missing on anthropic path: %#v", forwarded)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["type"] != "message" || response["stop_reason"] != "end_turn" {
		t.Fatalf("response = %#v", response)
	}
}

func TestAnthropicMessagesConvertsStreamingSSE(t *testing.T) {
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"model\":\"grok-4.5\",\"output_text\":\"hello\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)),
	)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), body)
	}
	for _, event := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %q in %s", event, body)
		}
	}
}

func TestCompatibilityRoutesRequireAPIKey(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "secret")
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/models", nil),
		httptest.NewRequest(http.MethodGet, "/v1/billing", nil),
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)),
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", request.URL.Path, recorder.Code)
		}
	}
}

func TestCompatibilityRouteErrorBranches(t *testing.T) {
	t.Run("billing gateway error", func(t *testing.T) {
		server := api.NewServer(&fakeGateway{requestErr: context.DeadlineExceeded}, fakeStatus{}, "")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/billing", nil))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", recorder.Code)
		}
	})

	t.Run("responses invalid json", func(t *testing.T) {
		server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{invalid}`)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", recorder.Code)
		}
	})

	t.Run("responses pool unavailable", func(t *testing.T) {
		server := api.NewServer(&fakeGateway{requestErr: &service.PoolUnavailableError{Status: 429, RetryAfter: time.Minute}}, fakeStatus{}, "")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi"}`)))
		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
			t.Fatalf("status=%d retry=%q", recorder.Code, recorder.Header().Get("Retry-After"))
		}
	})

	t.Run("messages invalid request", func(t *testing.T) {
		server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{invalid}`)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", recorder.Code)
		}
	})

	t.Run("messages invalid upstream response", func(t *testing.T) {
		server := api.NewServer(&fakeGateway{result: service.ChatResult{Status: 200, Header: make(http.Header), Body: []byte(`not-json`)}}, fakeStatus{}, "")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`)))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", recorder.Code)
		}
	})

	t.Run("messages missing stream", func(t *testing.T) {
		server := api.NewServer(&fakeGateway{result: service.ChatResult{Status: 200, Header: make(http.Header)}}, fakeStatus{}, "")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true,"messages":[]}`)))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", recorder.Code)
		}
	})
}

func TestModelsEnrichesKnownCLIMetadata(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"data":[{"id":"grok-4.5"}]}`),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"api_backend":"responses"`) || !strings.Contains(body, `"context_window":500000`) {
		t.Fatalf("body=%s", body)
	}
}
