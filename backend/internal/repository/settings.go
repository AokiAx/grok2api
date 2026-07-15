package repository

import (
	"context"
	"errors"

	"github.com/AokiAx/grok2api/backend/internal/domain/settings"
)

var (
	ErrSettingsNotFound     = errors.New("settings document not found")
	ErrSettingsConflict     = errors.New("settings revision conflict")
	ErrSettingsSnapshotGone = errors.New("settings snapshot not found")
)

// SettingsRepository persists versioned runtime settings.
type SettingsRepository interface {
	GetSettings(context.Context) (settings.Document, error)
	// PutSettings applies expectedRevision optimistic lock and returns the new document.
	PutSettings(context.Context, int64, settings.Document, string) (settings.Document, error)
	ListSettingsSnapshots(context.Context, int) ([]settings.Snapshot, error)
	GetSettingsSnapshot(context.Context, int64) (settings.Snapshot, bool, error)
	// RollbackSettings restores a previous snapshot as a new revision.
	RollbackSettings(context.Context, int64, int64, string) (settings.Document, error)
}
