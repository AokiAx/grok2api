package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/api"
)

func TestEmbeddedSPAIndexAndDeepLinkContract(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	root := requestPanel(t, server, http.MethodGet, "/")
	if root.Code != http.StatusOK {
		t.Fatalf("root status=%d body=%s", root.Code, root.Body.String())
	}
	if got := root.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("root content-type=%q", got)
	}
	if got := root.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("root cache-control=%q", got)
	}
	index := root.Body.String()
	if !strings.Contains(index, `id="root"`) {
		t.Fatalf("root is not the SPA index: %s", index[:min(200, len(index))])
	}

	for _, path := range []string{"/login", "/accounts/account-1?pool=ready", "/import", "/system"} {
		t.Run(path, func(t *testing.T) {
			recorder := requestPanel(t, server, http.MethodGet, path)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Body.String() != index {
				t.Fatal("deep link did not resolve to the same SPA index")
			}
			if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("content-type=%q", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("cache-control=%q", got)
			}
		})
	}
}

func TestEmbeddedSPAAssetCacheAndFallbackBoundary(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	root := requestPanel(t, server, http.MethodGet, "/")
	assetPath := firstEmbeddedAssetPath(t, root.Body.String())

	asset := requestPanel(t, server, http.MethodGet, assetPath)
	if asset.Code != http.StatusOK {
		t.Fatalf("asset %s status=%d body=%s", assetPath, asset.Code, asset.Body.String())
	}
	if got := asset.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("asset cache-control=%q", got)
	}
	if got := asset.Header().Get("Content-Type"); strings.HasPrefix(got, "text/html") || got == "" {
		t.Fatalf("asset content-type=%q", got)
	}
	if strings.Contains(asset.Body.String(), `id="root"`) {
		t.Fatal("asset request returned the SPA index")
	}

	for _, path := range []string{
		"/assets/missing-contract.js",
		"/api/missing-contract",
		"/admin/missing-contract",
		"/v1/missing-contract",
		"/chat/missing-contract",
		"/health/missing-contract",
		"/healthz/missing-contract",
		"/readyz/missing-contract",
		"/panel/missing-contract",
		"/manager/missing-contract",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := requestPanel(t, server, http.MethodGet, path)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status=%d, want 404; body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), `id="root"`) {
				t.Fatal("reserved or missing asset path fell back to SPA index")
			}
			if got := recorder.Header().Get("Cache-Control"); got != "" {
				t.Fatalf("404 cache-control=%q, want empty", got)
			}
		})
	}
}

func TestEmbeddedSPADeepLinksAreGetOnly(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	recorder := requestPanel(t, server, http.MethodPost, "/accounts")

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405; body=%s", recorder.Code, recorder.Body.String())
	}
	if allow := recorder.Header().Get("Allow"); !headerContainsToken(allow, http.MethodGet) {
		t.Fatalf("Allow=%q, want GET", allow)
	}
	if strings.Contains(recorder.Body.String(), `id="root"`) {
		t.Fatal("non-GET deep link returned SPA index")
	}
}

func requestPanel(t *testing.T, server *api.Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func firstEmbeddedAssetPath(t *testing.T, index string) string {
	t.Helper()
	start := strings.Index(index, "/assets/")
	if start < 0 {
		t.Fatalf("SPA index has no root-mounted asset: %s", index[:min(200, len(index))])
	}
	end := start
	for end < len(index) && index[end] != '"' && index[end] != '\'' {
		end++
	}
	if end == len(index) {
		t.Fatalf("unterminated asset URL in SPA index: %s", index[start:])
	}
	return index[start:end]
}
