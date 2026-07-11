package compat

import (
	"net/http"
	"strings"
)

// SessionIDFromRequest extracts a sticky session id for prompt cache / conv continuity.
// Prefer Claude Code's session header, then generic session / conv headers.
func SessionIDFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, key := range []string{
		"x-claude-code-session-id",
		"X-Claude-Code-Session-Id",
		"x-session-id",
		"x-grok-conv-id",
		"X-Grok-Conv-Id",
	} {
		if v := strings.TrimSpace(r.Header.Get(key)); v != "" {
			return v
		}
	}
	return ""
}
