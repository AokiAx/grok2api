package clientkey

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
)

func TestClientKeyValidationFreezesPolicyAndLimitSemantics(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("secret"))
	key := ClientKey{
		ID:            " key-1 ",
		Name:          " Primary ",
		Origin:        OriginManaged,
		KeyHash:       hash,
		KeyPrefix:     "g2a_abcd",
		ModelPolicy:   ModelPolicyAllowlist,
		RPMLimit:      60,
		MaxConcurrent: 3,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	scopes := []string{" grok-4.5 ", "GROK-4.5", "grok-code-fast-1"}

	gotScopes, err := key.NormalizeAndValidate(scopes)
	if err != nil {
		t.Fatalf("validate key: %v", err)
	}
	if key.ID != "key-1" || key.Name != "Primary" || key.KeyHash != hash {
		t.Fatalf("normalized key = %+v", key)
	}
	if len(gotScopes) != 2 || gotScopes[0] != "grok-4.5" || gotScopes[1] != "grok-code-fast-1" {
		t.Fatalf("normalized scopes = %#v", gotScopes)
	}
	if key.UnlimitedRPM() || key.UnlimitedConcurrency() {
		t.Fatalf("finite limits reported as unlimited: %+v", key)
	}

	unlimited := key
	unlimited.ModelPolicy = ModelPolicyAll
	unlimited.RPMLimit = 0
	unlimited.MaxConcurrent = 0
	allScopes, err := unlimited.NormalizeAndValidate(nil)
	if err != nil || len(allScopes) != 0 || !unlimited.UnlimitedRPM() || !unlimited.UnlimitedConcurrency() {
		t.Fatalf("unlimited all-model key = %+v scopes=%#v err=%v", unlimited, allScopes, err)
	}
}

func TestClientKeyRejectsInvalidScopesLimitsAndLifecycle(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("secret"))
	base := ClientKey{
		ID: "key-1", Name: "Primary", Origin: OriginManaged, KeyHash: hash,
		KeyPrefix: "g2a_abcd", ModelPolicy: ModelPolicyAll, CreatedAt: now, UpdatedAt: now,
	}

	tests := []struct {
		name   string
		mutate func(*ClientKey)
		scopes []string
	}{
		{name: "all rejects scopes", scopes: []string{"grok-4.5"}},
		{name: "allowlist requires scope", mutate: func(k *ClientKey) { k.ModelPolicy = ModelPolicyAllowlist }},
		{name: "negative rpm", mutate: func(k *ClientKey) { k.RPMLimit = -1 }},
		{name: "negative concurrency", mutate: func(k *ClientKey) { k.MaxConcurrent = -1 }},
		{name: "unknown origin", mutate: func(k *ClientKey) { k.Origin = "copied" }},
		{name: "zero hash", mutate: func(k *ClientKey) { k.KeyHash = [32]byte{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := base
			if tt.mutate != nil {
				tt.mutate(&item)
			}
			if _, err := item.NormalizeAndValidate(tt.scopes); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	active := base
	active.ExpiresAt = now.Add(time.Hour)
	if !active.Active(now) || active.Active(now.Add(time.Hour)) {
		t.Fatalf("expiry boundary is incorrect: %+v", active)
	}
	active.Revoke(now.Add(time.Minute))
	if active.Active(now.Add(2*time.Minute)) || active.RevokedAt.IsZero() {
		t.Fatalf("revoked key remained active: %+v", active)
	}
	firstRevocation := active.RevokedAt
	active.Revoke(now.Add(3 * time.Minute))
	if !active.RevokedAt.Equal(firstRevocation) {
		t.Fatalf("revocation should be irreversible/idempotent: first=%v second=%v", firstRevocation, active.RevokedAt)
	}
}

func TestClientKeyModelAuthorization(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("secret"))
	all, err := NewCredential(ClientKey{
		ID: "all", Name: "All", Origin: OriginManaged, KeyHash: hash, KeyPrefix: "g2a_all",
		ModelPolicy: ModelPolicyAll, CreatedAt: now, UpdatedAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("new all credential: %v", err)
	}
	if !all.AllowsModel("anything") {
		t.Fatal("all policy should authorize any non-empty model")
	}
	allowlistKey := all.Key
	allowlistKey.ID = "allowlist"
	allowlistKey.Name = "Allowlist"
	allowlistKey.ModelPolicy = ModelPolicyAllowlist
	allowlist, err := NewCredential(allowlistKey, []string{"grok-4.5"})
	if err != nil {
		t.Fatalf("new allowlist credential: %v", err)
	}
	if !allowlist.AllowsModel("GROK-4.5") {
		t.Fatal("allowlist comparison should be normalized")
	}
	if allowlist.AllowsModel("grok-3") || allowlist.AllowsModel("") {
		t.Fatal("allowlist authorized an unscoped model")
	}
	returnedScopes := allowlist.Scopes()
	returnedScopes[0] = "grok-3"
	if allowlist.AllowsModel("grok-3") {
		t.Fatal("mutating returned scopes changed authorization")
	}
}

func TestClientKeyJSONNeverSerializesKeyHash(t *testing.T) {
	item := ClientKey{ID: "key-1", KeyHash: sha256.Sum256([]byte("sensitive"))}
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, found := payload["KeyHash"]; found {
		t.Fatalf("key hash was serialized: %s", data)
	}
}

func TestClientKeyRejectsZeroSecurityTimestamps(t *testing.T) {
	hash := sha256.Sum256([]byte("secret"))
	item := ClientKey{
		ID: "key-1", Name: "Key", Origin: OriginManaged, KeyHash: hash,
		KeyPrefix: "g2a_key", ModelPolicy: ModelPolicyAll,
	}
	if _, err := item.NormalizeAndValidate(nil); err == nil {
		t.Fatal("zero created/updated timestamps should be rejected")
	}
}
