// Package intercept provides a temporary request-trace interceptor for debugging
// Chat/Anthropic/Responses conversion issues (tool calls, empty bodies, 422s).
//
// Enable with config "debug_trace": true or env GROK2API_DEBUG_TRACE=1.
// Traces are written as JSON Lines under data/traces/ (or debug_trace_dir).
package intercept

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}

// Options controls the temporary interceptor.
type Options struct {
	Enabled bool
	// Dir is the directory for JSONL trace files. Empty → {data_dir}/traces.
	Dir string
	// MaxBody caps logged body size (bytes). Default 64 KiB.
	MaxBody int
}

// Tracer records multi-stage events for a request.
type Tracer struct {
	opts   Options
	mu     sync.Mutex
	logger *slog.Logger
}

// Span is one in-flight request trace.
type Span struct {
	tracer *Tracer
	id     string
	path   string
	start  time.Time
	mu     sync.Mutex
	events []map[string]any
}

// New creates a tracer. When disabled, methods are cheap no-ops.
func New(opts Options) *Tracer {
	if opts.MaxBody <= 0 {
		opts.MaxBody = 64 << 10
	}
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = "data/traces"
	}
	return &Tracer{
		opts:   opts,
		logger: slog.Default().With("component", "intercept"),
	}
}

func (t *Tracer) Enabled() bool {
	return t != nil && t.opts.Enabled
}

// Start begins a new request span and stores it on the context.
func (t *Tracer) Start(ctx context.Context, path, method string) (context.Context, *Span) {
	if !t.Enabled() {
		return ctx, nil
	}
	span := &Span{
		tracer: t,
		id:     newID(),
		path:   path,
		start:  time.Now().UTC(),
		events: make([]map[string]any, 0, 8),
	}
	span.Event("request.start", map[string]any{
		"method": method,
		"path":   path,
	})
	return context.WithValue(ctx, contextKey{}, span), span
}

// FromContext returns the span attached by Start, if any.
func FromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	span, _ := ctx.Value(contextKey{}).(*Span)
	return span
}

// Event appends a stage event (client_request, upstream_request, upstream_response, client_response, …).
func (s *Span) Event(stage string, fields map[string]any) {
	if s == nil || s.tracer == nil || !s.tracer.Enabled() {
		return
	}
	event := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"stage": stage,
	}
	for key, value := range fields {
		event[key] = value
	}
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()

	// Also emit a short slog line so docker logs show the trail live.
	s.tracer.logger.Info("trace",
		"trace_id", s.id,
		"path", s.path,
		"stage", stage,
		"summary", summarizeEvent(event),
	)
}

// End flushes the span to a JSONL file.
func (s *Span) End(status int, err error) {
	if s == nil || s.tracer == nil || !s.tracer.Enabled() {
		return
	}
	fields := map[string]any{
		"status":      status,
		"elapsed_ms":  time.Since(s.start).Milliseconds(),
		"event_count": len(s.events),
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	s.Event("request.end", fields)
	s.tracer.write(s)
}

func (t *Tracer) write(span *Span) {
	if err := os.MkdirAll(t.opts.Dir, 0o700); err != nil {
		t.logger.Error("create trace dir", "error", err, "dir", t.opts.Dir)
		return
	}
	name := fmt.Sprintf("%s_%s.jsonl",
		span.start.Format("20060102T150405.000"),
		span.id,
	)
	// Sanitize path for filename.
	safePath := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, span.path)
	name = fmt.Sprintf("%s_%s_%s.jsonl",
		span.start.Format("20060102T150405.000"),
		safePath,
		span.id,
	)
	path := filepath.Join(t.opts.Dir, name)

	span.mu.Lock()
	events := append([]map[string]any(nil), span.events...)
	span.mu.Unlock()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.logger.Error("open trace file", "error", err, "path", path)
		return
	}
	defer file.Close()

	header, _ := json.Marshal(map[string]any{
		"trace_id": span.id,
		"path":     span.path,
		"started":  span.start.Format(time.RFC3339Nano),
		"file":     path,
	})
	_, _ = file.Write(header)
	_, _ = file.Write([]byte("\n"))
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			continue
		}
		_, _ = file.Write(line)
		_, _ = file.Write([]byte("\n"))
	}
	t.logger.Info("trace written", "trace_id", span.id, "path", path, "events", len(events))
}

// BodyPreview redacts secrets and truncates large payloads for safe logging.
func (t *Tracer) BodyPreview(raw []byte) map[string]any {
	max := 64 << 10
	if t != nil && t.opts.MaxBody > 0 {
		max = t.opts.MaxBody
	}
	preview := map[string]any{
		"bytes": len(raw),
	}
	if len(raw) == 0 {
		preview["body"] = ""
		return preview
	}
	truncated := false
	data := raw
	if len(data) > max {
		data = append([]byte(nil), raw[:max]...)
		truncated = true
	}
	var parsed any
	if json.Unmarshal(data, &parsed) == nil {
		preview["body"] = redact(parsed)
	} else {
		preview["body"] = string(data)
	}
	if truncated {
		preview["truncated"] = true
		preview["max_body"] = max
	}
	return preview
}

func redact(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if isSecretKey(key) {
				out[key] = "***"
				continue
			}
			out[key] = redact(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redact(child)
		}
		return out
	default:
		return value
	}
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "authorization", "api_key", "app_key", "panel_password", "password",
		"secret", "access_token", "refresh_token", "id_token", "sso", "sso_cookie",
		"cookie", "set-cookie", "x-api-key", "admin_password", "capmonster_api_key":
		return true
	default:
		// Avoid matching max_tokens / max_completion_tokens / total_tokens.
		if strings.HasSuffix(lower, "_api_key") ||
			strings.HasSuffix(lower, "_password") ||
			strings.HasSuffix(lower, "_secret") ||
			strings.HasSuffix(lower, "_token") && !strings.Contains(lower, "max_") && !strings.Contains(lower, "total_") && !strings.Contains(lower, "completion_") && !strings.Contains(lower, "prompt_") && !strings.Contains(lower, "input_") && !strings.Contains(lower, "output_") {
			return true
		}
		return false
	}
}

func summarizeEvent(event map[string]any) string {
	parts := make([]string, 0, 4)
	for _, key := range []string{"method", "path", "upstream_path", "status", "stream", "error"} {
		if value, ok := event[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", key, value))
		}
	}
	if body, ok := event["body"].(map[string]any); ok {
		if n, ok := body["bytes"].(int); ok {
			parts = append(parts, fmt.Sprintf("body_bytes=%d", n))
		}
	}
	if len(parts) == 0 {
		return event["stage"].(string)
	}
	return strings.Join(parts, " ")
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
