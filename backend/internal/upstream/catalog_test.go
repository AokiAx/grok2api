package upstream_test

import (
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

func TestDefaultCatalogRoutesGrok45ToResponses(t *testing.T) {
	catalog := upstream.NewDefaultCatalog()
	if got := catalog.Backend("grok-4.5"); got != upstream.BackendResponses {
		t.Fatalf("backend = %q", got)
	}
	item, ok := catalog.Get("grok-4.5")
	if !ok || !item.SupportsReasoningEffort || item.ContextWindow != 500000 {
		t.Fatalf("item = %#v ok=%v", item, ok)
	}
}

func TestCatalogListAndEnrich(t *testing.T) {
	catalog := upstream.NewDefaultCatalog()
	list := catalog.List()
	if len(list) < 2 {
		t.Fatalf("list=%#v", list)
	}
	item := map[string]any{"id": "grok-4.5"}
	enriched := catalog.EnrichModelMap(item)
	if enriched["api_backend"] != upstream.BackendResponses {
		t.Fatalf("enriched=%#v", enriched)
	}
	if enriched["context_window"] != 500000 {
		t.Fatalf("context=%#v", enriched["context_window"])
	}
}

func TestResolveUpstream(t *testing.T) {
	catalog := upstream.NewCatalog([]upstream.ModelInfo{
		{ID: "grok-fast", UpstreamID: "grok-4.5", APIBackend: upstream.BackendResponses},
		{ID: "alias-fast", UpstreamID: "grok-4.5", APIBackend: upstream.BackendResponses},
	})
	if got := catalog.ResolveUpstream("grok-fast"); got != "grok-4.5" {
		t.Fatalf("public id: %q", got)
	}
	if got := catalog.ResolveUpstream("alias-fast"); got != "grok-4.5" {
		t.Fatalf("alias: %q", got)
	}
	if got := catalog.ResolveUpstream("unknown-model"); got != "unknown-model" {
		t.Fatalf("unknown: %q", got)
	}
}
