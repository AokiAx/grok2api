package runtime_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/runtime"
	"github.com/AokiAx/grok2api/backend/internal/scheduler"
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
	calls  atomic.Int64
}

func (p *quotaProber) ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error) {
	p.calls.Add(1)
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

func TestRecoverQuotaPromotesWhenSlidingWindowDue(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	item := account.Account{
		ID:                "quota",
		AccessToken:       "token",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(-time.Minute),
		QuotaActual:       100,
		QuotaLimit:        100,
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
		24*time.Hour,
		nil,
	)
	if err != nil {
		t.Fatalf("recover quota: %v", err)
	}
	// Sliding-window restore is time-based — no probe.
	if result.Recovered != 1 || prober.calls.Load() != 0 {
		t.Fatalf("result=%#v calls=%d", result, prober.calls.Load())
	}
	if len(store.saved) != 1 || store.saved[0].Pool != account.PoolReady {
		t.Fatalf("saved = %#v", store.saved)
	}
	if store.saved[0].QuotaActual != 0 {
		t.Fatalf("quota counters should clear on window roll: %#v", store.saved[0])
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

func TestRecoverQuotaOldestDueFirst(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	// 260 due accounts; only 256 fit in a tick. Oldest retry_at must win.
	accounts := make([]account.Account, 0, 260)
	for i := 0; i < 260; i++ {
		accounts = append(accounts, account.Account{
			ID:                fmt.Sprintf("%03d", i),
			Pool:              account.PoolUnavailable,
			UnavailableReason: account.ReasonQuota,
			// Higher index = more recently due.
			RetryAt:   now.Add(-time.Duration(260-i) * time.Minute),
			MaxActive: 1,
		})
	}
	store := &recoveryStore{accounts: accounts}
	pool := scheduler.New(accounts)
	prober := &quotaProber{}
	result, err := runtime.RecoverQuota(
		context.Background(), pool, store, prober, nil, now, time.Hour, nil,
	)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Recovered != 256 {
		t.Fatalf("recovered=%d", result.Recovered)
	}
	// Oldest IDs (000..) should be among recovered/saved ready.
	readyIDs := map[string]bool{}
	for _, item := range store.saved {
		if item.Pool == account.PoolReady {
			readyIDs[item.ID] = true
		}
	}
	if !readyIDs["000"] || !readyIDs["001"] || readyIDs["259"] {
		t.Fatalf("ordering wrong; ready sample has 000=%v 001=%v 259=%v count=%d",
			readyIDs["000"], readyIDs["001"], readyIDs["259"], len(readyIDs))
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
		nil,
	)
	if err != nil {
		t.Fatalf("recover quota: %v", err)
	}
	if result.Skipped != 1 || prober.calls.Load() != 0 || result.Recovered != 0 || len(store.saved) != 0 {
		t.Fatalf("result=%#v calls=%d saved=%d", result, prober.calls.Load(), len(store.saved))
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

func TestRecoverValidatingPromotesHealthyAccount(t *testing.T) {
	now := time.Now().UTC()
	item := account.Account{
		ID: "new-mint", AccessToken: "tok",
		Pool: account.PoolUnavailable, UnavailableReason: account.ReasonValidating,
		LastErrorCode: "permission-denied", RetryAt: now.Add(-time.Second),
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	pool := scheduler.New([]account.Account{item})
	result, err := runtime.RecoverValidating(
		context.Background(), pool, store, credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover validating: %v", err)
	}
	if result.Recovered != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(store.saved) != 1 || store.saved[0].Pool != account.PoolReady {
		t.Fatalf("saved = %#v", store.saved)
	}
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.Release()
}

func TestRecoverValidatingSkipsFutureRetryAt(t *testing.T) {
	now := time.Now().UTC()
	item := account.Account{
		ID: "waiting", AccessToken: "tok",
		Pool: account.PoolUnavailable, UnavailableReason: account.ReasonValidating,
		RetryAt: now.Add(time.Minute),
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	result, err := runtime.RecoverValidating(
		context.Background(), scheduler.New([]account.Account{item}), store,
		credentialValidator{}, now,
	)
	if err != nil {
		t.Fatalf("recover validating: %v", err)
	}
	if result.Skipped < 1 || result.Recovered != 0 || len(store.saved) != 0 {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
}

func TestRecoverValidatingEscalatesAfterMaxFails(t *testing.T) {
	now := time.Now().UTC()
	item := account.Account{
		ID: "stuck", AccessToken: "tok",
		Pool: account.PoolUnavailable, UnavailableReason: account.ReasonValidating,
		LastErrorCode: "permission-denied", AuthenticationFails: 11,
		RetryAt: now.Add(-time.Second),
	}
	store := &recoveryStore{accounts: []account.Account{item}}
	result, err := runtime.RecoverValidating(
		context.Background(), scheduler.New([]account.Account{item}), store,
		credentialValidator{reason: account.ReasonValidating, code: "permission-denied"}, now,
	)
	if err != nil {
		t.Fatalf("recover validating: %v", err)
	}
	if result.Failed != 1 || len(store.saved) != 1 {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
	if store.saved[0].UnavailableReason != account.ReasonAuth {
		t.Fatalf("expected escalate to auth, got %#v", store.saved[0])
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
	if store.saved[0].LastErrorCode != "refresh-failed" {
		t.Fatalf("saved = %#v", store.saved[0])
	}
	if !store.saved[0].RetryAt.After(now.Add(4*time.Minute)) || !store.saved[0].RetryAt.Before(now.Add(10*time.Minute)) {
		t.Fatalf("expected ~5m backoff, got retry_at=%v", store.saved[0].RetryAt)
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

func TestRecoverQuotaIgnoresProberAndPromotesDue(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	quota := account.Account{ID: "q", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota, RetryAt: now.Add(-time.Minute)}
	store := &recoveryStore{accounts: []account.Account{quota}}
	pool := scheduler.New([]account.Account{quota})
	// Prober would fail; time-based path must still promote.
	result, err := runtime.RecoverQuota(context.Background(), pool, store, errQuotaProbe{}, credentialValidator{}, now, time.Hour, nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result.Recovered != 1 || store.saved[0].Pool != account.PoolReady {
		t.Fatalf("result=%#v saved=%#v", result, store.saved)
	}
}

func TestRecoverQuotaWorksWithoutProber(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	quota := account.Account{ID: "q", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota, RetryAt: now.Add(-time.Minute)}
	store := &recoveryStore{accounts: []account.Account{quota}}
	pool := scheduler.New([]account.Account{quota})
	result, err := runtime.RecoverQuota(context.Background(), pool, store, nil, nil, now, time.Hour, nil)
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
	if store.saved[0].UnavailableReason != account.ReasonDisabled {
		t.Fatalf("want disabled quarantine, got %#v", store.saved[0])
	}
	if !store.saved[0].RetryAt.After(now.Add(24 * time.Hour)) {
		t.Fatalf("retry_at should be far future: %v", store.saved[0].RetryAt)
	}
}

func TestRecoverCredentialsIsolatesAlreadyRevokedCodes(t *testing.T) {
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
	if result.Revoked != 1 || refresher.calls != 0 {
		t.Fatalf("result=%#v calls=%d", result, refresher.calls)
	}
	if len(store.saved) != 1 || store.saved[0].UnavailableReason != account.ReasonDisabled {
		t.Fatalf("saved=%#v", store.saved)
	}
}

func TestIsolateUnrecoverableAuthMovesRevokedToDisabled(t *testing.T) {
	now := time.Now().UTC()
	accounts := []account.Account{
		{ID: "revoked", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonAuth, LastErrorCode: "refresh-revoked", RefreshToken: "x"},
		{ID: "auth_fail", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonAuth, LastErrorCode: "auth_failed", RefreshToken: "y"},
		{ID: "ready", Pool: account.PoolReady, RefreshToken: "z"},
	}
	store := &recoveryStore{accounts: accounts}
	pool := scheduler.New(accounts)
	result, err := runtime.IsolateUnrecoverableAuth(context.Background(), pool, store, now)
	if err != nil {
		t.Fatalf("isolate: %v", err)
	}
	if result.Isolated != 1 {
		t.Fatalf("result=%#v", result)
	}
	if len(store.saved) != 1 || store.saved[0].ID != "revoked" || store.saved[0].UnavailableReason != account.ReasonDisabled {
		t.Fatalf("saved=%#v", store.saved)
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
