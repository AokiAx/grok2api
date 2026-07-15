// Package requestctx carries per-request metadata (sticky client key, etc.).
package requestctx

import "context"

type ctxKey int

const (
	stickyKey    ctxKey = 1
	requestIDKey ctxKey = 2
)

// WithStickyKey attaches a sticky session key used by the account pool.
func WithStickyKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, stickyKey, key)
}

// StickyKey returns the sticky session key, or empty when unset.
func StickyKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(stickyKey).(string)
	return value
}

// WithRequestID attaches a correlation id for logs and response headers.
func WithRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the correlation id, or empty when unset.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}
