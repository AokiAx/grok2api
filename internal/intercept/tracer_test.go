package intercept_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/intercept"
)

func TestMiddlewareWritesTraceFile(t *testing.T) {
	dir := t.TempDir()
	tracer := intercept.New(intercept.Options{Enabled: true, Dir: dir, MaxBody: 4096})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	handler := intercept.Middleware(tracer, inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected trace file")
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "client_request") || !strings.Contains(text, "client_response") {
		t.Fatalf("trace missing stages: %s", text)
	}
	// Ensure body parsed as JSON object in at least one line.
	foundBody := false
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event["stage"] == "client_request" {
			if body, ok := event["body"].(map[string]any); ok && body["bytes"] != nil {
				foundBody = true
			}
		}
	}
	if !foundBody {
		t.Fatalf("client_request body missing: %s", text)
	}
}

func TestMiddlewareDisabledIsNoop(t *testing.T) {
	dir := t.TempDir()
	tracer := intercept.New(intercept.Options{Enabled: false, Dir: dir})
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	handler := intercept.Middleware(tracer, inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called {
		t.Fatal("inner not called")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("disabled tracer should not write files: %v", entries)
	}
}

func TestBodyPreviewRedactsSecrets(t *testing.T) {
	tracer := intercept.New(intercept.Options{Enabled: true, MaxBody: 1024})
	raw := []byte(`{"api_key":"secret","messages":[{"role":"user","content":"hi"}],"authorization":"Bearer x"}`)
	preview := tracer.BodyPreview(raw)
	body, _ := preview["body"].(map[string]any)
	if body["api_key"] != "***" {
		t.Fatalf("api_key not redacted: %#v", body)
	}
	if body["authorization"] != "***" {
		t.Fatalf("authorization not redacted: %#v", body)
	}
}
