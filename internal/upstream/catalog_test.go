package upstream_test

import (
	"testing"

	"github.com/AokiAx/grok2api/internal/upstream"
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
