// Package settings defines the managed runtime settings center.
// Secrets and bootstrap credentials stay outside this surface.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Document is the versioned, operator-editable settings document.
type Document struct {
	Revision   int64      `json:"revision"`
	UpdatedAt  time.Time  `json:"updated_at"`
	UpdatedBy  string     `json:"updated_by,omitempty"`
	Pool       Pool       `json:"pool"`
	Timeouts   Timeouts   `json:"timeouts"`
	Audit      Audit      `json:"audit"`
	Proxy      Proxy      `json:"proxy"`
	ClientKeys ClientKeys `json:"client_keys"`
	DeviceAuth DeviceAuth `json:"device_auth"`
	// DebugTrace toggles the temporary protocol interceptor (JSONL under data/traces).
	DebugTrace DebugTrace `json:"debug_trace"`
}

type Pool struct {
	MaxConcurrent     int    `json:"max_concurrent"`
	MaxAttempts       int    `json:"max_attempts"`
	Strategy          string `json:"strategy"`
	ActiveSize        int    `json:"active_size"`
	Sticky            bool   `json:"sticky"`
	StickyTTLMinutes  int    `json:"sticky_ttl_minutes"`
	QuotaRetryMinutes int    `json:"quota_retry_minutes"`
	RateRetrySeconds  int    `json:"rate_retry_seconds"`
}

type Timeouts struct {
	RequestTimeoutSec int `json:"request_timeout_sec"`
	AcquireTimeoutSec int `json:"acquire_timeout_sec"`
}

type Audit struct {
	RetentionDays int `json:"retention_days"`
}

// Proxy controls outbound HTTP(S) forward proxy for upstream + OIDC traffic.
// RuntimeStatus is filled by the API from the live client when possible.
type Proxy struct {
	URL           string `json:"url"`
	Enabled       bool   `json:"enabled"`
	RuntimeStatus string `json:"runtime_status"` // disabled | active | invalid
	Note          string `json:"note,omitempty"`
}


// DeviceAuth is OIDC device-flow configuration for Build Device OAuth import.
// Values are operator-editable defaults; secrets never live here.
type DeviceAuth struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
}

// ClientKeys holds operator defaults used when creating new client keys in the admin UI.
// Existing keys are unaffected; values are defaults only (0 still means unlimited when chosen explicitly).
type ClientKeys struct {
	DefaultRPMLimit      int `json:"default_rpm_limit"`
	DefaultMaxConcurrent int `json:"default_max_concurrent"`
}

// DebugTrace controls the temporary request interceptor for protocol debugging.
// Bodies are truncated; prompts/secrets should still be treated carefully.
type DebugTrace struct {
	Enabled    bool   `json:"enabled"`
	Dir        string `json:"dir"`
	ErrorsOnly bool   `json:"errors_only"`
}

func Defaults() Document {
	return Document{
		Revision:  1,
		UpdatedAt: time.Now().UTC(),
		Pool: Pool{
			MaxConcurrent:     4,
			MaxAttempts:       3,
			Strategy:          "round-robin",
			ActiveSize:        0,
			Sticky:            true,
			StickyTTLMinutes:  30,
			QuotaRetryMinutes: 1440,
			RateRetrySeconds:  45,
		},
		Timeouts: Timeouts{
			RequestTimeoutSec: 600,
			AcquireTimeoutSec: 60,
		},
		Audit: Audit{RetentionDays: 30},
		Proxy: Proxy{
			Enabled:       false,
			RuntimeStatus: "disabled",
			Note:          "HTTP(S) proxy for outbound Grok/OIDC traffic (e.g. http://privoxy:8118 via WARP).",
		},
		ClientKeys: ClientKeys{
			DefaultRPMLimit:      120,
			DefaultMaxConcurrent: 4,
		},
		DeviceAuth: DeviceAuth{
			Issuer:   "https://auth.x.ai",
			ClientID: "b1a00492-073a-47ea-816f-4c329264a828",
			Scope:    "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write",
		},
		DebugTrace: DebugTrace{
			Enabled:    false,
			Dir:        "",
			ErrorsOnly: true,
		},
	}
}

func (d *Document) Normalize() error {
	if d == nil {
		return errors.New("settings document is required")
	}
	if d.Pool.MaxConcurrent <= 0 {
		d.Pool.MaxConcurrent = 1
	}
	if d.Pool.MaxAttempts <= 0 {
		d.Pool.MaxAttempts = 3
	}
	switch strings.ToLower(strings.TrimSpace(d.Pool.Strategy)) {
	case "fill-first", "fill_first", "fillfirst":
		d.Pool.Strategy = "fill-first"
	default:
		d.Pool.Strategy = "round-robin"
	}
	if d.Pool.ActiveSize < 0 {
		d.Pool.ActiveSize = 0
	}
	if d.Pool.StickyTTLMinutes <= 0 {
		d.Pool.StickyTTLMinutes = 30
	}
	if d.Pool.QuotaRetryMinutes <= 0 {
		d.Pool.QuotaRetryMinutes = 1440
	}
	if d.Pool.RateRetrySeconds <= 0 {
		d.Pool.RateRetrySeconds = 45
	}
	if d.Timeouts.RequestTimeoutSec <= 0 {
		d.Timeouts.RequestTimeoutSec = 600
	}
	if d.Timeouts.AcquireTimeoutSec <= 0 {
		d.Timeouts.AcquireTimeoutSec = 60
	}
	if d.Audit.RetentionDays <= 0 {
		d.Audit.RetentionDays = 30
	}
	if d.Audit.RetentionDays > 365 {
		return fmt.Errorf("audit retention_days must be <= 365")
	}
	// Client key create-form defaults. 0 means "unlimited" as the form default
	// when an operator explicitly chooses it; product Defaults() uses 120 RPM / 4 concurrent.
	if d.ClientKeys.DefaultRPMLimit < 0 {
		return fmt.Errorf("client_keys.default_rpm_limit must be >= 0")
	}
	if d.ClientKeys.DefaultMaxConcurrent < 0 {
		return fmt.Errorf("client_keys.default_max_concurrent must be >= 0")
	}
	d.DeviceAuth.Issuer = strings.TrimRight(strings.TrimSpace(d.DeviceAuth.Issuer), "/")
	d.DeviceAuth.ClientID = strings.TrimSpace(d.DeviceAuth.ClientID)
	d.DeviceAuth.Scope = strings.Join(strings.Fields(d.DeviceAuth.Scope), " ")
	defaultsDA := Defaults().DeviceAuth
	if d.DeviceAuth.Issuer == "" {
		d.DeviceAuth.Issuer = defaultsDA.Issuer
	}
	if d.DeviceAuth.ClientID == "" {
		d.DeviceAuth.ClientID = defaultsDA.ClientID
	}
	if d.DeviceAuth.Scope == "" {
		d.DeviceAuth.Scope = defaultsDA.Scope
	}
	d.Proxy.URL = strings.TrimSpace(d.Proxy.URL)
	// Desired config status; live overlay may refine via ProxyRuntime API.
	if d.Proxy.Enabled && d.Proxy.URL != "" {
		if d.Proxy.RuntimeStatus != "invalid" {
			d.Proxy.RuntimeStatus = "active"
		}
	} else {
		d.Proxy.RuntimeStatus = "disabled"
	}
	if d.Proxy.Note == "" {
		d.Proxy.Note = "HTTP(S) proxy for outbound Grok/OIDC traffic (e.g. http://privoxy:8118 via WARP)."
	}
	d.DebugTrace.Dir = strings.TrimSpace(d.DebugTrace.Dir)
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = time.Now().UTC()
	}
	return nil
}

func (d Document) Marshal() ([]byte, error) {
	if err := d.Normalize(); err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

func Unmarshal(raw []byte) (Document, error) {
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Document{}, err
	}
	// Legacy documents lack client_keys; seed product defaults so create-key UX
	// has 120 RPM / 4 concurrency until the operator saves settings explicitly.
	if !jsonHasClientKeys(raw) {
		defaults := Defaults().ClientKeys
		doc.ClientKeys = defaults
	}
	if !jsonHasDeviceAuth(raw) {
		doc.DeviceAuth = Defaults().DeviceAuth
	}
	if !jsonHasDebugTrace(raw) {
		doc.DebugTrace = Defaults().DebugTrace
	}
	if err := doc.Normalize(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func jsonHasClientKeys(raw []byte) bool {
	var probe struct {
		ClientKeys json.RawMessage `json:"client_keys"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return len(probe.ClientKeys) > 0 && string(probe.ClientKeys) != "null"
}

func jsonHasDeviceAuth(raw []byte) bool {
	var probe struct {
		DeviceAuth json.RawMessage `json:"device_auth"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return len(probe.DeviceAuth) > 0 && string(probe.DeviceAuth) != "null"
}

func jsonHasDebugTrace(raw []byte) bool {
	var probe struct {
		DebugTrace json.RawMessage `json:"debug_trace"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return len(probe.DebugTrace) > 0 && string(probe.DebugTrace) != "null"
}
