// Package compat converts between client-facing protocols and the Grok CLI
// /responses backend.
//
// Layers (call order for a chat request):
//
//  1. NormalizeChatRequest — defaults, reasoning aliases
//  2. ChatToResponses / NormalizeResponsesRequest — field mapping + tools
//  3. ChatMessagesToResponsesInput — multi-turn tool history → Responses items
//  4. FinalizeResponsesUpstream — search policy, whitelist, force stream:true
//  5. AggregateResponsesStream / ResponsesToChatStream / ResponsesToChat — egress
//
// Anthropic Messages enter via AnthropicToOpenAI then the same Chat path.
package compat
