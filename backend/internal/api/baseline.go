package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/requestctx"
)

const (
	// HeaderRequestID is the canonical client/server request correlation header.
	HeaderRequestID = "X-Request-Id"
	// defaultMaxBodyBytes is the global ceiling for non-streaming request bodies.
	// Route handlers may apply tighter limits for sensitive endpoints.
	defaultMaxBodyBytes int64 = 32 << 20
)

// Readiness reports whether the process is ready to accept traffic.
// Liveness (/health, /healthz) must not depend on this.
type Readiness interface {
	Ready() (ready bool, reason string)
}

// AtomicReadiness is a simple process-level readiness gate.
type AtomicReadiness struct {
	ready  atomic.Bool
	reason atomic.Value // string
}

// Set marks the process ready or not ready with a short reason for /readyz.
func (r *AtomicReadiness) Set(ready bool, reason string) {
	if r == nil {
		return
	}
	r.ready.Store(ready)
	if reason == "" {
		if ready {
			reason = "ready"
		} else {
			reason = "starting"
		}
	}
	r.reason.Store(reason)
}

// Ready implements Readiness.
func (r *AtomicReadiness) Ready() (bool, string) {
	if r == nil {
		return true, "ready"
	}
	reason, _ := r.reason.Load().(string)
	if reason == "" {
		if r.ready.Load() {
			reason = "ready"
		} else {
			reason = "starting"
		}
	}
	return r.ready.Load(), reason
}

type alwaysReady struct{}

func (alwaysReady) Ready() (bool, string) { return true, "ready" }

// productionBaseline wraps the root handler with request IDs, security headers,
// body size limits, and structured access logs.
func productionBaseline(next http.Handler, readiness Readiness, maxBodyBytes int64) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if readiness == nil {
		readiness = alwaysReady{}
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(request.Header.Get(HeaderRequestID))
		if requestID == "" {
			requestID = strings.TrimSpace(request.Header.Get("X-Request-ID"))
		}
		if requestID == "" {
			requestID = newRequestID()
		}
		ctx := requestctx.WithRequestID(request.Context(), requestID)
		ctx, _ = requestctx.WithBodyCache(ctx)
		request = request.WithContext(ctx)

		// Bound body size globally. Handlers that already re-wrap MaxBytesReader
		// stay within this ceiling.
		if request.Body != nil && request.Body != http.NoBody {
			request.Body = http.MaxBytesReader(writer, request.Body, maxBodyBytes)
		}

		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		recorder.Header().Set(HeaderRequestID, requestID)
		setSecurityHeaders(recorder.Header(), request)

		// Dedicated readiness endpoint needs the gate even when mounted on mux.
		if request.Method == http.MethodGet && (request.URL.Path == "/readyz" || request.URL.Path == "/ready") {
			// Fall through to mux after headers; mux handler still owns body.
			// Gate is applied inside readyz when server.readiness is set.
		}

		next.ServeHTTP(recorder, request)

		path := request.URL.Path
		if path == "" {
			path = "/"
		}
		slog.Info("http_request",
			"request_id", requestID,
			"method", request.Method,
			"path", path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_ip", clientIP(request),
		)
	})
}

func setSecurityHeaders(header http.Header, request *http.Request) {
	if header.Get("X-Content-Type-Options") == "" {
		header.Set("X-Content-Type-Options", "nosniff")
	}
	if header.Get("X-Frame-Options") == "" {
		header.Set("X-Frame-Options", "DENY")
	}
	if header.Get("Referrer-Policy") == "" {
		header.Set("Referrer-Policy", "no-referrer")
	}
	if header.Get("Permissions-Policy") == "" {
		header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	}
	// Do not force Cache-Control here: upstream / SPA responses may set their own.
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(raw[:])
}

func clientIP(request *http.Request) string {
	if request == nil {
		return ""
	}
	if forwarded := strings.TrimSpace(request.Header.Get("X-Forwarded-For")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(request.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(request.RemoteAddr)
	}
	return host
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	if status <= 0 {
		status = http.StatusOK
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// RequestIDFromContext is a thin alias so handlers outside requestctx can read IDs.
func RequestIDFromContext(ctx context.Context) string {
	return requestctx.RequestID(ctx)
}
