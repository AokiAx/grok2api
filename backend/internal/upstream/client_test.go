package upstream_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

func TestClientSendsGrokCLIHeaders(t *testing.T) {
	var (
		agentID   string
		sessionID string
		reqID     string
		convID    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("X-XAI-Token-Auth"); got != "xai-grok-cli" {
			t.Errorf("token auth = %q", got)
		}
		if got := r.Header.Get("x-grok-client-version"); got != "0.2.93" {
			t.Errorf("client version = %q", got)
		}
		if got := r.Header.Get("x-grok-client-identifier"); got != "grok-cli" {
			t.Errorf("client identifier = %q", got)
		}
		if got := r.Header.Get("x-grok-client-surface"); got != "tui" {
			t.Errorf("client surface = %q", got)
		}
		if got := r.Header.Get("x-grok-model-override"); got != "grok-test" {
			t.Errorf("model override = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "xai-grok-build/0.2.93" {
			t.Errorf("user-agent = %q", got)
		}
		if got := r.Header.Get("x-userid"); got != "user-1" {
			t.Errorf("userid = %q", got)
		}
		agentID = r.Header.Get("x-grok-agent-id")
		sessionID = r.Header.Get("x-grok-session-id")
		reqID = r.Header.Get("x-grok-req-id")
		convID = r.Header.Get("x-grok-conv-id")
		if agentID == "" || sessionID == "" || reqID == "" || convID == "" {
			t.Errorf("missing identity headers agent=%q session=%q req=%q conv=%q", agentID, sessionID, reqID, convID)
		}
		if got := r.Header.Get("x-grok-conversation-id"); got != convID {
			t.Errorf("conversation-id = %q want %q", got, convID)
		}
		if got := r.Header.Get("traceparent"); !strings.HasPrefix(got, "00-") {
			t.Errorf("traceparent = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload["model"] != "grok-test" {
			t.Errorf("model = %v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := upstream.NewClient(server.URL+"/v1", "0.2.93", server.Client())
	item := account.Account{ID: "acct-1", AccessToken: "access-token", UserID: "user-1"}
	response, err := client.Chat(
		context.Background(),
		item,
		[]byte(`{"model":"grok-test","stream":false}`),
		false,
	)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	// Second request: prefer ctx conv-id for x-grok-conv-id.
	var gotConv string
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConv = r.Header.Get("x-grok-conv-id")
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("stream Accept-Encoding = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server2.Close()
	client2 := upstream.NewClientWithOptions(server2.URL+"/v1", "0.2.93", server2.Client(), upstream.ClientOptions{
		TokenAuth:        "xai-grok-cli",
		ClientIdentifier: "custom-cli",
		UserAgent:        "custom-ua/1",
	})
	response2, err := client2.Request(
		upstream.WithConvID(context.Background(), "cache-key-abc"),
		item,
		http.MethodPost,
		"/responses",
		[]byte(`{"model":"grok-test","stream":true}`),
		true,
	)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	defer response2.Body.Close()
	if gotConv != "cache-key-abc" {
		t.Fatalf("conv-id = %q", gotConv)
	}
}

func TestStreamingRequestIsNotCancelledByNonStreamingTotalTimeout(t *testing.T) {
	releaseBody := make(chan struct{})
	requestCancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
			close(requestCancelled)
			return
		case <-releaseBody:
			_, _ = writer.Write([]byte("data: done\n\n"))
		}
	}))
	defer server.Close()

	baseClient := server.Client()
	baseClient.Timeout = 25 * time.Millisecond
	client := upstream.NewClient(server.URL, "0.2.93", baseClient)
	response, err := client.Request(
		context.Background(),
		account.Account{ID: "stream-account", AccessToken: "token"},
		http.MethodPost,
		"/responses",
		[]byte(`{"model":"grok-4.5","stream":true}`),
		true,
	)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer response.Body.Close()

	select {
	case <-requestCancelled:
		t.Fatal("stream was cancelled by the non-streaming total timeout")
	case <-time.After(75 * time.Millisecond):
	}
	close(releaseBody)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(body) != "data: done\n\n" {
		t.Fatalf("body=%q", body)
	}
}

func TestClientStableIdentityAndStreamEncoding(t *testing.T) {
	var firstAgent, firstSession, secondAgent, secondSession string
	var acceptEncoding string
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			firstAgent = r.Header.Get("x-grok-agent-id")
			firstSession = r.Header.Get("x-grok-session-id")
			if got := r.Header.Get("x-grok-conv-id"); got != "sticky-conv" {
				t.Errorf("conv-id = %q", got)
			}
		} else {
			secondAgent = r.Header.Get("x-grok-agent-id")
			secondSession = r.Header.Get("x-grok-session-id")
			acceptEncoding = r.Header.Get("Accept-Encoding")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := upstream.NewClient(server.URL+"/v1", "0.2.93", server.Client())
	item := account.Account{ID: "stable-1", AccessToken: "tok"}
	ctx := upstream.WithConvID(context.Background(), "sticky-conv")
	resp1, err := client.Request(ctx, item, http.MethodPost, "/responses", []byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatalf("req1: %v", err)
	}
	resp1.Body.Close()
	resp2, err := client.Request(ctx, item, http.MethodPost, "/responses", []byte(`{"model":"m"}`), true)
	if err != nil {
		t.Fatalf("req2: %v", err)
	}
	resp2.Body.Close()
	if firstAgent == "" || firstAgent != secondAgent {
		t.Fatalf("agent identity unstable: %q vs %q", firstAgent, secondAgent)
	}
	if firstSession == "" || firstSession != secondSession {
		t.Fatalf("session identity unstable: %q vs %q", firstSession, secondSession)
	}
	if acceptEncoding != "identity" {
		t.Fatalf("stream Accept-Encoding = %q", acceptEncoding)
	}
}

func TestValidateClassifiesAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"invalid-token"}`))
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL+"/v1", "0.2.93", server.Client())

	reason, code, err := client.Validate(
		context.Background(),
		account.Account{AccessToken: "bad-token"},
	)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if reason != account.ReasonAuth || code != "invalid-token" {
		t.Fatalf("validation = %q %q", reason, code)
	}
}

func TestValidateAcceptsUsableAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL+"/v1", "0.2.93", server.Client())

	reason, code, err := client.Validate(
		context.Background(),
		account.Account{AccessToken: "valid-token"},
	)
	if err != nil || reason != "" || code != "" {
		t.Fatalf("validation = %q %q %v", reason, code, err)
	}
}

func TestRefreshUsesOIDCDiscoveryAndUpdatesCredential(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(writer).Encode(map[string]any{"token_endpoint": server.URL + "/token"})
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "old-refresh" || request.Form.Get("client_id") != "client" {
				t.Fatalf("form = %#v", request.Form)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"access_token":  "new-access",
				"refresh_token": "new-refresh",
				"expires_in":    3600,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL+"/v1", "0.2.93", server.Client())

	before := time.Now().UTC()
	item, err := client.Refresh(context.Background(), account.Account{
		ID:           "account-1",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		OIDCIssuer:   server.URL,
		OIDCClientID: "client",
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if item.AccessToken != "new-access" || item.RefreshToken != "new-refresh" {
		t.Fatalf("account = %#v", item)
	}
	if item.ExpiresAt.Before(before.Add(50 * time.Minute)) {
		t.Fatalf("expires at = %s", item.ExpiresAt)
	}
}

func TestRefreshRejectsMissingRefreshFields(t *testing.T) {
	client := upstream.NewClient("https://example.test/v1", "0.2.93", nil)
	_, err := client.Refresh(context.Background(), account.Account{})
	if err == nil || !strings.Contains(err.Error(), "refresh fields") {
		t.Fatalf("error = %v", err)
	}
}

func TestProbeFreeQuotaUsesRateLimitHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-limit-tokens", "1000")
		w.Header().Set("x-ratelimit-remaining-tokens", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL, "0.2.93", server.Client())
	reason, code, err := client.ProbeFreeQuota(context.Background(), account.Account{AccessToken: "t"})
	if err != nil || reason != "" {
		t.Fatalf("probe err=%v reason=%q code=%q", err, reason, code)
	}
}

func TestProbeFreeQuotaDetectsExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-limit-tokens", "1000")
		w.Header().Set("x-ratelimit-remaining-tokens", "0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL, "0.2.93", server.Client())
	reason, code, err := client.ProbeFreeQuota(context.Background(), account.Account{AccessToken: "t"})
	if err != nil || reason != account.ReasonQuota {
		t.Fatalf("probe err=%v reason=%q code=%q", err, reason, code)
	}
}

func TestValidateModelsAndProbeSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/responses"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			payload := "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
			_, _ = w.Write([]byte(payload))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL, "0.2.93", server.Client())
	reason, code, err := client.Validate(context.Background(), account.Account{AccessToken: "t"})
	if err != nil || reason != "" || code != "" {
		t.Fatalf("validate err=%v reason=%q code=%q", err, reason, code)
	}
}

func TestValidateResponsesProbeAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid or expired credentials"}`))
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL, "0.2.93", server.Client())
	reason, _, err := client.Validate(context.Background(), account.Account{AccessToken: "t"})
	if err != nil || reason != account.ReasonAuth {
		t.Fatalf("err=%v reason=%q", err, reason)
	}
}

func TestValidateRetriesTransientPermissionDenied(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.completed\ndata: {}\n\n"))
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL, "0.2.93", server.Client())
	reason, code, err := client.Validate(context.Background(), account.Account{AccessToken: "t"})
	if err != nil || reason != "" || code != "" {
		t.Fatalf("validate err=%v reason=%q code=%q hits=%d", err, reason, code, hits.Load())
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d; want 3", hits.Load())
	}
}

func TestValidatePermissionDeniedExhaustedIsValidating(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`))
	}))
	defer server.Close()
	client := upstream.NewClient(server.URL, "0.2.93", server.Client())
	// Short timeout so 3×1.5s retries don't hang the suite if something else breaks.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	reason, code, err := client.Validate(ctx, account.Account{AccessToken: "t"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if reason != account.ReasonValidating {
		t.Fatalf("reason=%q code=%q; want validating", reason, code)
	}
}

func TestRefreshMarksInvalidGrantAsPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "openid-configuration") {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token_endpoint": "http://" + r.Host + "/oauth2/token",
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token has been revoked"}`))
	}))
	defer server.Close()

	client := upstream.NewClient("https://example.invalid", "0.2.93", server.Client())
	// Point issuer at test server so discovery + token hit httptest.
	item := account.Account{
		ID: "a1", RefreshToken: "dead", OIDCIssuer: server.URL, OIDCClientID: "client",
	}
	_, err := client.Refresh(context.Background(), item)
	if !upstream.IsPermanentRefreshError(err) {
		t.Fatalf("expected permanent refresh error, got %v", err)
	}
}

func TestRefreshTransientStatusIsNotPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "openid-configuration") {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token_endpoint": "http://" + r.Host + "/oauth2/token",
			})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("temporary"))
	}))
	defer server.Close()
	client := upstream.NewClient("https://example.invalid", "0.2.93", server.Client())
	_, err := client.Refresh(context.Background(), account.Account{
		ID: "a1", RefreshToken: "x", OIDCIssuer: server.URL, OIDCClientID: "client",
	})
	if err == nil || upstream.IsPermanentRefreshError(err) {
		t.Fatalf("expected transient error, got %v", err)
	}
}
