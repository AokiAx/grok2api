package bridge

import (
	"github.com/AokiAx/grok2api/internal/compat"
	"github.com/AokiAx/grok2api/internal/upstream"
)

// prepared is the canonical /responses request after client conversion.
type prepared struct {
	Model        string
	Body         []byte
	ClientStream bool
	Format       ClientFormat
	// ChatBody is retained for PreferResponses=false fallback (native chat path).
	ChatBody []byte
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
