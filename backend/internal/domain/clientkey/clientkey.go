// Package clientkey owns client credential policy and lifecycle rules without
// exposing raw client secrets to the rest of the application.
package clientkey

import (
	"errors"
	"sort"
	"strings"
	"time"
)

type Origin string

const (
	OriginManaged      Origin = "managed"
	OriginConfigAPIKey Origin = "config_api_key"
)

type ModelPolicy string

const (
	ModelPolicyAll       ModelPolicy = "all"
	ModelPolicyAllowlist ModelPolicy = "allowlist"
)

type ClientKey struct {
	ID            string
	Name          string
	Origin        Origin
	KeyHash       [32]byte `json:"-"`
	KeyPrefix     string
	ModelPolicy   ModelPolicy
	RPMLimit      int
	MaxConcurrent int
	ExpiresAt     time.Time
	RevokedAt     time.Time
	LastUsedAt    time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (k *ClientKey) NormalizeAndValidate(scopes []string) ([]string, error) {
	if k == nil {
		return nil, errors.New("client key is required")
	}
	k.ID = strings.TrimSpace(k.ID)
	k.Name = strings.TrimSpace(k.Name)
	k.KeyPrefix = strings.TrimSpace(k.KeyPrefix)
	if k.ID == "" || k.Name == "" || k.KeyPrefix == "" {
		return nil, errors.New("client key id, name, and prefix are required")
	}
	if k.Origin != OriginManaged && k.Origin != OriginConfigAPIKey {
		return nil, errors.New("unsupported client key origin")
	}
	if k.KeyHash == ([32]byte{}) {
		return nil, errors.New("client key hash is required")
	}
	if k.RPMLimit < 0 {
		return nil, errors.New("rpm limit must be non-negative")
	}
	if k.MaxConcurrent < 0 {
		return nil, errors.New("max concurrent must be non-negative")
	}
	if k.CreatedAt.IsZero() || k.UpdatedAt.IsZero() {
		return nil, errors.New("client key creation and update times are required")
	}
	k.CreatedAt = k.CreatedAt.UTC()
	k.UpdatedAt = k.UpdatedAt.UTC()
	if k.UpdatedAt.Before(k.CreatedAt) {
		return nil, errors.New("client key update time cannot precede creation")
	}

	normalizedScopes, err := normalizeScopes(scopes)
	if err != nil {
		return nil, err
	}
	switch k.ModelPolicy {
	case ModelPolicyAll:
		if len(normalizedScopes) != 0 {
			return nil, errors.New("all-model policy cannot carry model scopes")
		}
	case ModelPolicyAllowlist:
		if len(normalizedScopes) == 0 {
			return nil, errors.New("allowlist policy requires at least one model scope")
		}
	default:
		return nil, errors.New("unsupported model policy")
	}
	return normalizedScopes, nil
}

func (k ClientKey) Active(at time.Time) bool {
	at = normalizeTime(at)
	return k.RevokedAt.IsZero() && (k.ExpiresAt.IsZero() || at.Before(k.ExpiresAt))
}

func (k *ClientKey) Revoke(at time.Time) error {
	if k == nil {
		return errors.New("client key is required")
	}
	if at.IsZero() {
		return errors.New("client key revocation time is required")
	}
	if !k.RevokedAt.IsZero() {
		return nil
	}
	k.RevokedAt = normalizeTime(at)
	k.UpdatedAt = k.RevokedAt
	return nil
}

func (k ClientKey) UnlimitedRPM() bool {
	return k.RPMLimit == 0
}

func (k ClientKey) UnlimitedConcurrency() bool {
	return k.MaxConcurrent == 0
}

type Credential struct {
	Key    ClientKey
	scopes []string
}

func NewCredential(key ClientKey, scopes []string) (Credential, error) {
	normalized, err := key.NormalizeAndValidate(scopes)
	if err != nil {
		return Credential{}, err
	}
	return Credential{Key: key, scopes: normalized}, nil
}

func (c Credential) Scopes() []string {
	return append([]string(nil), c.scopes...)
}

func (c Credential) AllowsModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if c.Key.ModelPolicy == ModelPolicyAll {
		return true
	}
	if c.Key.ModelPolicy != ModelPolicyAllowlist {
		return false
	}
	for _, scope := range c.scopes {
		if strings.ToLower(strings.TrimSpace(scope)) == model {
			return true
		}
	}
	return false
}

func normalizeScopes(scopes []string) ([]string, error) {
	seen := make(map[string]struct{}, len(scopes))
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope == "" {
			return nil, errors.New("model scope cannot be empty")
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeTime(value time.Time) time.Time {
	return value.UTC()
}
