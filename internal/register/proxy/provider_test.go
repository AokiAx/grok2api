package proxy_test

import (
	"testing"

	"github.com/AokiAx/grok2api/internal/register/proxy"
)

func TestPoolRotates(t *testing.T) {
	pool := proxy.NewPool([]string{"http://a", "http://b", ""})
	if got := pool.Next(); got != "http://a" {
		t.Fatalf("first = %q", got)
	}
	if got := pool.Next(); got != "http://b" {
		t.Fatalf("second = %q", got)
	}
	if got := pool.Next(); got != "http://a" {
		t.Fatalf("third = %q", got)
	}
}

func TestNewPrefersPrimaryThenPool(t *testing.T) {
	provider := proxy.New("http://primary", []string{"http://secondary"})
	if provider.Next() != "http://primary" {
		t.Fatal("expected primary first")
	}
	if provider.Next() != "http://secondary" {
		t.Fatal("expected secondary next")
	}
}
