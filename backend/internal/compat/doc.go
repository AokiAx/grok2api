// Package compat converts between client-facing protocols and the Grok CLI
// /responses backend.
//
// Design follows a two-stage pipeline:
//
//	Stage A — client protocol → standard Responses
//	  ChatToResponses / AnthropicToResponses / NormalizeResponsesRequest
//	Stage B — Build upstream compatibility (single pass in Finalize/Sanitize)
//	  tools normalize + tool_choice align + input sanitize + field whitelist
//
// Layers (call order for a chat request):
//
//  1. NormalizeChatRequest — defaults, reasoning aliases
//  2. ChatToResponses / NormalizeResponsesRequest — field mapping + tools
//  3. ChatMessagesToResponsesInput — multi-turn tool history → Responses items
//     (images preserved as input_image; call_id paired in order)
//  4. FinalizeResponsesUpstream — whitelist, align backend_search; default-inject
//     web_search/x_search when the model SupportsBackendSearch, stream:true
//  5. AggregateResponsesStream / ResponsesToChatStream / ResponsesToChat — egress
//
// Anthropic Messages always use a direct path (no Chat hop):
//
//  1. AnthropicToResponses — Messages → Responses (thinking, web_search_*, images)
//  2. FinalizeResponsesUpstream — same as above
//  3. ResponsesToAnthropic / NewResponsesToAnthropicStream / AggregateResponsesToAnthropic — egress
//
// Tool policy highlights:
//   - Hard-reject non-enforceable web_search constraints (filters, allowed_domains)
//   - Soft-degrade Codex types with X-Grok2API-Compatibility-Warnings codes
//   - tool_choice stays coherent after search collapse (AlignResponsesToolChoice)
//   - Default search tool injection is opt-in (ModelHints.InjectDefaultSearchTools)
//   - Namespace/custom/tool_search/apply_patch get upstream aliases; ToolCompatibility
//     restores client identities on JSON/SSE before Chat/Anthropic re-encoding
//   - Client tool_search (execution=client) + defer_loading: deferred tools stay off
//     the wire until tool_search_output loads them into tools[]
//   - MCP defer_loading with client tool_search; computer_use_preview / unknown types rejected
//   - Response.tools restored to the client-facing declaration (visibleTools)
//   - History: agent_message, compaction_trigger, mcp_tool_call_output, additional_tools
//   - local_shell upgrades to native shell{environment:local}; shell_call restores
//     to local_shell_call when the request used legacy local_shell
//   - RequestError carries OpenAI-style param/code; /v1/messages uses Anthropic error envelope
//
// Model ids are passed through as-is (no Claude→Grok alias rewriting).
package compat
