package settings

import "testing"

func TestDocumentNormalizeAndProxyStatus(t *testing.T) {
	doc := Defaults()
	doc.Pool.Strategy = "fill_first"
	doc.Proxy.Enabled = true
	doc.Proxy.URL = " http://127.0.0.1:8118 "
	if err := doc.Normalize(); err != nil {
		t.Fatal(err)
	}
	if doc.Pool.Strategy != "fill-first" {
		t.Fatalf("strategy=%q", doc.Pool.Strategy)
	}
	if doc.Proxy.RuntimeStatus != "not_wired" {
		t.Fatalf("runtime status=%q", doc.Proxy.RuntimeStatus)
	}
	if doc.Proxy.URL != "http://127.0.0.1:8118" {
		t.Fatalf("url=%q", doc.Proxy.URL)
	}
}
