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

func TestStaticEmptyAndAll(t *testing.T) {
	s := proxy.Static{}
	if s.Next() != "" || s.All() != nil {
		t.Fatalf("empty static = %q %#v", s.Next(), s.All())
	}
	s = proxy.Static{URL: " http://x "}
	if s.Next() != "http://x" {
		t.Fatalf("trim = %q", s.Next())
	}
	if len(s.All()) != 1 {
		t.Fatalf("all=%#v", s.All())
	}
}

func TestNewDirectWhenEmpty(t *testing.T) {
	provider := proxy.New("", nil)
	if provider.Next() != "" {
		t.Fatalf("expected direct empty, got %q", provider.Next())
	}
}

func TestProviderAll(t *testing.T) {
	provider := proxy.New("http://127.0.0.1:1", []string{"http://127.0.0.1:2", "http://127.0.0.1:3"})
	all := provider.All()
	if len(all) < 1 {
		t.Fatalf("all=%#v", all)
	}
}
