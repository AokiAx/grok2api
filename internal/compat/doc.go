// Package compat converts between client-facing protocols and the Grok CLI
// /responses backend.
//
// Layers (call order for a chat request):
//
//  1. NormalizeChatRequest — defaults, reasoning aliases
//  2. ChatToResponses / NormalizeResponsesRequest — field mapping + tools
//  3. ChatMessagesToResponsesInput — multi-turn tool history → Responses items
//  4. FinalizeResponsesUpstream — backend_search + default web_search/x_search tools, whitelist, stream:true
//  5. AggregateResponsesStream / ResponsesToChatStream / ResponsesToChat — egress
//
// Anthropic Messages always use a direct path (no Chat hop):
//
//  1. AnthropicToResponses — Messages → Responses (thinking signature, web_search_*, images)
//  2. FinalizeResponsesUpstream — same as above
//  3. ResponsesToAnthropic / NewResponsesToAnthropicStream / AggregateResponsesToAnthropic — egress
//
// Model ids are passed through as-is (no Claude→Grok alias rewriting).
package compat
