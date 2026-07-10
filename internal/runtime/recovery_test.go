package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/runtime"
	"github.com/AokiAx/grok2api/internal/scheduler"
)

type recoveryStore struct {
	accounts []account.Account
	saved    []account.Account
}

func (s *recoveryStore) SaveAccount(_ context.Context, item account.Account) error {
	s.saved = append(s.saved, item)
	// keep list in sync for subsequent recover loops in same test
	for index := range s.accounts {
		if s.accounts[index].ID == item.ID {
			s.accounts[index] = item
			return nil
		}
	}
	s.accounts = append(s.accounts, item)
	return nil
}

func (s *recoveryStore) ListAccounts(context.Context) ([]account.Account, error) {
	return append([]account.Account(nil), s.accounts...), nil
}

type credentialRefresher struct {
	item account.Account
	err  error
}

func (r credentialRefresher) Refresh(context.Context, account.Account) (account.Account, error) {
	return r.item, r.err
}

type credentialValidator struct {
	reason account.UnavailableReason
	code   string
	err    error
}

func (v credentialValidator) Validate(context.Context, account.Account) (account.UnavailableReason, string, error) {
	return v.reason, v.code, v.err
}

type quotaProber struct {
	reason account.UnavailableReason
	code   string
	err    error
	calls  int
}

func (p *quotaProber) ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error) {
	p.calls++
	return p.reason, p.code, p.err
}

func TestRecoverDuePromotesCooldownButNotQuota(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	s := scheduler.New([]account.Account{
		{
			ID:                "quota",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			RetryAt:           now.Add(-time.Minute),
			MaxActive:         1,
		},
		{
			ID:                "cooldown",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonCooldown,
			RetryAt:           now.Add(-time.Minute),
			MaxActive:         1,
		},
		{
			ID:                "auth",
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonAuth,
			RetryAt:           now.Add(-time.Minute),
			MaxActive:         1,
		},
	})
	store := &recoveryStore{}

	if err := runtime.RecoverDue(context.Background(), s, store, now); err != nil {
		t.Fatalf("recover due: %v", err)
	}
	if len(store.saved) != 1 || store.saved[0].ID != "cooldown" {
		t.Fatalf("saved = %#v; want only cooldown", store.saved)
	}
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.Account().ID != "cooldown" {
		t.Fatalf("selected %q; want cooldown", lease.Account().ID)
	}
	lease.Release()
}

func TestRecoverQuotaPromotesOnlyAfterSuccessfulProbe(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	item := account.Account{
		ID:                "quota",
		AccessToken:       "token",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(-time.Minute),
		MaxActive:         1,
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	pool := scheduler.New([]account.Account{item})
	prober := &quotaProber{}

	result, err := runtime.RecoverQuota(
		context.Background(),
		pool,
		store,
		prober,
		nil,
		now,
		30*time.Minute,
	)
	if err != nil {
		t.Fatalf("recover quota: %v", err)
	}
	if result.Recovered != 1 || prober.calls != 1 {
		t.Fatalf("result=%#v calls=%d", result, prober.calls)
	}
	if len(store.saved) != 1 || store.saved[0].Pool != account.PoolReady {
		t.Fatalf("saved = %#v", store.saved)
	}
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire recovered: %v", err)
	}
	if lease.Account().ID != "quota" {
		t.Fatalf("selected %q", lease.Account().ID)
	}
	lease.Release()
}

func TestRecoverQuotaDefersWhenProbeStillExhausted(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	item := account.Account{
		ID:                "quota",
		AccessToken:       "token",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(-time.Minute),
		MaxActive:         1,
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	pool := scheduler.New([]account.Account{item})
	prober := &quotaProber{reason: account.ReasonQuota, code: "subscription:free-usage-exhausted"}

	result, err := runtime.RecoverQuota(
		context.Background(),
		pool,
		store,
		prober,
		nil,
		now,
		45*time.Minute,
	)
	if err != nil {
		t.Fatalf("recover quota: %v", err)
	}
	if result.Deferred != 1 || result.Recovered != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved = %#v", store.saved)
	}
	got := store.saved[0]
	if got.Pool != account.PoolUnavailable || got.UnavailableReason != account.ReasonQuota {
		t.Fatalf("saved state = %#v", got)
	}
	if !got.RetryAt.Equal(now.Add(45 * time.Minute)) {
		t.Fatalf("retry_at = %s; want +45m", got.RetryAt)
	}
	if pool.ReadyCount() != 0 {
		t.Fatalf("ready count = %d; want 0", pool.ReadyCount())
	}
}

func TestRecoverQuotaSkipsFutureRetryAt(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	item := account.Account{
		ID:                "quota",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(10 * time.Minute),
		MaxActive:         1,
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	pool := scheduler.New([]account.Account{item})
	prober := &quotaProber{}

	result, err := runtime.RecoverQuota(
		context.Background(),
		pool,
		store,
		prober,
		nil,
		now,
		30*time.Minute,
	)
	if err != nil {
		t.Fatalf("recover quota: %v", err)
	}
	if result.Skipped != 1 || prober.calls != 0 || len(store.saved) != 0 {
		t.Fatalf("result=%#v calls=%d saved=%d", result, prober.calls, len(store.saved))
	}
}

func TestRunRecoveryStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runtime.RunRecovery(
		ctx,
		scheduler.New(nil),
		&recoveryStore{},
		time.Hour,
	)
	if err != nil {
		t.Fatalf("run recovery: %v", err)
	}
}

func TestRecoverCredentialsRefreshesAndRestoresAuthAccount(t *testing.T) {
	now := time.Now().UTC()
	expired := account.Account{
		ID: "expired", AccessToken: "old", RefreshToken: "refresh",
		OIDCIssuer: "https://auth.x.ai", OIDCClientID: "client",
		Pool: account.PoolUnavailable, UnavailableReason: account.ReasonAuth,
	}
	refreshed := expired
	refreshed.AccessToken = "new"
	refreshed.ExpiresAt = now.Add(time.Hour)
	store := &recoveryStore{accounts: []account.Account{expired}}
	pool := scheduler.New([]account.Account{expired})

	result, err := runtime.RecoverCredentials(
		context.Background(), pool, store,
		credentialRefresher{item: refreshed}, credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover credentials: %v", err)
	}
	if result.Recovered != 1 || result.Failed != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(store.saved) != 1 || store.saved[0].Pool != account.PoolReady || store.saved[0].AccessToken != "new" {
		t.Fatalf("saved = %#v", store.saved)
	}
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire refreshed: %v", err)
	}
	lease.Release()
}

func TestRecoverCredentialsBacksOffRejectedRefresh(t *testing.T) {
	now := time.Now().UTC()
	expired := account.Account{
		ID: "expired", RefreshToken: "bad", Pool: account.PoolUnavailable,
		UnavailableReason: account.ReasonAuth,
	}
	store := &recoveryStore{accounts: []account.Account{expired}}
	result, err := runtime.RecoverCredentials(
		context.Background(), scheduler.New([]account.Account{expired}), store,
		credentialRefresher{err: context.DeadlineExceeded}, credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover credentials: %v", err)
	}
	if result.Failed != 1 || len(store.saved) != 1 {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
	if !store.saved[0].RetryAt.After(now.Add(20*time.Minute)) || store.saved[0].LastErrorCode != "refresh-failed" {
		t.Fatalf("saved = %#v", store.saved[0])
	}
}

func TestRunRecoveryOptionsWire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &recoveryStore{}
	pool := scheduler.New(nil)
	err := runtime.RunRecovery(
		ctx,
		pool,
		store,
		time.Hour,
		runtime.WithCredentialRecovery(store, credentialRefresher{}, credentialValidator{}),
		runtime.WithQuotaProber(quotaProbe{}),
		runtime.WithQuotaRetry(15*time.Minute),
	)
	if err != nil {
		t.Fatalf("run recovery: %v", err)
	}
}

type quotaProbe struct{}

func (quotaProbe) ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error) {
	return "", "", nil
}

type errQuotaProbe struct{}

func (errQuotaProbe) ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error) {
	return "", "", context.DeadlineExceeded
}

func TestRecoverQuotaTransportErrorDefers(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	quota := account.Account{ID: "q", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota, RetryAt: now.Add(-time.Minute)}
	store := &recoveryStore{accounts: []account.Account{quota}}
	pool := scheduler.New([]account.Account{quota})
	result, err := runtime.RecoverQuota(context.Background(), pool, store, errQuotaProbe{}, credentialValidator{}, now, time.Hour)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Failed != 1 || len(store.saved) != 1 {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
}

func TestRecoverQuotaUsesValidatorWhenProberNil(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	quota := account.Account{ID: "q", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota, RetryAt: now.Add(-time.Minute)}
	store := &recoveryStore{accounts: []account.Account{quota}}
	pool := scheduler.New([]account.Account{quota})
	result, err := runtime.RecoverQuota(context.Background(), pool, store, nil, credentialValidator{}, now, time.Hour)
	if err != nil || result.Recovered != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRecoverCredentialsMarksRevokedRefreshAsUnrecoverable(t *testing.T) {
	now := time.Now().UTC()
	expired := account.Account{
		ID: "expired", RefreshToken: "dead", Pool: account.PoolUnavailable,
		UnavailableReason: account.ReasonAuth,
	}
	store := &recoveryStore{accounts: []account.Account{expired}}
	result, err := runtime.RecoverCredentials(
		context.Background(), scheduler.New([]account.Account{expired}), store,
		credentialRefresher{err: &permanentRefreshError{msg: "invalid_grant: Refresh token has been revoked"}}, credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover credentials: %v", err)
	}
	if result.Revoked != 1 || result.Failed != 0 {
		t.Fatalf("result=%#v", result)
	}
	if len(store.saved) != 1 || store.saved[0].LastErrorCode != "refresh-revoked" {
		t.Fatalf("saved=%#v", store.saved)
	}
	if !store.saved[0].RetryAt.After(now.Add(24 * time.Hour)) {
		t.Fatalf("retry_at should be far future: %v", store.saved[0].RetryAt)
	}
}

func TestRecoverCredentialsSkipsAlreadyRevokedCodes(t *testing.T) {
	now := time.Now().UTC()
	revoked := account.Account{
		ID: "revoked", RefreshToken: "dead", Pool: account.PoolUnavailable,
		UnavailableReason: account.ReasonAuth, LastErrorCode: "refresh-revoked",
	}
	store := &recoveryStore{accounts: []account.Account{revoked}}
	refresher := countingRefresher{}
	result, err := runtime.RecoverCredentials(
		context.Background(), scheduler.New([]account.Account{revoked}), store,
		&refresher, credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Skipped != 1 || refresher.calls != 0 {
		t.Fatalf("result=%#v calls=%d", result, refresher.calls)
	}
}

func TestRefreshExpiringRotatesReadyTokensNearExpiry(t *testing.T) {
	now := time.Now().UTC()
	item := account.Account{
		ID: "ready", AccessToken: "old", RefreshToken: "refresh",
		OIDCIssuer: "https://auth.x.ai", OIDCClientID: "client",
		Pool: account.PoolReady, ExpiresAt: now.Add(10 * time.Minute), MaxActive: 1,
	}
	refreshed := item
	refreshed.AccessToken = "new"
	refreshed.ExpiresAt = now.Add(6 * time.Hour)
	store := &recoveryStore{accounts: []account.Account{item}}
	pool := scheduler.New([]account.Account{item})
	result, err := runtime.RefreshExpiring(
		context.Background(), pool, store,
		credentialRefresher{item: refreshed}, now, 45*time.Minute,
	)
	if err != nil {
		t.Fatalf("refresh expiring: %v", err)
	}
	if result.Refreshed != 1 {
		t.Fatalf("result=%#v", result)
	}
	if len(store.saved) != 1 || store.saved[0].AccessToken != "new" || store.saved[0].Pool != account.PoolReady {
		t.Fatalf("saved=%#v", store.saved)
	}
}

func TestRefreshExpiringSkipsFarExpiry(t *testing.T) {
	now := time.Now().UTC()
	item := account.Account{
		ID: "ready", AccessToken: "old", RefreshToken: "refresh",
		OIDCIssuer: "https://auth.x.ai", OIDCClientID: "client",
		Pool: account.PoolReady, ExpiresAt: now.Add(3 * time.Hour), MaxActive: 1,
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	refresher := countingRefresher{}
	result, err := runtime.RefreshExpiring(
		context.Background(), scheduler.New([]account.Account{item}), store,
		&refresher, now, 45*time.Minute,
	)
	if err != nil {
		t.Fatalf("refresh expiring: %v", err)
	}
	if result.Refreshed != 0 || refresher.calls != 0 {
		t.Fatalf("result=%#v calls=%d", result, refresher.calls)
	}
}

type permanentRefreshError struct{ msg string }

func (e *permanentRefreshError) Error() string   { return e.msg }
func (e *permanentRefreshError) Permanent() bool { return true }

type countingRefresher struct{ calls int }

func (r *countingRefresher) Refresh(context.Context, account.Account) (account.Account, error) {
	r.calls++
	return account.Account{}, nil
}
