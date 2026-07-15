package service_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/service"
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

func TestStickyKeyFromAuthenticatedClientPreservesExplicitTenantIsolation(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer raw-bearer-secret")
	req.Header.Set("x-api-key", "raw-header-secret")
	req.Header.Set("X-Grok2API-Sticky", "caller-controlled")
	req = req.WithContext(service.WithClientGrant(req.Context(), service.ClientGrant{
		Authenticated: true,
		KeyID:         "ck_123",
		Principal:     "client-key:ck_123",
	}))
	got := service.StickyKeyFromRequest(req)
	if got != "X-Grok2API-Sticky:caller-controlled" {
		t.Fatalf("authenticated sticky=%q", got)
	}
	if strings.Contains(got, "raw-bearer-secret") || strings.Contains(got, "raw-header-secret") {
		t.Fatalf("authenticated sticky leaked raw secret: %q", got)
	}

	// An anonymous or malformed grant must not suppress the established
	// explicit-header fallback contract.
	req = req.WithContext(service.WithClientGrant(req.Context(), service.ClientGrant{
		Authenticated: false,
		Principal:     "client-key:untrusted",
	}))
	if got := service.StickyKeyFromRequest(req); got != "X-Grok2API-Sticky:caller-controlled" {
		t.Fatalf("anonymous fallback=%q", got)
	}
}

func TestStickyKeyFromAuthenticatedClientReplacesRawAuthFallback(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer raw-bearer-secret")
	req.Header.Set("x-api-key", "raw-header-secret")
	req = req.WithContext(service.WithClientGrant(req.Context(), service.ClientGrant{
		Authenticated: true,
		KeyID:         "ck_123",
		Principal:     "client-key:ck_123",
	}))
	got := service.StickyKeyFromRequest(req)
	if got != "client-key:ck_123" {
		t.Fatalf("authenticated auth fallback=%q", got)
	}
	if strings.Contains(got, "raw-bearer-secret") || strings.Contains(got, "raw-header-secret") {
		t.Fatalf("auth fallback leaked raw secret: %q", got)
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

func TestPromptCacheKeyStickyPriority(t *testing.T) {
	if got := service.PromptCacheKeyFromPayload(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
	if got := service.PromptCacheKeyFromPayload([]byte(`{"prompt_cache_key":" sess-1 "}`)); got != "sess-1" {
		t.Fatalf("extract: %q", got)
	}
	// prompt_cache_key wins over affinity when no tenant header.
	if got := service.ComposeStickyKeyParts("auth:shared", "sess-1", "aff:x"); got != "cache:sess-1" {
		t.Fatalf("auth+cache: %q", got)
	}
	// tenant header + cache keeps isolation.
	if got := service.ComposeStickyKeyParts("X-User-Id:u1", "sess-1", "aff:x"); got != "X-User-Id:u1|cache:sess-1" {
		t.Fatalf("tenant+cache: %q", got)
	}
	// without cache, client + affinity still compose.
	if got := service.ComposeStickyKeyParts("X-User-Id:u1", "", "aff:x"); got != "X-User-Id:u1|aff:x" {
		t.Fatalf("tenant+aff: %q", got)
	}
}
