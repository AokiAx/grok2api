package upstream

import "context"

type contextKey int

const convIDContextKey contextKey = 1

// WithConvID attaches a Grok conversation / sticky session id to ctx.
// Client.Request reads it and sets x-grok-conv-id.
func WithConvID(ctx context.Context, convID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if convID == "" {
		return ctx
	}
	return context.WithValue(ctx, convIDContextKey, convID)
}

// ConvIDFrom returns the sticky session id from ctx, or "".
func ConvIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(convIDContextKey).(string)
	return value
}
