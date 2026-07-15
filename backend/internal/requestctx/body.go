package requestctx

import (
	"context"
	"errors"
	"io"
	"sync"
)

var ErrBodyTooLarge = errors.New("request body too large")

type bodyCacheKey struct{}

// BodyCache memoizes one bounded request-body read for all middleware and
// handlers participating in the same request. The byte slice is intentionally
// returned without copying so model authorization, protocol conversion, and
// tracing share one allocation.
type BodyCache struct {
	mu     sync.Mutex
	loaded bool
	body   []byte
	err    error
}

// WithBodyCache installs a cache unless the context already carries one.
func WithBodyCache(ctx context.Context) (context.Context, *BodyCache) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cache := BodyCacheFromContext(ctx); cache != nil {
		return ctx, cache
	}
	cache := &BodyCache{}
	return context.WithValue(ctx, bodyCacheKey{}, cache), cache
}

// BodyCacheFromContext returns the shared body cache, if one was installed.
func BodyCacheFromContext(ctx context.Context) *BodyCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(bodyCacheKey{}).(*BodyCache)
	return cache
}

// Load executes reader exactly once and memoizes both its result and error.
func (c *BodyCache) Load(reader func() ([]byte, error)) ([]byte, error) {
	if c == nil {
		return reader()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.body, c.err
	}
	c.body, c.err = reader()
	c.loaded = true
	return c.body, c.err
}

// Snapshot returns the loaded body without triggering a read.
func (c *BodyCache) Snapshot() ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return nil, false
	}
	return c.body, c.err == nil
}

// ReadBounded reads at most max bytes plus one sentinel byte, returning
// ErrBodyTooLarge when the request exceeds the configured bound.
func ReadBounded(reader io.Reader, max int) ([]byte, error) {
	if max <= 0 {
		return nil, ErrBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(reader, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > max {
		return nil, ErrBodyTooLarge
	}
	return body, nil
}
