// Package requestctx carries per-request metadata (sticky client key, etc.).
package requestctx

import "context"

type ctxKey int

const stickyKey ctxKey = 1

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
