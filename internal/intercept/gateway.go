package intercept

import (
	"bytes"
	"context"
	"io"

	"github.com/AokiAx/grok2api/internal/service"
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
		limited := &limitedBuffer{buf: &preview, max: g.Tracer.opts.MaxBody}
		result.Stream = &teeReadCloser{
			Reader: io.TeeReader(result.Stream, limited),
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
		}
		return result, nil
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
	once    bool
}

func (s *streamPreviewCloser) Close() error {
	if !s.once && s.span != nil && s.tracer != nil && s.preview != nil {
		s.once = true
		s.span.Event("upstream_stream_preview", map[string]any{
			"body": s.tracer.BodyPreview(s.preview.Bytes()),
		})
	}
	if s.ReadCloser != nil {
		return s.ReadCloser.Close()
	}
	return nil
}

// Ensure interface compliance.
var _ Gateway = (*TraceGateway)(nil)
