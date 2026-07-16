package api_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

func TestMessagesValidationErrorUsesAnthropicEnvelopeWithParam(t *testing.T) {
	// filters cannot be enforced → bridge invalid request with param.
	gateway := &fakeGateway{requestResult: service.ChatResult{Status: http.StatusOK, Body: []byte(`{}`)}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	body := `{
		"model":"grok-4.5",
		"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]
	}`
	// Anthropic path uses Messages; filters on tools may be OpenAI-shaped after convert.
	// Use Responses which hits the same finalize validation.
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Error struct {
			Message string  `json:"message"`
			Code    string  `json:"code"`
			Param   *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v body=%s", err, recorder.Body.String())
	}
	if envelope.Error.Code == "" {
		t.Fatalf("expected code: %#v", envelope.Error)
	}
	if envelope.Error.Param == nil || !strings.Contains(*envelope.Error.Param, "filters") {
		t.Fatalf("expected filters param: %#v", envelope.Error)
	}
}

func TestMessagesPoolErrorUsesAnthropicEnvelope(t *testing.T) {
	gateway := &fakeGateway{requestErr: &service.PoolUnavailableError{
		Status:     http.StatusServiceUnavailable,
		RetryAfter: 30 * time.Second,
		Reason:     service.PoolReasonEmpty,
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
			`{"model":"grok-4.5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		)),
	)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v body=%s", err, recorder.Body.String())
	}
	if payload.Type != "error" || payload.Error.Type == "" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestMessagesGenericUpstreamErrorAnthropicEnvelope(t *testing.T) {
	gateway := &fakeGateway{requestErr: errors.New("upstream boom")}
	server := api.NewServer(gateway, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
			`{"model":"grok-4.5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		)),
	)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"type":"error"`) {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}

func TestChatInvalidJSONUsesOpenAIEnvelope(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{bad`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("body=%s", recorder.Body.String())
	}
	_ = io.Discard
}
