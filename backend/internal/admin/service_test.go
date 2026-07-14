package admin_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/account"
	"github.com/AokiAx/grok2api/backend/internal/admin"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type memoryRepository struct {
	accounts map[string]account.Account
	saves    int
	deletes  int
}

type memorySink struct {
	items   []account.Account
	deleted []string
}

func (s *memorySink) Upsert(item account.Account) {
	s.items = append(s.items, item)
}

func (s *memorySink) Delete(id string) bool {
	s.deleted = append(s.deleted, id)
	return true
}

func (r *memoryRepository) ListAccounts(context.Context) ([]account.Account, error) {
	result := make([]account.Account, 0, len(r.accounts))
	for _, item := range r.accounts {
		result = append(result, item)
	}
	return result, nil
}

func (r *memoryRepository) ListAccountsPage(_ context.Context, query repository.ListAccountsQuery) (repository.ListAccountsResult, error) {
	all, _ := r.ListAccounts(context.Background())
	filtered := make([]account.Account, 0, len(all))
	pool := strings.ToLower(strings.TrimSpace(query.Pool))
	q := strings.ToLower(strings.TrimSpace(query.Q))
	for _, item := range all {
		if pool == "ready" || pool == "unavailable" {
			if string(item.Pool) != pool {
				continue
			}
		}
		if q != "" {
			haystack := strings.ToLower(strings.Join([]string{
				item.ID, item.Email, string(item.UnavailableReason), item.LastErrorCode,
			}, " "))
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	page := query.Page
	if page < 1 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	return repository.ListAccountsResult{
		Items:    filtered[start:end],
		Total:    len(filtered),
		Page:     page,
		PageSize: pageSize,
	}, nil
}

func (r *memoryRepository) AccountStats(context.Context) (repository.AccountStats, error) {
	all, _ := r.ListAccounts(context.Background())
	now := time.Now().UTC()
	soon := now.Add(time.Hour)
	stats := repository.AccountStats{
		Reasons:    map[string]int{},
		ErrorCodes: map[string]int{},
	}
	for _, item := range all {
		stats.TotalAccounts++
		if item.Pool == account.PoolReady {
			stats.ReadyAccounts++
		} else {
			stats.UnavailableAccounts++
			if item.UnavailableReason != "" {
				stats.Reasons[string(item.UnavailableReason)]++
			}
			if !item.RetryAt.IsZero() && !item.RetryAt.After(now) {
				stats.RetryDue++
			}
		}
		stats.TotalRequests += item.RequestCount
		maxActive := item.MaxActive
		if maxActive <= 0 {
			maxActive = 1
		}
		stats.MaxActive += maxActive
		if item.RefreshToken != "" {
			stats.RefreshableAccounts++
		} else {
			stats.NoRefreshToken++
		}
		if item.AuthenticationFails > 0 {
			stats.AuthFailAccounts++
			stats.TotalAuthFails += int64(item.AuthenticationFails)
		}
		if !item.ExpiresAt.IsZero() {
			if item.ExpiresAt.Before(now) {
				stats.AccessExpired++
			} else if item.ExpiresAt.Before(soon) {
				stats.AccessExpiringSoon++
			}
		}
		if item.LastErrorCode != "" {
			stats.ErrorCodes[item.LastErrorCode]++
		}
		if item.QuotaLimit > 0 {
			used := item.QuotaActual
			if used < 0 {
				used = 0
			}
			if used > item.QuotaLimit {
				used = item.QuotaLimit
			}
			remaining := item.QuotaLimit - used
			stats.QuotaActual += used
			stats.QuotaLimit += item.QuotaLimit
			stats.QuotaRemaining += remaining
			stats.QuotaObserved++
			if item.Pool == account.PoolReady {
				stats.ReadyQuotaRemaining += remaining
				stats.ReadyQuotaObserved++
			}
		}
	}
	return stats, nil
}

func (r *memoryRepository) SaveAccount(_ context.Context, item account.Account) error {
	if r.accounts == nil {
		r.accounts = map[string]account.Account{}
	}
	r.accounts[item.ID] = item
	r.saves++
	return nil
}

func (r *memoryRepository) DeleteAccount(_ context.Context, id string) error {
	delete(r.accounts, id)
	r.deletes++
	return nil
}

func (r *memoryRepository) GetAccount(_ context.Context, id string) (account.Account, bool, error) {
	item, ok := r.accounts[id]
	return item, ok, nil
}

func (r *memoryRepository) SaveAccounts(ctx context.Context, items []account.Account) error {
	for _, item := range items {
		if err := r.SaveAccount(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (r *memoryRepository) DeleteAccounts(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := r.DeleteAccount(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (r *memoryRepository) ListAccountEvents(_ context.Context, query repository.ListAccountEventsQuery) (repository.ListAccountEventsResult, error) {
	return repository.ListAccountEventsResult{Page: query.Page, PageSize: query.PageSize}, nil
}

type validator struct {
	reason account.UnavailableReason
	code   string
	err    error
}

func (v validator) Validate(context.Context, account.Account) (account.UnavailableReason, string, error) {
	return v.reason, v.code, v.err
}

func TestImportValidAccountEntersReadyPool(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})

	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{
			Key:          "access-token",
			RefreshToken: "refresh-token",
			Email:        "User@Example.com",
		}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Added != 1 || repository.saves != 1 {
		t.Fatalf("result = %#v saves=%d", result, repository.saves)
	}
	for _, item := range repository.accounts {
		if item.Pool != account.PoolReady {
			t.Fatalf("pool = %q; want ready", item.Pool)
		}
		if item.Email != "user@example.com" {
			t.Fatalf("email = %q", item.Email)
		}
	}
}

func TestImportAuthenticationFailureEntersUnavailablePool(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{
		reason: account.ReasonAuth,
		code:   "invalid-token",
	})

	_, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{Key: "bad-token"}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, item := range repository.accounts {
		if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonAuth {
			t.Fatalf("account = %#v", item)
		}
	}
}

func TestImportPreviewDoesNotPersist(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})

	result, err := service.Import(context.Background(), admin.ImportRequest{
		DryRun:   true,
		Accounts: []admin.ImportAccount{{Key: "preview-token"}},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if result.Added != 1 || repository.saves != 0 {
		t.Fatalf("result = %#v saves=%d", result, repository.saves)
	}
}

func TestImportValidatorInfrastructureErrorStopsImport(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{err: errors.New("network down")})

	_, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{Key: "token"}},
	})
	if err == nil {
		t.Fatal("expected validator error")
	}
}

func TestImportUpdatesExistingEmailAndAddsToScheduler(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"existing": {
			ID:           "existing",
			AccessToken:  "old-token",
			RefreshToken: "old-refresh",
			Email:        "user@example.com",
			Pool:         account.PoolUnavailable,
		},
	}}
	sink := &memorySink{}
	service := admin.NewService(repository, validator{}, admin.WithSink(sink))

	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{
			Key:   "new-token",
			Email: "USER@example.com",
		}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Updated != 1 || result.Added != 0 {
		t.Fatalf("result = %#v", result)
	}
	if repository.accounts["existing"].AccessToken != "new-token" {
		t.Fatalf("account = %#v", repository.accounts["existing"])
	}
	if len(sink.items) != 1 || sink.items[0].Pool != account.PoolReady {
		t.Fatalf("sink = %#v", sink.items)
	}
}

func TestImportRejectsMissingKey(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})
	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{Email: "user@example.com"}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Invalid != 1 || repository.saves != 0 {
		t.Fatalf("result = %#v saves=%d", result, repository.saves)
	}
}

func TestDeleteRemovesAccountFromRepositoryAndScheduler(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"account-1": {ID: "account-1", Pool: account.PoolReady},
	}}
	sink := &memorySink{}
	service := admin.NewService(repository, validator{}, admin.WithSink(sink))

	if err := service.Delete(context.Background(), "account-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if repository.deletes != 1 || len(repository.accounts) != 0 {
		t.Fatalf("repository = %#v deletes=%d", repository.accounts, repository.deletes)
	}
	if len(sink.deleted) != 1 || sink.deleted[0] != "account-1" {
		t.Fatalf("sink deleted = %#v", sink.deleted)
	}
}

func TestDeleteMissingAccountReturnsError(t *testing.T) {
	service := admin.NewService(&memoryRepository{}, validator{})
	if err := service.Delete(context.Background(), "missing"); !errors.Is(err, admin.ErrAccountNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestRecoverValidatesUnavailableAccountBeforeReturningReady(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"account-1": {
			ID:                "account-1",
			AccessToken:       "token",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			RetryAt:           time.Now().Add(time.Hour),
		},
	}}
	sink := &memorySink{}
	service := admin.NewService(repository, validator{}, admin.WithSink(sink))

	item, err := service.Recover(context.Background(), "account-1")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if item.Pool != account.PoolReady || item.UnavailableReason != "" || !item.RetryAt.IsZero() {
		t.Fatalf("account = %#v", item)
	}
	if len(sink.items) != 1 || sink.items[0].Pool != account.PoolReady {
		t.Fatalf("sink = %#v", sink.items)
	}
}

func TestRecoverKeepsRejectedCredentialUnavailable(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"account-1": {ID: "account-1", AccessToken: "bad", Pool: account.PoolUnavailable},
	}}
	service := admin.NewService(repository, validator{reason: account.ReasonAuth}, admin.WithSink(&memorySink{}))

	item, err := service.Recover(context.Background(), "account-1")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonAuth {
		t.Fatalf("account = %#v", item)
	}
}

func TestRecoverMissingAccountReturnsError(t *testing.T) {
	service := admin.NewService(&memoryRepository{}, validator{})
	if _, err := service.Recover(context.Background(), "missing"); err == nil {
		t.Fatal("missing account should fail")
	}
}

func TestImportAcceptsAccessTokenAlias(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})

	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{
			AccessToken:  "legacy-access",
			RefreshToken: "legacy-refresh",
			Email:        "alias@example.com",
		}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Added != 1 || repository.saves != 1 {
		t.Fatalf("result = %#v saves=%d", result, repository.saves)
	}
	for _, item := range repository.accounts {
		if item.AccessToken != "legacy-access" {
			t.Fatalf("account = %#v", item)
		}
	}
}

func TestServiceList(t *testing.T) {
	repository := &memoryRepository{accounts: map[string]account.Account{
		"a": {ID: "a", Pool: account.PoolReady},
	}}
	service := admin.NewService(repository, validator{})
	items, err := service.List(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("list=%#v err=%v", items, err)
	}
}

func TestImportAuthMapKeyUsesUserIDAndExpiresAt(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})

	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{
			ID:           "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828::user-123",
			Key:          "access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    "2026-07-10T17:06:19.000000000Z",
			OIDCIssuer:   "https://auth.x.ai",
			OIDCClientID: "b1a00492-073a-47ea-816f-4c329264a828",
			UserID:       "user-123",
		}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Added != 1 || repository.saves != 1 {
		t.Fatalf("result=%#v saves=%d", result, repository.saves)
	}
	item := repository.accounts["user-123"]
	if item.ID != "user-123" {
		t.Fatalf("id=%q", item.ID)
	}
	if item.UserID != "user-123" {
		t.Fatalf("user_id=%q", item.UserID)
	}
	if item.ExpiresAt.IsZero() || item.ExpiresAt.Year() != 2026 {
		t.Fatalf("expires_at=%v", item.ExpiresAt)
	}
	if item.RefreshToken != "refresh-token" {
		t.Fatalf("refresh=%q", item.RefreshToken)
	}
}

func TestImportDerivesUserIDFromAuthMapKeyWhenMissing(t *testing.T) {
	repository := &memoryRepository{}
	service := admin.NewService(repository, validator{})

	result, err := service.Import(context.Background(), admin.ImportRequest{
		Accounts: []admin.ImportAccount{{
			ID:  "https://auth.x.ai::client::uid-9",
			Key: "token-9",
		}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("result=%#v", result)
	}
	item := repository.accounts["uid-9"]
	if item.UserID != "uid-9" || item.ID != "uid-9" {
		t.Fatalf("account=%#v", item)
	}
}
