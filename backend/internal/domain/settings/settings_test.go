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
	if doc.ClientKeys.DefaultRPMLimit != 120 || doc.ClientKeys.DefaultMaxConcurrent != 4 {
		t.Fatalf("client key defaults=%+v", doc.ClientKeys)
	}
}

func TestUnmarshalSeedsLegacyClientKeyDefaults(t *testing.T) {
	raw := []byte(`{"revision":2,"pool":{"max_concurrent":4,"max_attempts":3,"strategy":"round-robin","active_size":0,"sticky":true,"sticky_ttl_minutes":30,"quota_retry_minutes":1440,"rate_retry_seconds":45},"timeouts":{"request_timeout_sec":600,"acquire_timeout_sec":60},"audit":{"retention_days":30},"proxy":{"url":"","enabled":false}}`)
	doc, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ClientKeys.DefaultRPMLimit != 120 || doc.ClientKeys.DefaultMaxConcurrent != 4 {
		t.Fatalf("legacy seed=%+v", doc.ClientKeys)
	}
}

func TestUnmarshalKeepsExplicitZeroClientKeyDefaults(t *testing.T) {
	raw := []byte(`{"revision":3,"pool":{"max_concurrent":4,"max_attempts":3,"strategy":"round-robin","active_size":0,"sticky":true,"sticky_ttl_minutes":30,"quota_retry_minutes":1440,"rate_retry_seconds":45},"timeouts":{"request_timeout_sec":600,"acquire_timeout_sec":60},"audit":{"retention_days":30},"proxy":{"url":"","enabled":false,"runtime_status":"not_wired"},"client_keys":{"default_rpm_limit":0,"default_max_concurrent":0}}`)
	doc, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ClientKeys.DefaultRPMLimit != 0 || doc.ClientKeys.DefaultMaxConcurrent != 0 {
		t.Fatalf("explicit zeros mutated: %+v", doc.ClientKeys)
	}
}

