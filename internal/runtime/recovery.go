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
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := RecoverDue(ctx, scheduler, store, now); err != nil {
				return err
			}
		}
	}
}
