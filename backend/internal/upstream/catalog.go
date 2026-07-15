package upstream

import "strings"

type ModelInfo struct {
	ID string `json:"id"`
	// UpstreamID is the provider-facing model id. Empty means ID is already upstream.
	UpstreamID              string   `json:"upstream_id,omitempty"`
	Name                    string   `json:"name,omitempty"`
	APIBackend              string   `json:"api_backend"`
	ContextWindow           int      `json:"context_window,omitempty"`
	SupportsReasoningEffort bool     `json:"supports_reasoning_effort,omitempty"`
	ReasoningEfforts        []string `json:"reasoning_efforts,omitempty"`
	SupportsBackendSearch   bool     `json:"supports_backend_search,omitempty"`
	OwnedBy                 string   `json:"owned_by,omitempty"`
}

const (
	BackendResponses       = "responses"
	BackendChatCompletions = "chat_completions"
)

// DefaultCatalog mirrors the local Grok CLI models cache (0.2.93).
func DefaultCatalog() []ModelInfo {
	return []ModelInfo{
		{
			ID:                      "grok-4.5",
			Name:                    "Grok 4.5",
			APIBackend:              BackendResponses,
			ContextWindow:           500000,
			SupportsReasoningEffort: true,
			ReasoningEfforts:        []string{"high", "medium", "low"},
			SupportsBackendSearch:   true,
			OwnedBy:                 "xai",
		},
		{
			ID:            "grok-composer-2.5-fast",
			Name:          "Composer 2.5",
			APIBackend:    BackendResponses,
			ContextWindow: 200000,
			OwnedBy:       "xai",
		},
	}
}

type Catalog struct {
	models map[string]ModelInfo
}

func NewCatalog(items []ModelInfo) *Catalog {
	catalog := &Catalog{models: make(map[string]ModelInfo, len(items))}
	for _, item := range items {
		catalog.models[strings.ToLower(strings.TrimSpace(item.ID))] = item
	}
	return catalog
}

func NewDefaultCatalog() *Catalog {
	return NewCatalog(DefaultCatalog())
}

func (c *Catalog) Backend(model string) string {
	if c == nil {
		return BackendResponses
	}
	if item, ok := c.models[strings.ToLower(strings.TrimSpace(model))]; ok {
		if item.APIBackend != "" {
			return item.APIBackend
		}
	}
	// CLI current generation defaults to responses.
	return BackendResponses
}

func (c *Catalog) Get(model string) (ModelInfo, bool) {
	if c == nil {
		return ModelInfo{}, false
	}
	item, ok := c.models[strings.ToLower(strings.TrimSpace(model))]
	return item, ok
}

// ResolveUpstream maps a public model id or alias to the provider model id.
func (c *Catalog) ResolveUpstream(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if item, ok := c.Get(model); ok {
		if upstream := strings.TrimSpace(item.UpstreamID); upstream != "" {
			return upstream
		}
		return strings.TrimSpace(item.ID)
	}
	return model
}

func (c *Catalog) List() []ModelInfo {
	if c == nil {
		return nil
	}
	out := make([]ModelInfo, 0, len(c.models))
	for _, item := range DefaultCatalog() {
		if current, ok := c.models[strings.ToLower(item.ID)]; ok {
			out = append(out, current)
		}
	}
	// Include any extra models not in defaults.
	seen := make(map[string]struct{}, len(out))
	for _, item := range out {
		seen[strings.ToLower(item.ID)] = struct{}{}
	}
	for _, item := range c.models {
		if _, ok := seen[strings.ToLower(item.ID)]; ok {
			continue
		}
		out = append(out, item)
	}
	return out
}

// EnrichModelMap adds known CLI metadata onto an OpenAI-style model object.
func (c *Catalog) EnrichModelMap(item map[string]any) map[string]any {
	if item == nil {
		return item
	}
	id, _ := item["id"].(string)
	if id == "" {
		id, _ = item["model"].(string)
	}
	info, ok := c.Get(id)
	if !ok {
		applyCodexModelDefaults(item, 128000, false, false)
		return item
	}
	if _, exists := item["api_backend"]; !exists {
		item["api_backend"] = info.APIBackend
	}
	if _, exists := item["context_window"]; !exists && info.ContextWindow > 0 {
		item["context_window"] = info.ContextWindow
	}
	if _, exists := item["supports_reasoning_effort"]; !exists {
		item["supports_reasoning_effort"] = info.SupportsReasoningEffort
	}
	if _, exists := item["reasoning_efforts"]; !exists && len(info.ReasoningEfforts) > 0 {
		item["reasoning_efforts"] = info.ReasoningEfforts
	}
	if _, exists := item["supports_backend_search"]; !exists {
		item["supports_backend_search"] = info.SupportsBackendSearch
	}
	if name, _ := item["name"].(string); name == "" && info.Name != "" {
		item["name"] = info.Name
	}
	applyCodexModelDefaults(item, info.ContextWindow, info.SupportsReasoningEffort, info.SupportsBackendSearch)
	return item
}

// applyCodexModelDefaults fills fields Codex looks up in model metadata.
// Missing metadata causes: "Model metadata for grok-4.5 not found".
func applyCodexModelDefaults(item map[string]any, contextWindow int, reasoning, search bool) {
	if contextWindow <= 0 {
		contextWindow = 128000
	}
	if _, ok := item["context_window"]; !ok {
		item["context_window"] = contextWindow
	}
	if _, ok := item["max_context_window"]; !ok {
		item["max_context_window"] = contextWindow
	}
	if _, ok := item["context_length"]; !ok {
		item["context_length"] = contextWindow
	}
	if _, ok := item["max_completion_tokens"]; !ok {
		maxOut := contextWindow / 4
		if maxOut > 128000 {
			maxOut = 128000
		}
		if maxOut < 8192 {
			maxOut = 8192
		}
		item["max_completion_tokens"] = maxOut
	}
	if _, ok := item["supports_function_calling"]; !ok {
		item["supports_function_calling"] = true
	}
	if _, ok := item["supports_tools"]; !ok {
		item["supports_tools"] = true
	}
	if _, ok := item["supports_parallel_function_calling"]; !ok {
		item["supports_parallel_function_calling"] = true
	}
	if _, ok := item["supports_streaming"]; !ok {
		item["supports_streaming"] = true
	}
	if reasoning {
		if _, ok := item["supports_reasoning"]; !ok {
			item["supports_reasoning"] = true
		}
	}
	if search {
		if _, ok := item["supports_web_search"]; !ok {
			item["supports_web_search"] = true
		}
	}
	if _, ok := item["supported_endpoints"]; !ok {
		item["supported_endpoints"] = []string{"chat_completions", "responses"}
	}
}
