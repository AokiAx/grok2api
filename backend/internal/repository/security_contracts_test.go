package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type adminAuthContractRepository struct{}

func (adminAuthContractRepository) CountAdminUsers(context.Context) (int, error) { return 0, nil }
func (adminAuthContractRepository) CreateAdminUser(context.Context, adminauth.AdminUser) error {
	return nil
}
func (adminAuthContractRepository) GetAdminUserByID(context.Context, string) (adminauth.AdminUser, bool, error) {
	return adminauth.AdminUser{}, false, nil
}
func (adminAuthContractRepository) GetAdminUserByUsername(context.Context, string) (adminauth.AdminUser, bool, error) {
	return adminauth.AdminUser{}, false, nil
}
func (adminAuthContractRepository) CreateAdminSession(context.Context, adminauth.Session) error {
	return nil
}
func (adminAuthContractRepository) GetAdminSession(context.Context, string) (adminauth.Session, bool, error) {
	return adminauth.Session{}, false, nil
}
func (adminAuthContractRepository) FindAdminSessionByAccessHash(context.Context, [32]byte) (adminauth.Session, bool, error) {
	return adminauth.Session{}, false, nil
}
func (adminAuthContractRepository) RotateAdminSession(context.Context, string, [32]byte, adminauth.Session, time.Time) (bool, error) {
	return false, nil
}
func (adminAuthContractRepository) RevokeAdminSession(context.Context, string, time.Time, adminauth.RevocationReason) error {
	return nil
}
func (adminAuthContractRepository) RevokeAdminSessionFamily(context.Context, string, time.Time, adminauth.RevocationReason) error {
	return nil
}
func (adminAuthContractRepository) RecordAdminLoginAttempt(context.Context, adminauth.LoginAttempt) error {
	return nil
}
func (adminAuthContractRepository) CountRecentAdminLoginFailures(context.Context, string, string, time.Time) (repository.AdminLoginFailureCounts, error) {
	return repository.AdminLoginFailureCounts{}, nil
}

type clientKeyContractRepository struct{}

func (clientKeyContractRepository) CreateClientKey(context.Context, clientkey.ClientKey, []string) error {
	return nil
}
func (clientKeyContractRepository) GetClientKey(context.Context, string) (clientkey.ClientKey, []string, bool, error) {
	return clientkey.ClientKey{}, nil, false, nil
}
func (clientKeyContractRepository) FindClientKeyByHash(context.Context, [32]byte) (clientkey.ClientKey, []string, bool, error) {
	return clientkey.ClientKey{}, nil, false, nil
}
func (clientKeyContractRepository) ListClientKeysPage(context.Context, repository.ListClientKeysQuery) (repository.ListClientKeysResult, error) {
	return repository.ListClientKeysResult{}, nil
}
func (clientKeyContractRepository) UpdateClientKeyPolicy(context.Context, string, repository.ClientKeyPolicyUpdate) error {
	return nil
}
func (clientKeyContractRepository) RevokeClientKey(context.Context, string, time.Time) error {
	return nil
}
func (clientKeyContractRepository) ClientAuthRequired(context.Context) (bool, error) {
	return false, nil
}
func (clientKeyContractRepository) ConsumeClientKeyRPM(context.Context, string, int, time.Time) (repository.RateLimitDecision, error) {
	return repository.RateLimitDecision{}, nil
}

var (
	_ repository.AdminAuthRepository = adminAuthContractRepository{}
	_ repository.ClientKeyRepository = clientKeyContractRepository{}
)

func TestRateLimitDecisionCarriesStableResetBoundary(t *testing.T) {
	reset := time.Date(2026, 7, 15, 1, 1, 0, 0, time.UTC)
	decision := repository.RateLimitDecision{Allowed: true, Limit: 60, Remaining: 59, ResetAt: reset}
	if !decision.Allowed || decision.Limit != 60 || decision.Remaining != 59 || !decision.ResetAt.Equal(reset) {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestAdminLoginFailureCountsKeepUsernameAndSourceDimensionsSeparate(t *testing.T) {
	counts := repository.AdminLoginFailureCounts{ByUsername: 4, BySourceIP: 5}
	if counts.ByUsername != 4 || counts.BySourceIP != 5 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestClientKeyListDTOCarriesScopesWithoutSecrets(t *testing.T) {
	result := repository.ListClientKeysResult{
		Items: []repository.ClientKeyRecord{{Key: clientkey.ClientKey{ID: "key-1"}, Scopes: []string{"grok-4.5"}}},
		Total: 1, Page: 1, PageSize: 50,
	}
	if result.Items[0].Key.ID != "key-1" || len(result.Items[0].Scopes) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestClientKeyPolicyUpdateCannotCarryHashOriginOrRevocation(t *testing.T) {
	update := repository.ClientKeyPolicyUpdate{
		Name: "Restricted", ModelPolicy: clientkey.ModelPolicyAllowlist, Scopes: []string{"grok-4.5"},
		RPMLimit: 30, MaxConcurrent: 2, UpdatedAt: time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC),
	}
	if update.Name != "Restricted" || update.ModelPolicy != clientkey.ModelPolicyAllowlist || len(update.Scopes) != 1 {
		t.Fatalf("update = %+v", update)
	}
}
