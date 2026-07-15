package repository

import (
	"context"
	"errors"

	"github.com/AokiAx/grok2api/backend/internal/domain/settings"
)

var (
	ErrSettingsNotFound = errors.New("settings document not found")
	ErrSettingsConflict = errors.New("settings revision conflict")
)

// SettingsRepository persists runtime settings with optimistic locking.
type SettingsRepository interface {
	GetSettings(context.Context) (settings.Document, error)
	// PutSettings applies expectedRevision optimistic lock and returns the new document.
	PutSettings(context.Context, int64, settings.Document, string) (settings.Document, error)
}
