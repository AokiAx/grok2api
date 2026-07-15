package settings

import (
	"strings"
	"testing"
)

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
	if doc.Proxy.RuntimeStatus != "active" {
		t.Fatalf("runtime status=%q want active", doc.Proxy.RuntimeStatus)
	}
	if doc.Proxy.URL != "http://127.0.0.1:8118" {
		t.Fatalf("url=%q", doc.Proxy.URL)
	}
	if doc.ClientKeys.DefaultRPMLimit != 120 || doc.ClientKeys.DefaultMaxConcurrent != 4 {
		t.Fatalf("client key defaults=%+v", doc.ClientKeys)
	}
	if doc.DeviceAuth.ClientID == "" || !strings.Contains(doc.DeviceAuth.Scope, "offline_access") {
		t.Fatalf("device auth defaults=%+v", doc.DeviceAuth)
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

func TestUnmarshalSeedsLegacyDeviceAuth(t *testing.T) {
	raw := []byte(`{"revision":2,"pool":{"max_concurrent":4,"max_attempts":3,"strategy":"round-robin","active_size":0,"sticky":true,"sticky_ttl_minutes":30,"quota_retry_minutes":1440,"rate_retry_seconds":45},"timeouts":{"request_timeout_sec":600,"acquire_timeout_sec":60},"audit":{"retention_days":30},"proxy":{"url":"","enabled":false},"client_keys":{"default_rpm_limit":120,"default_max_concurrent":4}}`)
	doc, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if doc.DeviceAuth.Issuer != "https://auth.x.ai" {
		t.Fatalf("issuer=%q", doc.DeviceAuth.Issuer)
	}
	if doc.DeviceAuth.ClientID != "b1a00492-073a-47ea-816f-4c329264a828" {
		t.Fatalf("client_id=%q", doc.DeviceAuth.ClientID)
	}
	if doc.DebugTrace.Enabled || !doc.DebugTrace.ErrorsOnly {
		t.Fatalf("debug_trace defaults=%+v", doc.DebugTrace)
	}
}

func TestUnmarshalKeepsExplicitDebugTrace(t *testing.T) {
	raw := []byte(`{"revision":4,"pool":{"max_concurrent":4,"max_attempts":3,"strategy":"round-robin","active_size":0,"sticky":true,"sticky_ttl_minutes":30,"quota_retry_minutes":1440,"rate_retry_seconds":45},"timeouts":{"request_timeout_sec":600,"acquire_timeout_sec":60},"audit":{"retention_days":30},"proxy":{"url":"","enabled":false,"runtime_status":"not_wired"},"client_keys":{"default_rpm_limit":120,"default_max_concurrent":4},"device_auth":{"issuer":"https://auth.x.ai","client_id":"x","scope":"openid"},"debug_trace":{"enabled":true,"dir":"/tmp/traces","errors_only":false}}`)
	doc, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.DebugTrace.Enabled || doc.DebugTrace.ErrorsOnly || doc.DebugTrace.Dir != "/tmp/traces" {
		t.Fatalf("debug_trace=%+v", doc.DebugTrace)
	}
}

func TestProxyNormalizeDesiredStatus(t *testing.T) {
	doc := Defaults()
	doc.Proxy.Enabled = false
	doc.Proxy.URL = ""
	if err := doc.Normalize(); err != nil {
		t.Fatal(err)
	}
	if doc.Proxy.RuntimeStatus != "disabled" {
		t.Fatalf("status=%q", doc.Proxy.RuntimeStatus)
	}
	doc.Proxy.Enabled = true
	doc.Proxy.URL = " http://privoxy:8118 "
	if err := doc.Normalize(); err != nil {
		t.Fatal(err)
	}
	if doc.Proxy.URL != "http://privoxy:8118" || doc.Proxy.RuntimeStatus != "active" {
		t.Fatalf("proxy=%+v", doc.Proxy)
	}
}
