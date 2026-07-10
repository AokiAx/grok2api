package upstream_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/upstream"
)

func TestClientSendsGrokCLIHeaders(t *testing.T) {
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
		if got := r.Header.Get("x-grok-model-override"); got != "grok-test" {
			t.Errorf("model override = %q", got)
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
	response, err := client.Chat(
		context.Background(),
		account.Account{AccessToken: "access-token"},
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
