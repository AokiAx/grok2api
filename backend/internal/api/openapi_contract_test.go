package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/api/openapi"
)

func TestOpenAPIDocumentListsRequiredPaths(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(openapi.DocumentJSON(), &root); err != nil {
		t.Fatalf("openapi json: %v", err)
	}
	paths, _ := root["paths"].(map[string]any)
	for path, methods := range openapi.RequiredContractPaths() {
		item, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("contract path missing from openapi: %s", path)
		}
		for _, method := range methods {
			if _, ok := item[strings.ToLower(method)]; !ok {
				t.Fatalf("path %s missing method %s", path, method)
			}
		}
	}
}

func TestOpenAPIAndDocsEndpoints(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi.json=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type=%q", rec.Header().Get("Content-Type"))
	}
	var root map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if root["openapi"] == nil {
		t.Fatal("missing openapi field")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/docs", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "swagger-ui") {
		body := rec.Body.String()
		if len(body) > 120 {
			body = body[:120]
		}
		t.Fatalf("docs=%d body=%s", rec.Code, body)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "openapi:") {
		t.Fatalf("yaml=%d", rec.Code)
	}
}

func TestLiveRoutesCoverOpenAPIRequiredContract(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "api-secret", api.WithAdmin(&fakeAdmin{}, "panel-secret"))
	probes := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodGet, "/readyz", http.StatusOK},
		{http.MethodGet, "/openapi.json", http.StatusOK},
		{http.MethodGet, "/docs", http.StatusOK},
		{http.MethodGet, "/v1/models", http.StatusUnauthorized},
		{http.MethodGet, "/api/admin/v1/system/meta", http.StatusOK},
		{http.MethodGet, "/api/admin/v1/dashboard", http.StatusUnauthorized},
		{http.MethodGet, "/api/admin/v1/accounts", http.StatusUnauthorized},
	}
	for _, probe := range probes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(probe.method, probe.path, nil)
		server.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s %s returned 404; route missing from server", probe.method, probe.path)
		}
		if rec.Code != probe.want {
			t.Fatalf("%s %s status=%d want %d body=%s", probe.method, probe.path, rec.Code, probe.want, rec.Body.String())
		}
	}
}
