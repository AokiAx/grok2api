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
	KeyHash       [32]byte
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

func (k *ClientKey) Revoke(at time.Time) {
	if k == nil || !k.RevokedAt.IsZero() {
		return
	}
	k.RevokedAt = normalizeTime(at)
	k.UpdatedAt = k.RevokedAt
}

func (k ClientKey) UnlimitedRPM() bool {
	return k.RPMLimit == 0
}

func (k ClientKey) UnlimitedConcurrency() bool {
	return k.MaxConcurrent == 0
}

func (k ClientKey) AllowsModel(model string, scopes []string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if k.ModelPolicy == ModelPolicyAll {
		return true
	}
	if k.ModelPolicy != ModelPolicyAllowlist {
		return false
	}
	for _, scope := range scopes {
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
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
