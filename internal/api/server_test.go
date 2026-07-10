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

	"github.com/AokiAx/grok2api/internal/api"
	"github.com/AokiAx/grok2api/internal/service"
)

type fakeGateway struct {
	result  service.ChatResult
	err     error
	payload []byte
}

func (g *fakeGateway) Chat(_ context.Context, payload []byte, _ bool) (service.ChatResult, error) {
	g.payload = append([]byte(nil), payload...)
	return g.result, g.err
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
	gateway := &fakeGateway{err: &service.PoolUnavailableError{
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
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader("data: hello\n\ndata: [DONE]\n\n")),
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
	if recorder.Body.String() != "data: hello\n\ndata: [DONE]\n\n" {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestChatAddsConfiguredDefaultModel(t *testing.T) {
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"ok":true}`),
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
}

func TestChatRequiresConfiguredAPIKey(t *testing.T) {
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"ok":true}`),
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
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"ok":true}`),
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
