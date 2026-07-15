// Package modelreg defines the managed model registry used by the gateway catalog.
package modelreg

import (
	"errors"
	"strings"
	"time"
)

// Model is a public-facing model identity with optional upstream mapping.
type Model struct {
	ID                      string
	UpstreamID              string
	Name                    string
	APIBackend              string
	ContextWindow           int
	SupportsReasoningEffort bool
	ReasoningEfforts        []string
	SupportsBackendSearch   bool
	OwnedBy                 string
	Enabled                 bool
	Aliases                 []string
	Source                  string // seed | managed
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

func (m Model) PublicID() string {
	return strings.TrimSpace(m.ID)
}

func (m Model) ResolveUpstream() string {
	if u := strings.TrimSpace(m.UpstreamID); u != "" {
		return u
	}
	return m.PublicID()
}

func (m *Model) Normalize() error {
	if m == nil {
		return errors.New("model is required")
	}
	m.ID = strings.TrimSpace(m.ID)
	if m.ID == "" {
		return errors.New("model id is required")
	}
	m.UpstreamID = strings.TrimSpace(m.UpstreamID)
	if m.UpstreamID == "" {
		m.UpstreamID = m.ID
	}
	m.Name = strings.TrimSpace(m.Name)
	m.APIBackend = strings.TrimSpace(m.APIBackend)
	if m.APIBackend == "" {
		m.APIBackend = "responses"
	}
	m.OwnedBy = strings.TrimSpace(m.OwnedBy)
	if m.OwnedBy == "" {
		m.OwnedBy = "xai"
	}
	if m.Source == "" {
		m.Source = "managed"
	}
	seen := map[string]struct{}{}
	aliases := make([]string, 0, len(m.Aliases))
	for _, alias := range m.Aliases {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias == "" || alias == strings.ToLower(m.ID) {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	m.Aliases = aliases
	return nil
}
