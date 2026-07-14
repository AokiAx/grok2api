package intercept

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strconv"

	"github.com/AokiAx/grok2api/backend/internal/service"
)

// usage fields appear in response.completed near stream end — keep a tail window
// so cache diagnosis is not limited by MaxBody head preview.
var (
	reCachedTokens  = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
	reInputTokens   = regexp.MustCompile(`"input_tokens"\s*:\s*(\d+)`)
	reOutputTokens  = regexp.MustCompile(`"output_tokens"\s*:\s*(\d+)`)
	reTotalTokens   = regexp.MustCompile(`"total_tokens"\s*:\s*(\d+)`)
	usageTailWindow = 96 << 10
)

// Gateway is the subset of service.Gateway used by the API/bridge.
type Gateway interface {
	Chat(context.Context, []byte, bool) (service.ChatResult, error)
	Request(context.Context, string, string, []byte, bool) (service.ChatResult, error)
}

// TraceGateway wraps a gateway and records upstream request/response stages.
type TraceGateway struct {
	Inner  Gateway
	Tracer *Tracer
}

func (g *TraceGateway) Chat(ctx context.Context, payload []byte, stream bool) (service.ChatResult, error) {
	return g.trace(ctx, "POST", "/chat/completions", payload, stream, func() (service.ChatResult, error) {
		return g.Inner.Chat(ctx, payload, stream)
	})
}

func (g *TraceGateway) Request(
	ctx context.Context,
	method string,
	path string,
	payload []byte,
	stream bool,
) (service.ChatResult, error) {
	return g.trace(ctx, method, path, payload, stream, func() (service.ChatResult, error) {
		return g.Inner.Request(ctx, method, path, payload, stream)
	})
}

func (g *TraceGateway) trace(
	ctx context.Context,
	method string,
	path string,
	payload []byte,
	stream bool,
	call func() (service.ChatResult, error),
) (service.ChatResult, error) {
	if g == nil || g.Inner == nil {
		return service.ChatResult{}, io.ErrUnexpectedEOF
	}
	span := FromContext(ctx)
	if g.Tracer != nil && g.Tracer.Enabled() && span != nil {
		span.Event("upstream_request", map[string]any{
			"method":        method,
			"upstream_path": path,
			"stream":        stream,
			"body":          g.Tracer.BodyPreview(payload),
		})
	}

	result, err := call()
	if g.Tracer == nil || !g.Tracer.Enabled() || span == nil {
		return result, err
	}

	fields := map[string]any{
		"method":        method,
		"upstream_path": path,
		"stream":        stream,
		"status":        result.Status,
	}
	if err != nil {
		fields["error"] = err.Error()
		span.Event("upstream_response", fields)
		return result, err
	}
	if result.Stream != nil {
		// Tee stream so we can log a preview without consuming the client stream.
		var preview bytes.Buffer
		sniffer := &usageSniffer{}
		limited := &limitedBuffer{buf: &preview, max: g.Tracer.opts.MaxBody}
		multi := io.MultiWriter(limited, sniffer)
		result.Stream = &teeReadCloser{
			Reader: io.TeeReader(result.Stream, multi),
			Closer: result.Stream,
		}
		fields["body"] = g.Tracer.BodyPreview(nil)
		fields["stream_preview_note"] = "stream tee active; partial body captured as bytes are read"
		span.Event("upstream_response", fields)
		// Attach final stream preview flush via wrapped closer.
		result.Stream = &streamPreviewCloser{
			ReadCloser: result.Stream,
			span:       span,
			tracer:     g.Tracer,
			preview:    &preview,
			sniffer:    sniffer,
		}
		return result, nil
	}
	// Non-stream: extract usage from JSON body when present.
	if usage := extractUsageMetrics(result.Body); usage != nil {
		fields["usage"] = usage
	}
	fields["body"] = g.Tracer.BodyPreview(result.Body)
	span.Event("upstream_response", fields)
	return result, nil
}

type teeReadCloser struct {
	io.Reader
	Closer io.Closer
}

func (t *teeReadCloser) Close() error {
	if t.Closer != nil {
		return t.Closer.Close()
	}
	return nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
	n   int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.n >= l.max {
		return len(p), nil
	}
	remain := l.max - l.n
	if len(p) > remain {
		_, _ = l.buf.Write(p[:remain])
		l.n = l.max
		return len(p), nil
	}
	n, err := l.buf.Write(p)
	l.n += n
	return len(p), err
}

type streamPreviewCloser struct {
	io.ReadCloser
	span    *Span
	tracer  *Tracer
	preview *bytes.Buffer
	sniffer *usageSniffer
	once    bool
}

func (s *streamPreviewCloser) Close() error {
	if !s.once && s.span != nil && s.tracer != nil {
		s.once = true
		if s.preview != nil {
			s.span.Event("upstream_stream_preview", map[string]any{
				"body": s.tracer.BodyPreview(s.preview.Bytes()),
			})
		}
		if s.sniffer != nil {
			if usage := s.sniffer.metrics(); usage != nil {
				s.span.Event("upstream_usage", usage)
			}
		}
	}
	if s.ReadCloser != nil {
		return s.ReadCloser.Close()
	}
	return nil
}

// usageSniffer watches the full upstream stream (not head-truncated) and pulls
// token usage / cached_tokens out of response.completed tails.
type usageSniffer struct {
	tail []byte
}

func (u *usageSniffer) Write(p []byte) (int, error) {
	if u == nil {
		return len(p), nil
	}
	if len(p) == 0 {
		return 0, nil
	}
	u.tail = append(u.tail, p...)
	if len(u.tail) > usageTailWindow {
		u.tail = append([]byte(nil), u.tail[len(u.tail)-usageTailWindow:]...)
	}
	return len(p), nil
}

func (u *usageSniffer) metrics() map[string]any {
	if u == nil || len(u.tail) == 0 {
		return nil
	}
	return extractUsageMetrics(u.tail)
}

func extractUsageMetrics(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	// Prefer the last occurrence (completed event is near the end).
	findLast := func(re *regexp.Regexp) (int, bool) {
		all := re.FindAllSubmatch(raw, -1)
		if len(all) == 0 {
			return 0, false
		}
		n, err := strconv.Atoi(string(all[len(all)-1][1]))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	out := map[string]any{}
	if n, ok := findLast(reCachedTokens); ok {
		out["cached_tokens"] = n
	}
	if n, ok := findLast(reInputTokens); ok {
		out["input_tokens"] = n
	}
	if n, ok := findLast(reOutputTokens); ok {
		out["output_tokens"] = n
	}
	if n, ok := findLast(reTotalTokens); ok {
		out["total_tokens"] = n
	}
	if len(out) == 0 {
		return nil
	}
	if cached, ok := out["cached_tokens"].(int); ok {
		if input, ok := out["input_tokens"].(int); ok && input > 0 {
			out["cache_ratio"] = float64(cached) / float64(input)
		}
	}
	return out
}

// Ensure interface compliance.
var _ Gateway = (*TraceGateway)(nil)
