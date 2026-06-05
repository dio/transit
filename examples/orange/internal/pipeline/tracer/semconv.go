// Package tracer provides OpenInference-conformant OTLP tracing for orange
// LLM and MCP calls. Semantic convention constants are inlined here following
// the same pattern as envoyproxy/ai-gateway to avoid an external dependency on
// the unpublished openinference-semantic-conventions module.
package tracer

// OpenInference span-kind constants.
const (
	oiSpanKind = "openinference.span.kind"

	oiSpanKindLLM       = "LLM"
	oiSpanKindEmbedding = "EMBEDDING"
	oiSpanKindTool      = "TOOL"
	oiSpanKindChain     = "CHAIN"
)

// LLM attributes.
const (
	oiLLMSystem               = "llm.system"
	oiLLMModelName            = "llm.model_name"
	oiLLMInvocationParameters = "llm.invocation_parameters"
	oiLLMFinishReason         = "llm.finish_reason"

	// Prefixes — individual messages are addressed via indexed helpers.
	oiLLMInputMessages  = "llm.input_messages"
	oiLLMOutputMessages = "llm.output_messages"

	oiMessageRole    = "message.role"
	oiMessageContent = "message.content"
)

// Token count attributes.
const (
	oiTokenCountPrompt      = "llm.token_count.prompt"
	oiTokenCountCompletion  = "llm.token_count.completion"
	oiTokenCountTotal       = "llm.token_count.total"
	oiTokenCountCacheRead   = "llm.token_count.prompt_details.cache_read"
	oiTokenCountCacheWrite  = "llm.token_count.prompt_details.cache_write"
	oiTokenCountReasoning   = "llm.token_count.completion_details.reasoning"
)

// Input / output attributes.
const (
	oiInputValue    = "input.value"
	oiInputMIME     = "input.mime_type"
	oiOutputValue   = "output.value"
	oiOutputMIME    = "output.mime_type"
	oiMIMETypeJSON  = "application/json"
)

// Embedding attributes.
const (
	oiEmbeddingModelName            = "embedding.model_name"
	oiEmbeddingInvocationParameters = "embedding.invocation_parameters"
)

// Tool / MCP attributes.
const (
	oiToolName        = "tool.name"
	oiToolParameters  = "tool.parameters"
	oiToolDescription = "tool.description"
)

// LLM system values.
const (
	oiLLMSystemOpenAI    = "openai"
	oiLLMSystemAnthropic = "anthropic"
)

// outputMessageAttr returns the indexed attribute key for an output message field.
func outputMessageAttr(i int, suffix string) string {
	return formatIndexed(oiLLMOutputMessages, i, "message", suffix)
}

func formatIndexed(prefix string, i int, mid, suffix string) string {
	// Avoid fmt.Sprintf allocation hot path; this is only called at span-end time.
	return prefix + "." + itoa(i) + "." + mid + "." + suffix
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	// Rare: more than 9 messages.
	b := make([]byte, 0, 3)
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
