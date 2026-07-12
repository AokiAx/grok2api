package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/scheduler"
)

type AccountStore interface {
	SaveAccount(context.Context, account.Account) error
}

type CredentialStore interface {
	AccountStore
	ListAccounts(context.Context) ([]account.Account, error)
}

type CredentialRefresher interface {
	Refresh(context.Context, account.Account) (account.Account, error)
}

type CredentialValidator interface {
	Validate(context.Context, account.Account) (account.UnavailableReason, string, error)
}

// QuotaProber verifies free-tier capacity before re-entry.
// Prefer a real /responses probe (same path as live chat) over a pure timer.
type QuotaProber interface {
	ProbeFreeQuota(context.Context, account.Account) (account.UnavailableReason, string, error)
}

// QuotaUsageProber optionally returns free-tier used/limit observed on the probe.
type QuotaUsageProber interface {
	ProbeFreeQuotaUsage(context.Context, account.Account) (account.UnavailableReason, string, int64, int64, bool, error)
}

type CredentialRecoveryResult struct {
	Recovered int
	Failed    int
	Skipped   int
	Refreshed int // proactive ready refreshes
	Revoked   int // permanent invalid_grant / revoked refresh
}

type QuotaRecoveryResult struct {
	Recovered int
	Deferred  int
	Failed    int
	Skipped   int
}

const (
	// Per recovery tick (default interval ~10s). Sized for multi-thousand
	// account pools so due quota/auth backlogs drain in minutes, not hours.
	maxQuotaProbesPerTick        = 256
	quotaProbeWorkers            = 32
	maxValidatingProbesPerTick   = 128
	validatingProbeWorkers       = 16
	maxAuthRefreshesPerTick      = 256
	authRefreshWorkers           = 32
	maxProactiveRefreshesPerTick = 256
	proactiveRefreshWorkers      = 32
	maxIsolatePerTick            = 500
	proactiveRefreshLead         = 90 * time.Minute
	authTransientBackoff         = 5 * time.Minute
	authValidationBackoff        = 10 * time.Minute
	validatingBackoff            = 45 * time.Second
	// After the initial quota park window, re-probes that are still exhausted
	// use this shorter cadence (capped by configured quota_retry) so a rolling
	// free-tier window is rechecked without waiting another full day.
	quotaRecheckBackoff = 2 * time.Hour
	// After this many failed validating re-probes, escalate to auth so truly
	// blocked accounts stop burning probe budget forever.
	maxValidatingFails = 12
)

// IsolationResult counts permanent auth quarantines.
type IsolationResult struct {
	Isolated int
	Skipped  int
	Failed   int
}

// IsolateUnrecoverableAuth moves permanently dead credentials out of the auth
// recovery queue into reason=disabled so status/ops can see them separately
// and recovery ticks stop wasting refresh budget on them.
func IsolateUnrecoverableAuth(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	now time.Time,
) (IsolationResult, error) {
	if store == nil {
		return IsolationResult{}, nil
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return IsolationResult{}, fmt.Errorf("list accounts for auth isolation: %w", err)
	}
	result := IsolationResult{}
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable {
			result.Skipped++
			continue
		}
		// Already quarantined.
		if item.UnavailableReason == account.ReasonDisabled && isUnrecoverableAuthCode(item.LastErrorCode) {
			result.Skipped++
			continue
		}
		if !isUnrecoverableAuthCode(item.LastErrorCode) {
			result.Skipped++
			continue
		}
		if result.Isolated >= maxIsolatePerTick {
			result.Skipped++
			continue
		}
		isolateRevokedAccount(&item, now)
		if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
			result.Failed++
			continue
		}
		result.Isolated++
	}
	return result, nil
}

func isolateRevokedAccount(item *account.Account, now time.Time) {
	item.Pool = account.PoolUnavailable
	item.UnavailableReason = account.ReasonDisabled
	if !isUnrecoverableAuthCode(item.LastErrorCode) {
		item.LastErrorCode = "refresh-revoked"
	}
	// Far-future: never auto-retry; operator must re-import.
	item.RetryAt = now.Add(365 * 24 * time.Hour)
	item.UpdatedAt = now.UTC()
}

func RecoverCredentials(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	refresher CredentialRefresher,
	validator CredentialValidator,
	now time.Time,
) (CredentialRecoveryResult, error) {
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return CredentialRecoveryResult{}, fmt.Errorf("list accounts for credential recovery: %w", err)
	}
	result := CredentialRecoveryResult{}
	candidates := make([]account.Account, 0, 256)
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable ||
			item.UnavailableReason != account.ReasonAuth ||
			item.RefreshToken == "" {
			result.Skipped++
			continue
		}
		// Permanent failures: quarantine and stop burning refresh budget.
		if isUnrecoverableAuthCode(item.LastErrorCode) {
			isolateRevokedAccount(&item, now)
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
			} else {
				result.Revoked++
			}
			continue
		}
		if !item.RetryAt.IsZero() && item.RetryAt.After(now) {
			result.Skipped++
			continue
		}
		candidates = append(candidates, item)
	}
	// Prefer oldest updated / earliest expiry so stuck auth_failed drain first.
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i], candidates[j]
		if !ai.ExpiresAt.Equal(aj.ExpiresAt) {
			if ai.ExpiresAt.IsZero() {
				return false
			}
			if aj.ExpiresAt.IsZero() {
				return true
			}
			return ai.ExpiresAt.Before(aj.ExpiresAt)
		}
		return ai.UpdatedAt.Before(aj.UpdatedAt)
	})
	if len(candidates) > maxAuthRefreshesPerTick {
		result.Skipped += len(candidates) - maxAuthRefreshesPerTick
		candidates = candidates[:maxAuthRefreshesPerTick]
	}
	if len(candidates) == 0 {
		return result, nil
	}

	type authOutcome struct {
		item        account.Account
		refreshed   account.Account
		refreshErr  error
		reason      account.UnavailableReason
		errorCode   string
		validateErr error
		didRefresh  bool
	}
	outcomes := make([]authOutcome, len(candidates))
	workers := authRefreshWorkers
	if workers > len(candidates) {
		workers = len(candidates)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				item := candidates[index]
				refreshed, refreshErr := refresher.Refresh(ctx, item)
				if refreshErr != nil {
					outcomes[index] = authOutcome{item: item, refreshErr: refreshErr}
					continue
				}
				reason, errorCode, validateErr := validator.Validate(ctx, refreshed)
				outcomes[index] = authOutcome{
					item:        item,
					refreshed:   refreshed,
					reason:      reason,
					errorCode:   errorCode,
					validateErr: validateErr,
					didRefresh:  true,
				}
			}
		}()
	}
	for index := range candidates {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	for _, outcome := range outcomes {
		if outcome.refreshErr != nil {
			item := outcome.item
			applyRefreshFailure(&item, outcome.refreshErr, now)
			if isUnrecoverableAuthCode(item.LastErrorCode) {
				result.Revoked++
			} else {
				result.Failed++
			}
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
			}
			continue
		}
		refreshed := outcome.refreshed
		if outcome.validateErr != nil {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = account.ReasonAuth
			refreshed.RetryAt = now.Add(authValidationBackoff)
			refreshed.LastErrorCode = "validation-failed"
			result.Failed++
		} else if outcome.reason == "" {
			refreshed.Pool = account.PoolReady
			refreshed.UnavailableReason = ""
			refreshed.RetryAt = time.Time{}
			refreshed.LastErrorCode = ""
			refreshed.AuthenticationFails = 0
			result.Recovered++
		} else if isUnrecoverableAuthCode(outcome.errorCode) {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = account.ReasonDisabled
			refreshed.LastErrorCode = outcome.errorCode
			refreshed.RetryAt = now.Add(365 * 24 * time.Hour)
			result.Revoked++
		} else {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = outcome.reason
			refreshed.LastErrorCode = outcome.errorCode
			refreshed.RetryAt = credentialRetryAt(outcome.reason, now)
			result.Failed++
		}
		refreshed.UpdatedAt = now.UTC()
		if err := saveAccountBestEffort(ctx, store, pool, refreshed); err != nil {
			result.Failed++
			continue
		}
	}
	return result, nil
}

// RefreshExpiring proactively refreshes ready accounts whose access tokens are
// near expiry (or already past expires_at), so live traffic does not wait for 401.
// Candidates are processed soonest-expiry first so backlog drains fairly.
func RefreshExpiring(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	refresher CredentialRefresher,
	now time.Time,
	lead time.Duration,
) (CredentialRecoveryResult, error) {
	if store == nil || refresher == nil {
		return CredentialRecoveryResult{}, nil
	}
	if lead <= 0 {
		lead = proactiveRefreshLead
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return CredentialRecoveryResult{}, fmt.Errorf("list accounts for proactive refresh: %w", err)
	}
	result := CredentialRecoveryResult{}
	deadline := now.Add(lead)
	candidates := make([]account.Account, 0, len(accounts))
	for _, item := range accounts {
		if item.Pool != account.PoolReady {
			result.Skipped++
			continue
		}
		if item.RefreshToken == "" || item.OIDCIssuer == "" || item.OIDCClientID == "" {
			result.Skipped++
			continue
		}
		// Unknown expiry: leave alone. Known and still far out: skip.
		if item.ExpiresAt.IsZero() || item.ExpiresAt.After(deadline) {
			result.Skipped++
			continue
		}
		candidates = append(candidates, item)
	}
	// Already-expired and soonest-to-expire first.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ExpiresAt.Before(candidates[j].ExpiresAt)
	})
	if len(candidates) > maxProactiveRefreshesPerTick {
		result.Skipped += len(candidates) - maxProactiveRefreshesPerTick
		candidates = candidates[:maxProactiveRefreshesPerTick]
	}
	if len(candidates) == 0 {
		return result, nil
	}

	type refreshOutcome struct {
		item       account.Account
		refreshed  account.Account
		refreshErr error
	}
	outcomes := make([]refreshOutcome, len(candidates))
	workers := proactiveRefreshWorkers
	if workers > len(candidates) {
		workers = len(candidates)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				item := candidates[index]
				refreshed, refreshErr := refresher.Refresh(ctx, item)
				outcomes[index] = refreshOutcome{item: item, refreshed: refreshed, refreshErr: refreshErr}
			}
		}()
	}
	for index := range candidates {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	for _, outcome := range outcomes {
		if outcome.refreshErr != nil {
			item := outcome.item
			applyRefreshFailure(&item, outcome.refreshErr, now)
			if isUnrecoverableAuthCode(item.LastErrorCode) {
				result.Revoked++
			} else {
				result.Failed++
			}
			if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
				result.Failed++
			}
			continue
		}
		// Keep ready; only rotate credentials.
		refreshed := outcome.refreshed
		refreshed.Pool = account.PoolReady
		refreshed.UnavailableReason = ""
		refreshed.RetryAt = time.Time{}
		refreshed.LastErrorCode = ""
		refreshed.UpdatedAt = now.UTC()
		if err := saveAccountBestEffort(ctx, store, pool, refreshed); err != nil {
			result.Failed++
			continue
		}
		result.Refreshed++
	}
	return result, nil
}

func applyRefreshFailure(item *account.Account, refreshErr error, now time.Time) {
	item.Pool = account.PoolUnavailable
	item.UpdatedAt = now.UTC()
	item.AuthenticationFails++
	if isPermanentRefresh(refreshErr) {
		// Quarantine permanently — do not keep them in the auth retry queue.
		item.UnavailableReason = account.ReasonDisabled
		item.LastErrorCode = "refresh-revoked"
		item.RetryAt = now.Add(365 * 24 * time.Hour)
		return
	}
	item.UnavailableReason = account.ReasonAuth
	item.LastErrorCode = "refresh-failed"
	item.RetryAt = now.Add(authTransientBackoff)
}

func isPermanentRefresh(err error) bool {
	if err == nil {
		return false
	}
	var marker interface{ Permanent() bool }
	if errors.As(err, &marker) && marker.Permanent() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "revoked") ||
		strings.Contains(msg, "refresh token has been")
}

func isUnrecoverableAuthCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "refresh-revoked", "invalid_grant":
		return true
	default:
		return false
	}
}

func credentialRetryAt(reason account.UnavailableReason, now time.Time) time.Time {
	switch reason {
	case account.ReasonQuota:
		// Free usage is a rolling ~24h window; short retries just burn probes.
		return now.Add(24 * time.Hour)
	case account.ReasonCooldown:
		return now.Add(45 * time.Second)
	case account.ReasonValidating:
		return now.Add(validatingBackoff)
	case account.ReasonAuth:
		return now.Add(authTransientBackoff)
	case account.ReasonDisabled:
		return now.Add(365 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}

// RecoverValidating re-probes accounts parked as validating (typically
// post-mint permission-denied on chat). Unlike auth recovery this does NOT
// require a refresh first — tokens are usually already valid.
func RecoverValidating(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	validator CredentialValidator,
	now time.Time,
) (CredentialRecoveryResult, error) {
	if store == nil || validator == nil {
		return CredentialRecoveryResult{}, nil
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return CredentialRecoveryResult{}, fmt.Errorf("list accounts for validating recovery: %w", err)
	}
	result := CredentialRecoveryResult{}
	candidates := make([]account.Account, 0, maxValidatingProbesPerTick)
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonValidating {
			result.Skipped++
			continue
		}
		if !item.RetryAt.IsZero() && item.RetryAt.After(now) {
			result.Skipped++
			continue
		}
		candidates = append(candidates, item)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		// Oldest parked first so long-waiting accounts do not starve.
		if candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt)
	})
	if len(candidates) > maxValidatingProbesPerTick {
		result.Skipped += len(candidates) - maxValidatingProbesPerTick
		candidates = candidates[:maxValidatingProbesPerTick]
	}
	if len(candidates) == 0 {
		return result, nil
	}

	type validateOutcome struct {
		item      account.Account
		reason    account.UnavailableReason
		errorCode string
		err       error
	}
	outcomes := make([]validateOutcome, len(candidates))
	workers := validatingProbeWorkers
	if workers > len(candidates) {
		workers = len(candidates)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				item := candidates[index]
				reason, errorCode, validateErr := validator.Validate(ctx, item)
				outcomes[index] = validateOutcome{
					item:      item,
					reason:    reason,
					errorCode: errorCode,
					err:       validateErr,
				}
			}
		}()
	}
	for index := range candidates {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	for _, outcome := range outcomes {
		item := outcome.item
		item.UpdatedAt = now.UTC()
		if outcome.err != nil {
			item.RetryAt = now.Add(validatingBackoff)
			item.LastErrorCode = "validation-failed"
			item.AuthenticationFails++
			result.Failed++
			_ = saveAccountBestEffort(ctx, store, pool, item)
			continue
		}
		if outcome.reason == "" {
			item.Pool = account.PoolReady
			item.UnavailableReason = ""
			item.RetryAt = time.Time{}
			item.LastErrorCode = ""
			item.AuthenticationFails = 0
			result.Recovered++
			_ = saveAccountBestEffort(ctx, store, pool, item)
			continue
		}
		// Still bad.
		item.AuthenticationFails++
		item.LastErrorCode = firstNonEmpty(outcome.errorCode, string(outcome.reason))
		switch {
		case outcome.reason == account.ReasonQuota:
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = account.ReasonQuota
			item.RetryAt = nextQuotaRetry(now, 24*time.Hour)
			result.Failed++
		case outcome.reason == account.ReasonCooldown:
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = account.ReasonCooldown
			item.RetryAt = now.Add(45 * time.Second)
			result.Failed++
		case outcome.reason == account.ReasonAuth || item.AuthenticationFails >= maxValidatingFails:
			// Hard auth, or too many validating failures → park as auth for
			// refresh-based recovery / operator attention.
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = account.ReasonAuth
			item.RetryAt = now.Add(authTransientBackoff)
			result.Failed++
		default:
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = account.ReasonValidating
			item.RetryAt = now.Add(validatingBackoff)
			result.Failed++
		}
		if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
			result.Failed++
		}
	}
	return result, nil
}

type recoveryConfig struct {
	credentialStore CredentialStore
	refresher       CredentialRefresher
	validator       CredentialValidator
	quotaProber     QuotaProber
	quotaRetry      time.Duration
}

type RecoveryOption func(*recoveryConfig)

func WithCredentialRecovery(
	store CredentialStore,
	refresher CredentialRefresher,
	validator CredentialValidator,
) RecoveryOption {
	return func(config *recoveryConfig) {
		config.credentialStore = store
		config.refresher = refresher
		config.validator = validator
	}
}

// WithQuotaRetry sets the backoff used when a quota probe still reports exhausted.
func WithQuotaRetry(duration time.Duration) RecoveryOption {
	return func(config *recoveryConfig) {
		if duration > 0 {
			config.quotaRetry = duration
		}
	}
}

// WithQuotaProber installs the free-quota probe used by RecoverQuota.
func WithQuotaProber(prober QuotaProber) RecoveryOption {
	return func(config *recoveryConfig) {
		config.quotaProber = prober
	}
}

// RecoverQuota returns due free-quota accounts to ready after the sliding
// window (retry_at, typically 24h). Free-tier usage is a rolling window:
// once parked until retry_at, re-entry is time-based — no chat probe.
//
// prober/validator/refresher are ignored (kept in the signature for callers).
func RecoverQuota(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store CredentialStore,
	prober QuotaProber,
	validator CredentialValidator,
	now time.Time,
	quotaRetry time.Duration,
	refresher CredentialRefresher,
) (QuotaRecoveryResult, error) {
	_ = prober
	_ = validator
	_ = refresher
	_ = quotaRetry
	if store == nil {
		return QuotaRecoveryResult{}, nil
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return QuotaRecoveryResult{}, fmt.Errorf("list accounts for quota recovery: %w", err)
	}
	result := QuotaRecoveryResult{}
	candidates := make([]account.Account, 0, maxQuotaProbesPerTick)
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable || item.UnavailableReason != account.ReasonQuota {
			result.Skipped++
			continue
		}
		if !item.RetryAt.IsZero() && item.RetryAt.After(now) {
			result.Skipped++
			continue
		}
		candidates = append(candidates, item)
	}
	// Oldest-due first so a large backlog does not starve early accounts.
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i].RetryAt, candidates[j].RetryAt
		if ai.IsZero() && !aj.IsZero() {
			return true
		}
		if !ai.IsZero() && aj.IsZero() {
			return false
		}
		if !ai.Equal(aj) {
			return ai.Before(aj)
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(candidates) > maxQuotaProbesPerTick {
		result.Skipped += len(candidates) - maxQuotaProbesPerTick
		candidates = candidates[:maxQuotaProbesPerTick]
	}
	for _, item := range candidates {
		item.Pool = account.PoolReady
		item.UnavailableReason = ""
		item.RetryAt = time.Time{}
		item.LastErrorCode = ""
		// Sliding window has elapsed; clear local counters for the new window.
		item.QuotaActual = 0
		item.UpdatedAt = now.UTC()
		if err := saveAccountBestEffort(ctx, store, pool, item); err != nil {
			result.Failed++
			continue
		}
		result.Recovered++
	}
	return result, nil
}

// nextQuotaRetry returns the re-probe deadline after a still-exhausted quota
// check. Uses the shorter recheck cadence unless the configured retry is
// tighter (tests / ops override).
func nextQuotaRetry(now time.Time, quotaRetry time.Duration) time.Time {
	if quotaRetry <= 0 {
		quotaRetry = 24 * time.Hour
	}
	recheck := quotaRecheckBackoff
	if quotaRetry < recheck {
		recheck = quotaRetry
	}
	return now.Add(recheck)
}

func accessTokenNeedsRefresh(item account.Account, now time.Time) bool {
	if strings.TrimSpace(item.RefreshToken) == "" ||
		strings.TrimSpace(item.OIDCIssuer) == "" ||
		strings.TrimSpace(item.OIDCClientID) == "" {
		return false
	}
	if item.ExpiresAt.IsZero() {
		// Unknown expiry on a long-parked quota account: refresh proactively
		// before spending a chat probe.
		return true
	}
	return !item.ExpiresAt.After(now.Add(5 * time.Minute))
}

func probeQuota(
	ctx context.Context,
	prober QuotaProber,
	validator CredentialValidator,
	item account.Account,
) (account.UnavailableReason, string, int64, int64, bool, error) {
	if usageProber, ok := prober.(QuotaUsageProber); ok {
		return usageProber.ProbeFreeQuotaUsage(ctx, item)
	}
	if prober != nil {
		reason, code, err := prober.ProbeFreeQuota(ctx, item)
		return reason, code, 0, 0, false, err
	}
	reason, code, err := validator.Validate(ctx, item)
	return reason, code, 0, 0, false, err
}

func saveAccountBestEffort(
	ctx context.Context,
	store AccountStore,
	pool *scheduler.Scheduler,
	item account.Account,
) error {
	if err := store.SaveAccount(ctx, item); err != nil {
		slog.Error("recovery save account failed", "account_id", item.ID, "error", err)
		return err
	}
	if pool != nil {
		pool.Upsert(item)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func RecoverDue(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store AccountStore,
	now time.Time,
) error {
	// Cooldown re-enters without an upstream probe. Quota uses RecoverQuota
	// (time-based sliding window, no chat probe).
	for _, item := range pool.PromoteDue(now) {
		if err := store.SaveAccount(ctx, item); err != nil {
			// Do not abort the whole recovery tick for one account.
			slog.Error("recovery promote save failed", "account_id", item.ID, "error", err)
			continue
		}
	}
	return nil
}

func RunRecovery(
	ctx context.Context,
	pool *scheduler.Scheduler,
	store AccountStore,
	interval time.Duration,
	options ...RecoveryOption,
) error {
	config := recoveryConfig{quotaRetry: 24 * time.Hour}
	for _, option := range options {
		option(&config)
	}
	runOnce := func(now time.Time) {
		if err := RecoverDue(ctx, pool, store, now); err != nil {
			slog.Error("recovery promote tick failed", "error", err)
		}
		// Quarantine permanently dead refresh tokens before spending refresh budget.
		if config.credentialStore != nil {
			if iso, err := IsolateUnrecoverableAuth(ctx, pool, config.credentialStore, now); err != nil {
				slog.Error("recovery auth isolation tick failed", "error", err)
			} else if iso.Isolated > 0 {
				slog.Info("recovery isolated revoked accounts", "isolated", iso.Isolated, "failed", iso.Failed)
			}
		}
		runQuota := func(label string) {
			if config.credentialStore == nil {
				return
			}
			if res, err := RecoverQuota(
				ctx,
				pool,
				config.credentialStore,
				config.quotaProber,
				config.validator,
				now,
				config.quotaRetry,
				config.refresher,
			); err != nil {
				slog.Error("recovery quota tick failed", "phase", label, "error", err)
			} else if res.Recovered > 0 || res.Deferred > 0 || res.Failed > 0 {
				slog.Info("recovery quota tick",
					"phase", label,
					"recovered", res.Recovered,
					"deferred", res.Deferred,
					"failed", res.Failed,
					"skipped", res.Skipped,
				)
			}
		}
		// Quota first: free-tier due queues are the largest backlog.
		runQuota("early")
		// Re-probe post-mint permission-denied accounts before spending refresh budget.
		if config.credentialStore != nil && config.validator != nil {
			if res, err := RecoverValidating(ctx, pool, config.credentialStore, config.validator, now); err != nil {
				slog.Error("recovery validating tick failed", "error", err)
			} else if res.Recovered > 0 || res.Failed > 0 {
				slog.Info("recovery validated accounts", "recovered", res.Recovered, "failed", res.Failed, "skipped", res.Skipped)
			}
		}
		if config.credentialStore != nil && config.refresher != nil {
			if res, err := RefreshExpiring(
				ctx,
				pool,
				config.credentialStore,
				config.refresher,
				now,
				proactiveRefreshLead,
			); err != nil {
				slog.Error("recovery proactive refresh tick failed", "error", err)
			} else if res.Refreshed > 0 || res.Failed > 0 || res.Revoked > 0 {
				slog.Info("recovery proactive refresh",
					"refreshed", res.Refreshed,
					"failed", res.Failed,
					"revoked", res.Revoked,
				)
			}
		}
		if config.credentialStore != nil && config.refresher != nil && config.validator != nil {
			if res, err := RecoverCredentials(
				ctx,
				pool,
				config.credentialStore,
				config.refresher,
				config.validator,
				now,
			); err != nil {
				slog.Error("recovery credential tick failed", "error", err)
			} else if res.Recovered > 0 || res.Failed > 0 || res.Revoked > 0 {
				slog.Info("recovery credentials",
					"recovered", res.Recovered,
					"failed", res.Failed,
					"revoked", res.Revoked,
				)
			}
		}
		// Second quota pass so due free accounts do not wait a full auth cycle.
		runQuota("late")
	}
	runOnce(time.Now().UTC())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			runOnce(now.UTC())
		}
	}
}
