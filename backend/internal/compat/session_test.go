package compat_test

import (
	"net/http"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/compat"
)

func TestSessionIDFromRequest(t *testing.T) {
	if got := compat.SessionIDFromRequest(nil); got != "" {
		t.Fatalf("nil request: %q", got)
	}
	req := httptestRequest(map[string]string{
		"X-Claude-Code-Session-Id": "sess-claude",
		"x-session-id":             "sess-generic",
	})
	if got := compat.SessionIDFromRequest(req); got != "sess-claude" {
		t.Fatalf("prefer claude header: %q", got)
	}
	req = httptestRequest(map[string]string{"x-grok-conv-id": "conv-1"})
	if got := compat.SessionIDFromRequest(req); got != "conv-1" {
		t.Fatalf("conv id: %q", got)
	}
	req = httptestRequest(nil)
	if got := compat.SessionIDFromRequest(req); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

func httptestRequest(headers map[string]string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "http://example.test/v1/messages", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}
