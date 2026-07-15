package repository

import (
	"context"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/audit"
)

// AuditWriter persists request and attempt audit rows.
type AuditWriter interface {
	RecordRequestAudit(context.Context, audit.Request, []audit.Attempt) error
}

// AuditReader powers dashboard analytics without exposing secrets.
type AuditReader interface {
	AuditUsageSummary(context.Context, time.Time, time.Time) (audit.UsageSummary, error)
	AuditSeries(context.Context, time.Time, time.Time, time.Duration) ([]audit.SeriesPoint, error)
	AuditTopModels(context.Context, time.Time, time.Time, int) ([]audit.NamedCount, error)
	AuditTopAccounts(context.Context, time.Time, time.Time, int) ([]audit.NamedCount, error)
	AuditRecentFailures(context.Context, time.Time, time.Time, int) ([]audit.RecentFailure, error)
	PruneRequestAudits(context.Context, time.Time) (int64, error)
}

// AuditRepository is the combined audit port.
type AuditRepository interface {
	AuditWriter
	AuditReader
}
