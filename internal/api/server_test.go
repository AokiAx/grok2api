package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/api"
	"github.com/AokiAx/grok2api/internal/service"
)

type fakeGateway struct {
	result service.ChatResult
	err    error
}

func (g *fakeGateway) Chat(_ context.Context, _ []byte, _ bool) (service.ChatResult, error) {
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
