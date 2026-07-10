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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/AokiAx/grok2api/internal/compat"
	"github.com/AokiAx/grok2api/internal/service"
	"github.com/AokiAx/grok2api/internal/upstream"
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

// ErrorClass distinguishes request validation from conversion/gateway failures.
type ErrorClass int

const (
	// ClassInvalidRequest maps to HTTP 400.
	ClassInvalidRequest ErrorClass = iota
	// ClassBadGateway maps to HTTP 502 for conversion failures after upstream success.
	ClassBadGateway
)

// Error is a bridge-level failure with an HTTP-oriented class.
type Error struct {
	Class   ErrorClass
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// AsError extracts a bridge.Error.
func AsError(err error) (*Error, bool) {
	var bridgeErr *Error
	if errors.As(err, &bridgeErr) {
		return bridgeErr, true
	}
	return nil, false
}

func invalidRequest(message string, cause error) error {
	return &Error{Class: ClassInvalidRequest, Message: message, Cause: cause}
}

func badGateway(message string, cause error) error {
	return &Error{Class: ClassBadGateway, Message: message, Cause: cause}
}

// prepared is the canonical /responses request after client conversion.
type prepared struct {
	Model        string
	Body         []byte
	ClientStream bool
	Format       ClientFormat
	// ChatBody is retained for PreferResponses=false fallback (native chat path).
	ChatBody []byte
}

// Chat handles POST /v1/chat/completions.
func (p *Pipeline) Chat(ctx context.Context, body []byte) (Result, error) {
	req, err := p.prepareChat(body)
	if err != nil {
		return Result{}, err
	}
	if p.useResponses(req.Model) {
		return p.executeResponses(ctx, req)
	}
	return p.executeNativeChat(ctx, req)
}

// Messages handles POST /v1/messages (Anthropic).
func (p *Pipeline) Messages(ctx context.Context, body []byte) (Result, error) {
	req, err := p.prepareAnthropic(body)
	if err != nil {
		return Result{}, err
	}
	if p.useResponses(req.Model) {
		return p.executeResponses(ctx, req)
	}
	return p.executeNativeChatAsAnthropic(ctx, req)
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

func (p *Pipeline) useResponses(model string) bool {
	if p == nil || !p.PreferResponses {
		return false
	}
	if p.Catalog == nil {
		return true
	}
	return p.Catalog.Backend(model) == upstream.BackendResponses
}

func (p *Pipeline) hintsFor(model string) compat.ModelHints {
	hints := compat.ModelHints{}
	if p == nil || p.Catalog == nil {
		return hints
	}
	if info, ok := p.Catalog.Get(model); ok {
		hints.SupportsBackendSearch = info.SupportsBackendSearch
	}
	return hints
}

func (p *Pipeline) prepareChat(body []byte) (prepared, error) {
	responsesBody, model, stream, err := compat.PrepareResponsesFromChat(body, p.defaultModel())
	if err != nil {
		return prepared{}, invalidRequest("Invalid JSON body", err)
	}
	chatBody, _, _, err := compat.NormalizeChatRequest(body, p.defaultModel())
	if err != nil {
		return prepared{}, invalidRequest("Invalid JSON body", err)
	}
	return prepared{
		Model:        model,
		Body:         responsesBody,
		ClientStream: stream,
		Format:       FormatChat,
		ChatBody:     chatBody,
	}, nil
}

func (p *Pipeline) prepareAnthropic(body []byte) (prepared, error) {
	responsesBody, model, stream, err := compat.PrepareResponsesFromAnthropic(body, p.defaultModel())
	if err != nil {
		return prepared{}, invalidRequest("Invalid Anthropic request", err)
	}
	chatBody, _, err := compat.AnthropicToOpenAI(body, p.defaultModel())
	if err != nil {
		return prepared{}, invalidRequest("Invalid Anthropic request", err)
	}
	return prepared{
		Model:        model,
		Body:         responsesBody,
		ClientStream: stream,
		Format:       FormatAnthropic,
		ChatBody:     chatBody,
	}, nil
}

func (p *Pipeline) prepareResponses(body []byte) (prepared, error) {
	responsesBody, model, stream, err := compat.NormalizeResponsesRequest(body, p.defaultModel())
	if err != nil {
		return prepared{}, invalidRequest("Invalid JSON body", err)
	}
	return prepared{
		Model:        model,
		Body:         responsesBody,
		ClientStream: stream,
		Format:       FormatResponses,
	}, nil
}

// executeResponses is the single upstream path for all client formats.
func (p *Pipeline) executeResponses(ctx context.Context, req prepared) (Result, error) {
	if p == nil || p.Gateway == nil {
		return Result{}, badGateway("gateway unavailable", nil)
	}
	upstreamBody, err := compat.FinalizeResponsesUpstream(req.Body, p.hintsFor(req.Model))
	if err != nil {
		return Result{}, invalidRequest("Invalid request payload", err)
	}

	// Always consume SSE from upstream. Non-stream clients are aggregated in deliver.
	result, err := p.Gateway.Request(ctx, http.MethodPost, "/responses", upstreamBody, true)
	if err != nil {
		return Result{}, err
	}
	if result.Status >= http.StatusBadRequest {
		return materializeErrorResult(result), nil
	}
	return p.deliver(result, req)
}

func (p *Pipeline) executeNativeChat(ctx context.Context, req prepared) (Result, error) {
	result, err := p.Gateway.Chat(ctx, req.ChatBody, req.ClientStream)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func (p *Pipeline) executeNativeChatAsAnthropic(ctx context.Context, req prepared) (Result, error) {
	result, err := p.Gateway.Chat(ctx, req.ChatBody, req.ClientStream)
	if err != nil {
		return Result{}, err
	}
	if result.Status >= http.StatusBadRequest {
		return materializeErrorResult(result), nil
	}
	return deliverAnthropicFromChat(result, req.Model, req.ClientStream)
}

func (p *Pipeline) deliver(result Result, req prepared) (Result, error) {
	switch req.Format {
	case FormatChat:
		return deliverChat(result, req.Model, req.ClientStream)
	case FormatAnthropic:
		return deliverAnthropicFromResponses(result, req.Model, req.ClientStream)
	case FormatResponses:
		return deliverResponses(result, req.ClientStream)
	default:
		return Result{}, badGateway("unknown client format", nil)
	}
}

func deliverChat(result Result, model string, clientStream bool) (Result, error) {
	if clientStream {
		if result.Stream == nil {
			return Result{}, badGateway("Upstream stream missing", nil)
		}
		result.Stream = compat.NewResponsesToChatStream(result.Stream, model)
		return withSSEHeaders(result), nil
	}
	body, err := readResponsesAsChat(result, model)
	if err != nil {
		return Result{}, err
	}
	result.Body = body
	result.Stream = nil
	return withJSONHeaders(result), nil
}

func deliverAnthropicFromResponses(result Result, model string, clientStream bool) (Result, error) {
	if clientStream {
		if result.Stream == nil {
			return Result{}, badGateway("Upstream stream missing", nil)
		}
		chatStream := compat.NewResponsesToChatStream(result.Stream, model)
		result.Stream = compat.NewAnthropicStream(chatStream, model)
		return withSSEHeaders(result), nil
	}
	chatBody, err := readResponsesAsChat(result, model)
	if err != nil {
		return Result{}, err
	}
	converted, err := compat.OpenAIToAnthropic(chatBody)
	if err != nil {
		return Result{}, badGateway("Invalid upstream response", err)
	}
	result.Body = converted
	result.Stream = nil
	return withJSONHeaders(result), nil
}

func deliverAnthropicFromChat(result Result, model string, clientStream bool) (Result, error) {
	if clientStream {
		if result.Stream == nil {
			return Result{}, badGateway("Upstream stream missing", nil)
		}
		result.Stream = compat.NewAnthropicStream(result.Stream, model)
		return withSSEHeaders(result), nil
	}
	converted, err := compat.OpenAIToAnthropic(result.Body)
	if err != nil {
		return Result{}, badGateway("Invalid upstream response", err)
	}
	result.Body = converted
	result.Stream = nil
	return withJSONHeaders(result), nil
}

func deliverResponses(result Result, clientStream bool) (Result, error) {
	if clientStream {
		if result.Stream == nil {
			// Some gateways may buffer; fall through to body if present.
			if len(result.Body) > 0 {
				return withJSONHeaders(result), nil
			}
			return Result{}, badGateway("Upstream stream missing", nil)
		}
		return withSSEHeaders(result), nil
	}
	if result.Stream != nil {
		data, err := io.ReadAll(result.Stream)
		_ = result.Stream.Close()
		result.Stream = nil
		if err != nil {
			return Result{}, badGateway("Invalid upstream stream", err)
		}
		if completed := compat.ExtractCompletedResponse(data); len(completed) > 0 {
			result.Body = completed
		} else {
			result.Body = data
		}
	}
	return withJSONHeaders(result), nil
}

func readResponsesAsChat(result Result, model string) ([]byte, error) {
	if result.Stream != nil {
		aggregated, err := compat.AggregateResponsesStream(result.Stream, model)
		if err != nil {
			return nil, badGateway("Invalid upstream stream", err)
		}
		return aggregated, nil
	}
	converted, err := compat.ResponsesToChat(result.Body)
	if err != nil {
		return nil, badGateway("Invalid upstream response", err)
	}
	return converted, nil
}

func materializeErrorResult(result Result) Result {
	if result.Stream == nil {
		return result
	}
	data, err := io.ReadAll(result.Stream)
	_ = result.Stream.Close()
	result.Stream = nil
	if err == nil {
		result.Body = data
	}
	return result
}

func withSSEHeaders(result Result) Result {
	if result.Header == nil {
		result.Header = make(http.Header)
	}
	result.Header.Del("Content-Length")
	result.Header.Set("Content-Type", "text/event-stream")
	return result
}

func withJSONHeaders(result Result) Result {
	if result.Header == nil {
		result.Header = make(http.Header)
	}
	result.Header.Del("Content-Length")
	result.Header.Set("Content-Type", "application/json")
	return result
}
