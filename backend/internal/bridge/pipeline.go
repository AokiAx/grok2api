// Package bridge is the single protocol bridge between client-facing APIs
// (OpenAI Chat, Anthropic Messages, OpenAI Responses) and the Grok CLI
// upstream /responses backend.
//
// Design:
//
//	client body
//	  → Prepare* (normalize + convert to Responses JSON)
//	  → FinalizeResponsesUpstream (search policy, whitelist, force stream:true)
//	  → gateway POST /responses (always SSE)
//	  → Deliver* (aggregate / re-encode to client format)
//
// Handlers in package api should only do auth, body size limits, and HTTP write.
package bridge

import (
	"context"
	"net/http"
	"strings"

	"github.com/AokiAx/grok2api/backend/internal/compat"
	"github.com/AokiAx/grok2api/backend/internal/service"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

// ClientFormat is the protocol the caller expects in the HTTP response.
type ClientFormat int

const (
	// FormatChat is OpenAI Chat Completions.
	FormatChat ClientFormat = iota
	// FormatAnthropic is Anthropic Messages.
	FormatAnthropic
	// FormatResponses is OpenAI/Grok Responses.
	FormatResponses
)

// Gateway is the account-pooling upstream client used by the API server.
type Gateway interface {
	Chat(context.Context, []byte, bool) (service.ChatResult, error)
	Request(context.Context, string, string, []byte, bool) (service.ChatResult, error)
}

// Catalog supplies per-model backend routing and search capability.
type Catalog interface {
	Backend(model string) string
	Get(model string) (upstream.ModelInfo, bool)
}

// Pipeline is the unified Chat/Anthropic/Responses → upstream bridge.
type Pipeline struct {
	Gateway         Gateway
	Catalog         Catalog
	DefaultModel    string
	PreferResponses bool
}

// Result is a client-ready upstream outcome (stream or buffered body).
type Result = service.ChatResult

// Chat handles POST /v1/chat/completions.
// Misrouted Responses bodies (input/instructions, no chat messages) are re-routed.
func (p *Pipeline) Chat(ctx context.Context, body []byte) (Result, error) {
	if compat.DetectPayload(body) == compat.KindResponses {
		return p.Responses(ctx, body)
	}
	req, err := p.prepareChat(body)
	if err != nil {
		return Result{}, err
	}
	if p.useResponses(req.UpstreamModel) {
		return p.executeResponses(ctx, req)
	}
	return p.executeNativeChat(ctx, req)
}

// Messages handles POST /v1/messages (Anthropic).
//
// Always uses the Grok /responses backend (direct Anthropic ↔ Responses).
// Codex and some gateways POST Responses-shaped JSON to /v1/messages; those are
// detected and handled as native Responses so input/tools are not dropped.
//
// convID is optional session sticky (Claude Code session header).
func (p *Pipeline) Messages(ctx context.Context, body []byte, convID ...string) (Result, error) {
	if compat.DetectPayload(body) == compat.KindResponses {
		return p.Responses(ctx, body)
	}
	session := ""
	if len(convID) > 0 {
		session = strings.TrimSpace(convID[0])
	}
	req, err := p.prepareAnthropic(body, session)
	if err != nil {
		return Result{}, err
	}
	return p.executeResponses(ctx, req)
}

// Responses handles POST /v1/responses.
func (p *Pipeline) Responses(ctx context.Context, body []byte) (Result, error) {
	req, err := p.prepareResponses(body)
	if err != nil {
		return Result{}, err
	}
	// Native Responses always targets /responses, even when PreferResponses is false.
	return p.executeResponses(ctx, req)
}

func (p *Pipeline) defaultModel() string {
	if p != nil && strings.TrimSpace(p.DefaultModel) != "" {
		return p.DefaultModel
	}
	return "grok-4.5"
}

// executeResponses is the single upstream path for all client formats.
func (p *Pipeline) executeResponses(ctx context.Context, req prepared) (Result, error) {
	if p == nil || p.Gateway == nil {
		return Result{}, badGateway("gateway unavailable", nil)
	}
	upstreamBody, warnings, err := compat.FinalizeResponsesUpstreamDetailed(req.Body, p.hintsFor(req.UpstreamModel))
	if err != nil {
		return Result{}, invalidRequest("Invalid request payload", err)
	}

	if conv := strings.TrimSpace(req.ConvID); conv != "" {
		ctx = upstream.WithConvID(ctx, conv)
	}

	// Always consume SSE from upstream. Non-stream clients are aggregated in deliver.
	result, err := p.Gateway.Request(ctx, http.MethodPost, "/responses", upstreamBody, true)
	if err != nil {
		return Result{}, err
	}
	if result.Status >= http.StatusBadRequest {
		return materializeErrorResult(result), nil
	}
	result = withCompatibilityWarnings(result, warnings)
	return p.deliver(result, req)
}

func (p *Pipeline) executeNativeChat(ctx context.Context, req prepared) (Result, error) {
	result, err := p.Gateway.Chat(ctx, req.ChatBody, req.ClientStream)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
