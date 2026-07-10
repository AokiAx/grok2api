package runtime

import (
	"context"
	"fmt"
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

type CredentialRecoveryResult struct {
	Recovered int
	Failed    int
	Skipped   int
}

func RecoverCredentials(
	ctx context.Context,
	scheduler *scheduler.Scheduler,
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
	for _, item := range accounts {
		if item.Pool != account.PoolUnavailable ||
			item.UnavailableReason != account.ReasonAuth ||
			item.RefreshToken == "" ||
			(!item.RetryAt.IsZero() && item.RetryAt.After(now)) {
			result.Skipped++
			continue
		}
		refreshed, refreshErr := refresher.Refresh(ctx, item)
		if refreshErr != nil {
			item.Pool = account.PoolUnavailable
			item.UnavailableReason = account.ReasonAuth
			item.RetryAt = now.Add(30 * time.Minute)
			item.LastErrorCode = "refresh-failed"
			item.UpdatedAt = now.UTC()
			if err := store.SaveAccount(ctx, item); err != nil {
				return result, fmt.Errorf("save refresh failure for %s: %w", item.ID, err)
			}
			scheduler.Upsert(item)
			result.Failed++
			continue
		}
		reason, errorCode, validateErr := validator.Validate(ctx, refreshed)
		if validateErr != nil {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = account.ReasonAuth
			refreshed.RetryAt = now.Add(30 * time.Minute)
			refreshed.LastErrorCode = "validation-failed"
			result.Failed++
		} else if reason == "" {
			refreshed.Pool = account.PoolReady
			refreshed.UnavailableReason = ""
			refreshed.RetryAt = time.Time{}
			refreshed.LastErrorCode = ""
			refreshed.AuthenticationFails = 0
			result.Recovered++
		} else {
			refreshed.Pool = account.PoolUnavailable
			refreshed.UnavailableReason = reason
			refreshed.LastErrorCode = errorCode
			refreshed.RetryAt = credentialRetryAt(reason, now)
			result.Failed++
		}
		refreshed.UpdatedAt = now.UTC()
		if err := store.SaveAccount(ctx, refreshed); err != nil {
			return result, fmt.Errorf("save refreshed account %s: %w", refreshed.ID, err)
		}
		scheduler.Upsert(refreshed)
	}
	return result, nil
}

func credentialRetryAt(reason account.UnavailableReason, now time.Time) time.Time {
	switch reason {
	case account.ReasonQuota:
		return now.Add(30 * time.Minute)
	case account.ReasonCooldown:
		return now.Add(45 * time.Second)
	case account.ReasonAuth, account.ReasonValidating:
		return now.Add(30 * time.Minute)
	default:
		return time.Time{}
	}
}

type recoveryConfig struct {
	credentialStore CredentialStore
	refresher       CredentialRefresher
	validator       CredentialValidator
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

func RecoverDue(
	ctx context.Context,
	scheduler *scheduler.Scheduler,
	store AccountStore,
	now time.Time,
) error {
	for _, item := range scheduler.PromoteDue(now) {
		if err := store.SaveAccount(ctx, item); err != nil {
			return fmt.Errorf("save promoted account %s: %w", item.ID, err)
		}
	}
	return nil
}

func RunRecovery(
	ctx context.Context,
	scheduler *scheduler.Scheduler,
	store AccountStore,
	interval time.Duration,
	options ...RecoveryOption,
) error {
	config := recoveryConfig{}
	for _, option := range options {
		option(&config)
	}
	runOnce := func(now time.Time) error {
		if err := RecoverDue(ctx, scheduler, store, now); err != nil {
			return err
		}
		if config.credentialStore != nil && config.refresher != nil && config.validator != nil {
			if _, err := RecoverCredentials(
				ctx,
				scheduler,
				config.credentialStore,
				config.refresher,
				config.validator,
				now,
			); err != nil {
				return err
			}
		}
		return nil
	}
	if err := runOnce(time.Now().UTC()); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := runOnce(now); err != nil {
				return err
			}
		}
	}
}
