package bridge

import (
	"strings"

	"github.com/AokiAx/grok2api/internal/compat"
	"github.com/AokiAx/grok2api/internal/service"
	"github.com/AokiAx/grok2api/internal/upstream"
)

// prepared is the canonical /responses request after client conversion.
type prepared struct {
	// Model is the client-facing model id used in Body and response echo.
	Model string
	Body  []byte
	// UpstreamModel is the model used for catalog hints (same as Model; no aliases).
	UpstreamModel string
	ClientStream  bool
	Format        ClientFormat
	// ChatBody is retained for PreferResponses=false Chat path only.
	ChatBody []byte
	// Session sticky id (Claude Code session → prompt_cache_key + x-grok-conv-id).
	ConvID string
	// Anthropic thinking bridge (CPA-style signature / summary blocks).
	ThinkingEnabled bool
	ThinkingDisplay string
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
	convID := service.PromptCacheKeyFromPayload(responsesBody)
	if convID == "" {
		convID = service.PromptCacheKeyFromPayload(body)
	}
	return prepared{
		Model:         model,
		UpstreamModel: model,
		Body:          responsesBody,
		ClientStream:  stream,
		Format:        FormatChat,
		ChatBody:      chatBody,
		ConvID:        convID,
	}, nil
}

func (p *Pipeline) prepareAnthropic(body []byte, convID string) (prepared, error) {
	if strings.TrimSpace(convID) == "" {
		convID = service.PromptCacheKeyFromPayload(body)
	}
	responsesBody, model, stream, err := compat.PrepareResponsesFromAnthropicWithOptions(body, compat.AnthropicToResponsesOptions{
		DefaultModel: p.defaultModel(),
		ConvID:       convID,
	})
	if err != nil {
		return prepared{}, invalidRequest("Invalid Anthropic request", err)
	}
	if strings.TrimSpace(convID) == "" {
		convID = service.PromptCacheKeyFromPayload(responsesBody)
	}
	thinkingEnabled, thinkingDisplay := compat.AnthropicThinkingBridge(body)
	return prepared{
		Model:           model,
		UpstreamModel:   model,
		Body:            responsesBody,
		ClientStream:    stream,
		Format:          FormatAnthropic,
		ConvID:          convID,
		ThinkingEnabled: thinkingEnabled,
		ThinkingDisplay: thinkingDisplay,
	}, nil
}

func (p *Pipeline) prepareResponses(body []byte) (prepared, error) {
	responsesBody, model, stream, err := compat.NormalizeResponsesRequest(body, p.defaultModel())
	if err != nil {
		return prepared{}, invalidRequest("Invalid JSON body", err)
	}
	// Official prompt_cache_key becomes x-grok-conv-id + pool sticky.
	convID := service.PromptCacheKeyFromPayload(responsesBody)
	if convID == "" {
		convID = service.PromptCacheKeyFromPayload(body)
	}
	return prepared{
		Model:         model,
		UpstreamModel: model,
		Body:          responsesBody,
		ClientStream:  stream,
		Format:        FormatResponses,
		ConvID:        convID,
	}, nil
}
