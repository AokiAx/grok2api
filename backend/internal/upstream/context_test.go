package upstream_test

import (
	"context"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

func TestWithConvID(t *testing.T) {
	if got := upstream.ConvIDFrom(nil); got != "" {
		t.Fatalf("nil ctx: %q", got)
	}
	ctx := upstream.WithConvID(nil, "conv-1")
	if got := upstream.ConvIDFrom(ctx); got != "conv-1" {
		t.Fatalf("set: %q", got)
	}
	ctx = upstream.WithConvID(context.Background(), "")
	if got := upstream.ConvIDFrom(ctx); got != "" {
		t.Fatalf("empty id: %q", got)
	}
}
