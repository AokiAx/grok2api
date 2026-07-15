package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// StickyKeyFromRequest builds a sticky identity from client headers / auth.
// Prefer explicit sticky/user headers (NewAPI multi-tenant behind one upstream key).
func StickyKeyFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	for _, header := range []string{
		"X-Grok2API-Sticky",
		"X-User-Id",
		"X-OpenAI-Client-User",
		"X-Newapi-User",
		"X-Request-Id",
	} {
		if value := strings.TrimSpace(request.Header.Get(header)); value != "" {
			return header + ":" + value
		}
	}
	if grant, ok := ClientGrantFromContext(request.Context()); ok && grant.Authenticated {
		expected := "client-key:" + strings.TrimSpace(grant.KeyID)
		if grant.Principal != "" && grant.Principal == expected && expected != "client-key:" {
			return expected
		}
	}
	token := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(token) >= len(prefix) && strings.EqualFold(token[:len(prefix)], prefix) {
		token = strings.TrimSpace(token[len(prefix):])
	} else {
		token = strings.TrimSpace(request.Header.Get("x-api-key"))
	}
	if token != "" {
		return "auth:" + token
	}
	return ""
}

// PromptCacheKeyFromPayload extracts the official OpenAI/Grok prompt_cache_key
// used for account sticky and upstream x-grok-conv-id continuity.
func PromptCacheKeyFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(asString(body["prompt_cache_key"]))
}

// PayloadAffinityKey fingerprints stable request prefix fields (model,
// instructions, tool names) so continuous sessions with the same tools/system
// stick to one Grok account even when many users share one API key.
func PayloadAffinityKey(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	parts := make([]string, 0, 8)
	if model := strings.TrimSpace(asString(body["model"])); model != "" {
		parts = append(parts, "m:"+model)
	}
	if inst := strings.TrimSpace(asString(body["instructions"])); inst != "" {
		// Cap length so huge instructions still group similar sessions.
		if len(inst) > 2048 {
			inst = inst[:2048]
		}
		parts = append(parts, "i:"+inst)
	}
	// Chat-style system content is often the stable prefix for agents.
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(asString(msg["role"])))
			if role != "system" && role != "developer" {
				continue
			}
			text := strings.TrimSpace(asString(msg["content"]))
			if text == "" {
				continue
			}
			if len(text) > 2048 {
				text = text[:2048]
			}
			parts = append(parts, "s:"+text)
			break
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		names := make([]string, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ := strings.ToLower(strings.TrimSpace(asString(tool["type"])))
			name := strings.TrimSpace(asString(tool["name"]))
			if name == "" {
				if fn, ok := tool["function"].(map[string]any); ok {
					name = strings.TrimSpace(asString(fn["name"]))
				}
			}
			if name == "" {
				name = typ
			}
			if name != "" {
				names = append(names, typ+":"+name)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			parts = append(parts, "t:"+strings.Join(names, ","))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "aff:" + hex.EncodeToString(sum[:8])
}

// ComposeStickyKey merges client identity and payload affinity.
func ComposeStickyKey(clientKey, affinityKey string) string {
	return ComposeStickyKeyParts(clientKey, "", affinityKey)
}

// ComposeStickyKeyParts merges client identity, official prompt_cache_key, and
// payload affinity. Session segment priority:
//
//	prompt_cache_key > client headers/auth > payload affinity
//
// When both a non-auth client key and prompt_cache_key exist they are joined so
// multi-tenant deployments behind one API key still isolate tenants.
func ComposeStickyKeyParts(clientKey, promptCacheKey, affinityKey string) string {
	clientKey = strings.TrimSpace(clientKey)
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	affinityKey = strings.TrimSpace(affinityKey)

	session := ""
	switch {
	case promptCacheKey != "":
		session = "cache:" + promptCacheKey
	case clientKey != "":
		session = clientKey
	case affinityKey != "":
		session = affinityKey
	default:
		return ""
	}

	if promptCacheKey != "" && clientKey != "" && !strings.HasPrefix(clientKey, "auth:") {
		return clientKey + "|" + session
	}
	if promptCacheKey == "" && clientKey != "" && affinityKey != "" && session == clientKey {
		return clientKey + "|" + affinityKey
	}
	return session
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}
