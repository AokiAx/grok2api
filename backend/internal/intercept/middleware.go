package intercept

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Middleware wraps an HTTP handler and records client request/response bodies
// for API routes that go through protocol conversion.
func Middleware(tracer *Tracer, next http.Handler) http.Handler {
	if tracer == nil || !tracer.Enabled() {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !shouldTrace(request) {
			next.ServeHTTP(writer, request)
			return
		}

		body, err := io.ReadAll(io.LimitReader(request.Body, 32<<20))
		_ = request.Body.Close()
		if err != nil {
			next.ServeHTTP(writer, request)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))

		ctx, span := tracer.Start(request.Context(), request.URL.Path, request.Method)
		request = request.WithContext(ctx)

		span.Event("client_request", map[string]any{
			"method":       request.Method,
			"path":         request.URL.Path,
			"query":        request.URL.RawQuery,
			"content_type": request.Header.Get("Content-Type"),
			"stream_hint":  strings.Contains(strings.ToLower(string(body)), `"stream":true`),
			"body":         tracer.BodyPreview(body),
		})

		recorder := &responseRecorder{
			ResponseWriter: writer,
			status:         http.StatusOK,
			body:           &bytes.Buffer{},
			maxBody:        tracer.opts.MaxBody,
		}
		start := time.Now()
		next.ServeHTTP(recorder, request)

		fields := map[string]any{
			"status":       recorder.status,
			"elapsed_ms":   time.Since(start).Milliseconds(),
			"content_type": recorder.Header().Get("Content-Type"),
			"stream":       strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream"),
			"body":         tracer.BodyPreview(recorder.body.Bytes()),
		}
		span.Event("client_response", fields)
		span.End(recorder.status, nil)
	})
}

func shouldTrace(request *http.Request) bool {
	if request.Method != http.MethodPost {
		return false
	}
	path := request.URL.Path
	return path == "/v1/chat/completions" ||
		path == "/chat/completions" ||
		path == "/v1/responses" ||
		path == "/v1/messages" ||
		path == "/messages"
}

type responseRecorder struct {
	http.ResponseWriter
	status  int
	body    *bytes.Buffer
	maxBody int
	written int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.body != nil && r.written < r.maxBody {
		remain := r.maxBody - r.written
		if remain > 0 {
			if len(p) > remain {
				r.body.Write(p[:remain])
			} else {
				r.body.Write(p)
			}
			r.written += min(len(p), remain)
		}
	}
	return r.ResponseWriter.Write(p)
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
