// Package audit defines request audit records for gateway observability.
// Records intentionally exclude prompt text, response bodies, and secrets.
package audit

import "time"

// Request is one client-facing gateway call (chat/responses/messages/models/billing).
type Request struct {
	ID           string
	RequestID    string
	StartedAt    time.Time
	FinishedAt   time.Time
	DurationMS   int64
	Method       string
	Path         string
	Operation    string
	Model        string
	ClientKeyID  string
	AccountID    string
	StatusCode   int
	Success      bool
	ErrorType    string
	ErrorCode    string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	AttemptCount int
	Stream       bool
}

// Attempt is one account try within a request (rotation/retry).
type Attempt struct {
	RequestID  string
	Ordinal    int
	AccountID  string
	StartedAt  time.Time
	FinishedAt time.Time
	DurationMS int64
	StatusCode int
	Success    bool
	ErrorType  string
	ErrorCode  string
	Rotated    bool
}

// UsageSummary aggregates request volume over a window.
type UsageSummary struct {
	Requests           int64
	SuccessfulRequests int64
	FailedRequests     int64
	InputTokens        int64
	OutputTokens       int64
	TotalTokens        int64
	// P95DurationMS is approximate from stored durations (percentile sample).
	P95DurationMS int64
	SuccessRate   float64
}

// SeriesModelUsage is per-model token volume inside one series bucket.
type SeriesModelUsage struct {
	Model  string
	Tokens int64
}

// SeriesPoint is a time-bucketed counter.
type SeriesPoint struct {
	BucketStart time.Time
	BucketEnd   time.Time
	Requests    int64
	Failures    int64
	Tokens      int64
	Models      []SeriesModelUsage
}

// NamedCount is a ranked aggregation row.
type NamedCount struct {
	Name   string
	Count  int64
	Tokens int64
}

// RecentFailure is a compact failure row for dashboards.
type RecentFailure struct {
	RequestID  string
	StartedAt  time.Time
	Model      string
	AccountID  string
	StatusCode int
	ErrorType  string
	ErrorCode  string
	Path       string
	DurationMS int64
}
