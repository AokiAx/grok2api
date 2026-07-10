package upstream_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
