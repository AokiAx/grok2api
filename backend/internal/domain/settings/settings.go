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
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	Pool      Pool      `json:"pool"`
	Timeouts  Timeouts  `json:"timeouts"`
	Audit     Audit     `json:"audit"`
	Proxy     Proxy     `json:"proxy"`
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

// Proxy is stored/versioned but not applied to the runtime yet.
type Proxy struct {
	URL           string `json:"url"`
	Enabled       bool   `json:"enabled"`
	RuntimeStatus string `json:"runtime_status"` // always "not_wired" until implemented
	Note          string `json:"note,omitempty"`
}

// Snapshot is an immutable historical revision.
type Snapshot struct {
	Revision  int64
	CreatedAt time.Time
	CreatedBy string
	Reason    string
	Document  Document
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
			RuntimeStatus: "not_wired",
			Note:          "Proxy settings are stored for future use and do not affect runtime outbound traffic yet.",
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
	d.Proxy.URL = strings.TrimSpace(d.Proxy.URL)
	d.Proxy.RuntimeStatus = "not_wired"
	if d.Proxy.Note == "" {
		d.Proxy.Note = "Proxy settings are stored for future use and do not affect runtime outbound traffic yet."
	}
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
	if err := doc.Normalize(); err != nil {
		return Document{}, err
	}
	return doc, nil
}
