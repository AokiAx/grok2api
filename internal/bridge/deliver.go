package bridge

import (
	"io"
	"net/http"

	"github.com/AokiAx/grok2api/internal/compat"
)

func (p *Pipeline) deliver(result Result, req prepared) (Result, error) {
	switch req.Format {
	case FormatChat:
		return deliverChat(result, req.Model, req.ClientStream)
	case FormatAnthropic:
		return deliverAnthropicFromResponses(result, req.Model, req.ClientStream, req.ThinkingEnabled, req.ThinkingDisplay)
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

func deliverAnthropicFromResponses(result Result, model string, clientStream bool, thinkingEnabled bool, thinkingDisplay string) (Result, error) {
	if thinkingDisplay == "" {
		thinkingDisplay = "summarized"
	}
	if clientStream {
		if result.Stream == nil {
			return Result{}, badGateway("Upstream stream missing", nil)
		}
		result.Stream = compat.NewResponsesToAnthropicStream(result.Stream, model, thinkingEnabled, thinkingDisplay)
		return withSSEHeaders(result), nil
	}
	if result.Stream != nil {
		converted, err := compat.AggregateResponsesToAnthropic(result.Stream, model, thinkingEnabled, thinkingDisplay)
		if err != nil {
			return Result{}, badGateway("Invalid upstream stream", err)
		}
		result.Body = converted
		result.Stream = nil
		return withJSONHeaders(result), nil
	}
	converted, err := compat.ResponsesToAnthropic(result.Body, model, thinkingEnabled, thinkingDisplay)
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
