package deviceauth

import (
	"errors"
	"strings"
	"time"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusSlowDown  Status = "slow_down"
	StatusExpired   Status = "expired"
	StatusDenied    Status = "denied"
	StatusCancelled Status = "cancelled"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// Session is a Build Device OAuth authorization attempt.
// DeviceCode and tokens never leave the server boundary.
type Session struct {
	ID                      string
	Status                  Status
	Issuer                  string
	ClientID                string
	Scope                   string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	DeviceCode              string // server-only
	IntervalSec             int
	ExpiresAt               time.Time
	LastError               string
	AccountID               string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	CompletedAt             time.Time
}

func (s *Session) Normalize() error {
	if s == nil {
		return errors.New("device auth session is required")
	}
	s.ID = strings.TrimSpace(s.ID)
	if s.ID == "" {
		return errors.New("session id is required")
	}
	if s.Status == "" {
		s.Status = StatusPending
	}
	s.Issuer = strings.TrimRight(strings.TrimSpace(s.Issuer), "/")
	if s.Issuer == "" {
		s.Issuer = "https://auth.x.ai"
	}
	s.ClientID = strings.TrimSpace(s.ClientID)
	if s.ClientID == "" {
		return errors.New("client_id is required")
	}
	if s.IntervalSec <= 0 {
		s.IntervalSec = 5
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	s.UpdatedAt = time.Now().UTC()
	return nil
}

func (s Session) Public() map[string]any {
	out := map[string]any{
		"id":               s.ID,
		"status":           string(s.Status),
		"issuer":           s.Issuer,
		"client_id":        s.ClientID,
		"scope":            s.Scope,
		"user_code":        s.UserCode,
		"verification_uri": s.VerificationURI,
		"interval_sec":     s.IntervalSec,
		"expires_at":       s.ExpiresAt.UTC().Format(time.RFC3339),
		"created_at":       s.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":       s.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if s.VerificationURIComplete != "" {
		out["verification_uri_complete"] = s.VerificationURIComplete
	}
	if s.LastError != "" {
		out["last_error"] = s.LastError
	}
	if s.AccountID != "" {
		out["account_id"] = s.AccountID
	}
	if !s.CompletedAt.IsZero() {
		out["completed_at"] = s.CompletedAt.UTC().Format(time.RFC3339)
	}
	// Intentionally omit device_code and tokens.
	return out
}
