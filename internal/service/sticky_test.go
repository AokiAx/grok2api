package service_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/service"
)

func TestStickyKeyFromRequest(t *testing.T) {
	if got := service.StickyKeyFromRequest(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Grok2API-Sticky", "tenant-a")
	if got := service.StickyKeyFromRequest(req); got != "X-Grok2API-Sticky:tenant-a" {
		t.Fatalf("sticky header: %q", got)
	}
	req, _ = http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	if got := service.StickyKeyFromRequest(req); got != "auth:secret-token" {
		t.Fatalf("auth: %q", got)
	}
	req, _ = http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("x-api-key", "key-1")
	if got := service.StickyKeyFromRequest(req); got != "auth:key-1" {
		t.Fatalf("api key: %q", got)
	}
}

func TestPayloadAffinityKeyAndCompose(t *testing.T) {
	if got := service.PayloadAffinityKey(nil); got != "" {
		t.Fatalf("nil payload: %q", got)
	}
	if got := service.PayloadAffinityKey([]byte(`{`)); got != "" {
		t.Fatalf("invalid json: %q", got)
	}
	body := []byte(`{
		"model":"grok-4.5",
		"instructions":"be helpful",
		"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],
		"tools":[{"type":"function","name":"Search"},{"type":"web_search"}]
	}`)
	key := service.PayloadAffinityKey(body)
	if key == "" || !strings.HasPrefix(key, "aff:") {
		t.Fatalf("affinity key empty/bad: %q", key)
	}
	// Stable for same payload.
	if service.PayloadAffinityKey(body) != key {
		t.Fatal("affinity key not stable")
	}
	if got := service.ComposeStickyKey("client", "aff"); got != "client|aff" {
		t.Fatalf("compose both: %q", got)
	}
	if got := service.ComposeStickyKey("client", ""); got != "client" {
		t.Fatalf("compose client only: %q", got)
	}
	if got := service.ComposeStickyKey("", "aff"); got != "aff" {
		t.Fatalf("compose aff only: %q", got)
	}
}
