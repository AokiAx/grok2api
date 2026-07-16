package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/api"
)

func TestPublicRouteMethodContracts(t *testing.T) {
	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"",
		api.WithAdmin(&fakeAdmin{}, "panel-secret"),
		api.WithFrontend(panelTestFS()),
	)

	tests := []struct {
		name        string
		method      string
		path        string
		wantStatus  int
		allowedVerb string
	}{
		{name: "health rejects post with allow", method: http.MethodPost, path: "/health", wantStatus: http.StatusMethodNotAllowed, allowedVerb: http.MethodGet},
		{name: "models rejects post with allow", method: http.MethodPost, path: "/v1/models", wantStatus: http.StatusMethodNotAllowed, allowedVerb: http.MethodGet},
		// Characterization: the configured SPA's GET catch-all currently wins over
		// these POST-only patterns and deliberately rejects their reserved paths.
		// A future transport/SPA split may choose to restore 405 + Allow instead.
		{name: "spa catch-all makes get on chat not found", method: http.MethodGet, path: "/v1/chat/completions", wantStatus: http.StatusNotFound},
		{name: "spa catch-all makes get on responses not found", method: http.MethodGet, path: "/v1/responses", wantStatus: http.StatusNotFound},
		{name: "spa catch-all makes get on messages not found", method: http.MethodGet, path: "/v1/messages", wantStatus: http.StatusNotFound},
		{name: "spa catch-all makes get on admin login not found", method: http.MethodGet, path: "/api/admin/v1/auth/login", wantStatus: http.StatusNotFound},
		{name: "admin account rejects put with allow", method: http.MethodPut, path: "/api/admin/v1/accounts/account-1", wantStatus: http.StatusMethodNotAllowed, allowedVerb: http.MethodDelete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, nil)

			server.Handler().ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("%s %s status=%d, want %d; body=%s", tt.method, tt.path, recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.allowedVerb != "" {
				if allow := recorder.Header().Get("Allow"); !headerContainsToken(allow, tt.allowedVerb) {
					t.Fatalf("%s %s Allow=%q, want %s", tt.method, tt.path, allow, tt.allowedVerb)
				}
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
				t.Fatalf("%s %s content-type=%q, want text/plain method error", tt.method, tt.path, contentType)
			}
		})
	}
}

func TestOpenAIAuthenticationAndErrorEnvelopeContract(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "api-secret")

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "models", method: http.MethodGet, path: "/v1/models"},
		{name: "billing", method: http.MethodGet, path: "/v1/billing"},
		{name: "chat canonical", method: http.MethodPost, path: "/v1/chat/completions", body: `{}`},
		{name: "chat alias", method: http.MethodPost, path: "/chat/completions", body: `{}`},
		{name: "responses", method: http.MethodPost, path: "/v1/responses", body: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))

			server.Handler().ServeHTTP(recorder, request)

			assertOpenAIErrorEnvelope(t, recorder, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
		})
	}
}

func TestAnthropicMessagesAuthenticationEnvelopeContract(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "api-secret")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
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
	if payload.Type != "error" || payload.Error.Type != "authentication_error" || payload.Error.Message != "Invalid API key" {
		t.Fatalf("anthropic error envelope=%#v", payload)
	}
}

func TestAuthorizationHeaderTakesPrecedenceOverXAPIKey(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "api-secret")
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer wrong-secret")
	request.Header.Set("x-api-key", "api-secret")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	assertOpenAIErrorEnvelope(t, recorder, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
}

func TestAdminV1AuthenticationEnvelopeContract(t *testing.T) {
	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"",
		api.WithAdmin(&fakeAdmin{}, "panel-secret"),
	)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/accounts", nil)

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q, want application/json", got)
	}
	var envelope struct {
		OK    bool `json:"ok"`
		Data  any  `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode admin envelope: %v; body=%s", err, recorder.Body.String())
	}
	if envelope.OK || envelope.Data != nil || envelope.Error.Code != "unauthorized" || envelope.Error.Message != "Invalid admin key" {
		t.Fatalf("unexpected admin envelope: %#v", envelope)
	}
}

func TestJSONResponseHeaderContract(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q, want application/json", got)
	}
	var envelope struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Pool    struct {
			Ready       int            `json:"ready"`
			Unavailable int            `json:"unavailable"`
			Reasons     map[string]int `json:"reasons"`
		} `json:"account_pool"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if !envelope.OK || envelope.Version != "1.0.0-go" || envelope.Pool.Ready != 3 || envelope.Pool.Unavailable != 7 || envelope.Pool.Reasons["quota"] != 5 {
		t.Fatalf("unexpected health contract: %#v", envelope)
	}
}

func assertOpenAIErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status=%d, want %d; body=%s", recorder.Code, status, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q, want application/json", got)
	}
	var envelope struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    string  `json:"code"`
			Param   *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode OpenAI error envelope: %v; body=%s", err, recorder.Body.String())
	}
	wantType := "api_error"
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		wantType = "authentication_error"
	}
	if status == http.StatusTooManyRequests {
		wantType = "rate_limit_error"
	}
	if status == http.StatusBadRequest {
		wantType = "invalid_request_error"
	}
	if envelope.Error.Message != message || envelope.Error.Type != wantType || envelope.Error.Code != code || envelope.Error.Param != nil {
		t.Fatalf("unexpected OpenAI error envelope: %#v want type=%s code=%s", envelope, wantType, code)
	}
}

func headerContainsToken(header, token string) bool {
	for _, item := range strings.Split(header, ",") {
		if strings.TrimSpace(item) == token {
			return true
		}
	}
	return false
}
