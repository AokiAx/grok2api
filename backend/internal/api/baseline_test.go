package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

func TestProductionBaselineAddsRequestIDAndSecurityHeaders(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	requestID := recorder.Header().Get(api.HeaderRequestID)
	if requestID == "" {
		t.Fatal("missing X-Request-Id")
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options=%q", got)
	}
	if got := recorder.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options=%q", got)
	}
	if got := recorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy=%q", got)
	}
}

func TestProductionBaselinePreservesIncomingRequestID(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(api.HeaderRequestID, "client-req-123")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if got := recorder.Header().Get(api.HeaderRequestID); got != "client-req-123" {
		t.Fatalf("request id = %q", got)
	}
}

func TestReadyzHonorsProcessReadinessGate(t *testing.T) {
	gate := &api.AtomicReadiness{}
	gate.Set(false, "starting")
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "", api.WithReadiness(gate))

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("starting readyz status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["reason"] != "starting" || body["process_ready"] != false {
		t.Fatalf("starting payload=%#v", body)
	}

	gate.Set(true, "serving")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("serving readyz status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestReadyzFailsWhenPoolEmptyEvenIfProcessReady(t *testing.T) {
	gate := &api.AtomicReadiness{}
	gate.Set(true, "serving")
	server := api.NewServer(&fakeGateway{}, emptyStatus{}, "", api.WithReadiness(gate))
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no_ready_accounts") {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}

func TestGlobalBodyLimitRejectsOversizedRequests(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "api-secret", api.WithMaxBodyBytes(64))
	body := strings.Repeat("x", 128)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer api-secret")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestUnauthorizedUsesStableErrorCode(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "api-secret")
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	assertOpenAIErrorEnvelope(t, recorder, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
}

func TestPoolUnavailableStatusContract(t *testing.T) {
	gateway := &fakeGateway{requestErr: &service.PoolUnavailableError{
		Status:     http.StatusServiceUnavailable,
		RetryAfter: 3 * time.Second,
		Reason:     service.PoolReasonSaturated,
		Message:    "ready accounts are at concurrency capacity",
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	if recorder.Header().Get("X-Grok2API-Pool-Reason") != "saturated" {
		t.Fatalf("pool reason=%q", recorder.Header().Get("X-Grok2API-Pool-Reason"))
	}
}

type emptyStatus struct{}

func (emptyStatus) PoolStatus() api.PoolStatus {
	return api.PoolStatus{Ready: 0, Unavailable: 2, Reasons: map[string]int{"quota": 2}}
}
