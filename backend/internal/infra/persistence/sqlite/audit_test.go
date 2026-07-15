package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/audit"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
)

func TestRequestAuditRoundTripAndDashboardQueries(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	item := audit.Request{
		ID: "aud_1", RequestID: "req_1", StartedAt: now.Add(-2 * time.Minute), FinishedAt: now.Add(-2*time.Minute + 40*time.Millisecond),
		DurationMS: 40, Method: "POST", Path: "/v1/chat/completions", Operation: "chat",
		Model: "grok-4.5", ClientKeyID: "key-1", AccountID: "acc-1", StatusCode: 200, Success: true,
		InputTokens: 100, CachedInputTokens: 40, OutputTokens: 12, TotalTokens: 112, AttemptCount: 1,
	}
	attempts := []audit.Attempt{{
		Ordinal: 1, AccountID: "acc-1", StartedAt: item.StartedAt, FinishedAt: item.FinishedAt,
		DurationMS: 40, StatusCode: 200, Success: true,
	}}
	if err := repo.RecordRequestAudit(ctx, item, attempts); err != nil {
		t.Fatalf("record success: %v", err)
	}
	fail := item
	fail.ID = "aud_2"
	fail.RequestID = "req_2"
	fail.StartedAt = now.Add(-time.Minute)
	fail.FinishedAt = now.Add(-time.Minute + 120*time.Millisecond)
	fail.DurationMS = 120
	fail.Success = false
	fail.StatusCode = 429
	fail.ErrorType = "quota"
	fail.ErrorCode = "quota_exhausted"
	fail.AccountID = "acc-2"
	fail.Model = "grok-code-fast-1"
	fail.InputTokens = 0
	fail.CachedInputTokens = 0
	fail.OutputTokens = 0
	fail.TotalTokens = 0
	if err := repo.RecordRequestAudit(ctx, fail, []audit.Attempt{{
		Ordinal: 1, AccountID: "acc-2", StartedAt: fail.StartedAt, FinishedAt: fail.FinishedAt,
		DurationMS: 100, StatusCode: 429, Success: false, ErrorType: "quota", ErrorCode: "quota_exhausted", Rotated: true,
	}}); err != nil {
		t.Fatalf("record fail: %v", err)
	}

	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)
	usage, err := repo.AuditUsageSummary(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Requests != 2 || usage.SuccessfulRequests != 1 || usage.FailedRequests != 1 {
		t.Fatalf("usage=%+v", usage)
	}
	if usage.InputTokens != 100 || usage.CachedInputTokens != 40 || usage.OutputTokens != 12 || usage.TotalTokens != 112 {
		t.Fatalf("token usage=%+v", usage)
	}
	if usage.SampledRequests != 1 {
		t.Fatalf("sampled=%d want 1", usage.SampledRequests)
	}
	if usage.P95DurationMS < 40 {
		t.Fatalf("p95=%d", usage.P95DurationMS)
	}
	models, err := repo.AuditTopModels(ctx, from, to, 5)
	if err != nil || len(models) == 0 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	found := false
	for _, m := range models {
		if m.Name == "grok-4.5" {
			found = true
			if m.InputTokens != 100 || m.CachedInputTokens != 40 || m.OutputTokens != 12 || m.Tokens != 112 {
				t.Fatalf("top model tokens=%+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("models missing grok-4.5: %+v", models)
	}
	accounts, err := repo.AuditTopAccounts(ctx, from, to, 5)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	recent, err := repo.AuditRecentFailures(ctx, from, to, 10)
	if err != nil || len(recent) != 1 || recent[0].ErrorCode != "quota_exhausted" {
		t.Fatalf("recent=%v err=%v", recent, err)
	}
	series, err := repo.AuditSeries(ctx, from, to, time.Hour)
	if err != nil || len(series) == 0 {
		t.Fatalf("series=%v err=%v", series, err)
	}
	n, err := repo.PruneRequestAudits(ctx, now.Add(-90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned=%d want 1", n)
	}
	count, err := repo.CountRequestAudits(ctx)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
