package meter_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dio/transit/examples/orange/internal/pipeline/meter"
	"github.com/dio/transit/up/buffer"
)

// feedSSEAnthropic feeds chunks through a HeadTail buffer and runs extractAnthropicSSE.
func feedSSEAnthropic(chunks []string) meter.TokenUsage {
	ht := buffer.NewHeadTail(8*1024, 64*1024)
	for _, c := range chunks {
		ht.Write([]byte(c))
	}
	return meter.ExtractAnthropicSSE(ht.Head(), ht.Tail())
}

// feedSSEOpenAI feeds chunks through a HeadTail buffer and runs extractOpenAISSE.
func feedSSEOpenAI(chunks []string) meter.TokenUsage {
	ht := buffer.NewHeadTail(8*1024, 64*1024)
	for _, c := range chunks {
		ht.Write([]byte(c))
	}
	return meter.ExtractOpenAISSE(ht.Head(), ht.Tail())
}

// --- OpenAI SSE ---

func TestExtractOpenAISSE_Chat(t *testing.T) {
	sse := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n",
		"data: [DONE]\n\n",
	}, "")
	u := feedSSEOpenAI([]string{sse})
	assert.Equal(t, uint32(10), u.Input)
	assert.Equal(t, uint32(5), u.Output)
}

func TestExtractOpenAISSE_ResponsesAPI(t *testing.T) {
	sse := "event: response.completed\ndata: {\"usage\":{\"input_tokens\":20,\"output_tokens\":8}}\n\n"
	u := feedSSEOpenAI([]string{sse})
	assert.Equal(t, uint32(20), u.Input)
	assert.Equal(t, uint32(8), u.Output)
}

func TestExtractOpenAISSE_WithDetails(t *testing.T) {
	sse := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
		`data: {"usage":{"prompt_tokens":7,"completion_tokens":326,` +
			`"prompt_tokens_details":{"cached_tokens":100,"audio_tokens":0},` +
			`"completion_tokens_details":{"reasoning_tokens":256,"audio_tokens":0,` +
			`"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")
	u := feedSSEOpenAI([]string{sse})
	assert.Equal(t, uint32(7), u.Input)
	assert.Equal(t, uint32(326), u.Output)
	assert.Equal(t, uint32(100), u.CachedInput)
	assert.Equal(t, uint32(256), u.ReasoningOutput)
}

func TestExtractOpenAISSE_LargeStream_HeadTailOnly(t *testing.T) {
	head := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	middle := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", 5000)
	tail := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"

	ht := buffer.NewHeadTail(8*1024, 64*1024)
	ht.Write([]byte(head))
	ht.Write([]byte(middle))
	ht.Write([]byte(tail))

	u := meter.ExtractOpenAISSE(ht.Head(), ht.Tail())
	assert.Equal(t, uint32(10), u.Input)
	assert.Equal(t, uint32(5), u.Output)
}

func TestExtractOpenAISSE_Empty(t *testing.T) {
	u := feedSSEOpenAI(nil)
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

// --- Anthropic SSE ---

func TestExtractAnthropicSSE_Basic(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start\n",
		"data: {\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"Hi\"}}\n\n",
		"event: message_delta\n",
		"data: {\"usage\":{\"output_tokens\":15}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}, "")
	u := feedSSEAnthropic([]string{sse})
	assert.Equal(t, uint32(42), u.Input)
	assert.Equal(t, uint32(15), u.Output)
}

func TestExtractAnthropicSSE_WithCacheTokens(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start\n",
		`data: {"message":{"usage":{"input_tokens":8,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":30}}}}` + "\n\n",
		"event: message_delta\n",
		`data: {"usage":{"output_tokens":16}}` + "\n\n",
	}, "")
	u := feedSSEAnthropic([]string{sse})
	assert.Equal(t, uint32(8), u.Input)
	assert.Equal(t, uint32(16), u.Output)
	assert.Equal(t, uint32(100), u.CacheCreationInput)
	assert.Equal(t, uint32(200), u.CacheReadInput)
	assert.Equal(t, uint32(50), u.CacheEphemeral5m)
	assert.Equal(t, uint32(30), u.CacheEphemeral1h)
}

func TestExtractAnthropicSSE_LargeStream_HeadTailOnly(t *testing.T) {
	head := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":99}}}\n\n"
	middle := strings.Repeat("data: {\"delta\":{\"text\":\"x\"}}\n\n", 5000) // ~200 KB filler
	tail := "event: message_delta\ndata: {\"usage\":{\"output_tokens\":77}}\n\n"

	ht := buffer.NewHeadTail(8*1024, 64*1024)
	ht.Write([]byte(head))
	ht.Write([]byte(middle))
	ht.Write([]byte(tail))

	u := meter.ExtractAnthropicSSE(ht.Head(), ht.Tail())
	assert.Equal(t, uint32(99), u.Input)
	assert.Equal(t, uint32(77), u.Output)
}

func TestExtractAnthropicSSE_Empty(t *testing.T) {
	u := feedSSEAnthropic(nil)
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

// --- OpenAI JSON ---

func TestExtractOpenAIJSON_Chat(t *testing.T) {
	body := `{"id":"chatcmpl-123","choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	u := meter.ExtractOpenAIJSON([]byte(body))
	assert.Equal(t, uint32(10), u.Input)
	assert.Equal(t, uint32(5), u.Output)
}

func TestExtractOpenAIJSON_ResponsesAPI(t *testing.T) {
	body := `{"usage":{"input_tokens":20,"output_tokens":8}}`
	u := meter.ExtractOpenAIJSON([]byte(body))
	assert.Equal(t, uint32(20), u.Input)
	assert.Equal(t, uint32(8), u.Output)
}

func TestExtractOpenAIJSON_WithDetails(t *testing.T) {
	body := `{"usage":{"prompt_tokens":7,"completion_tokens":326,"total_tokens":333,` +
		`"prompt_tokens_details":{"cached_tokens":100,"audio_tokens":5},` +
		`"completion_tokens_details":{"reasoning_tokens":256,"audio_tokens":3,` +
		`"accepted_prediction_tokens":10,"rejected_prediction_tokens":2}}}`
	u := meter.ExtractOpenAIJSON([]byte(body))
	assert.Equal(t, uint32(7), u.Input)
	assert.Equal(t, uint32(326), u.Output)
	assert.Equal(t, uint32(100), u.CachedInput)
	assert.Equal(t, uint32(5), u.AudioInput)
	assert.Equal(t, uint32(256), u.ReasoningOutput)
	assert.Equal(t, uint32(3), u.AudioOutput)
	assert.Equal(t, uint32(10), u.AcceptedPredictionOutput)
	assert.Equal(t, uint32(2), u.RejectedPredictionOutput)
}

func TestExtractOpenAIJSON_ChunkedBody(t *testing.T) {
	full := `{"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	buf := append([]byte(full[:20]), []byte(full[20:])...)
	u := meter.ExtractOpenAIJSON(buf)
	assert.Equal(t, uint32(7), u.Input)
	assert.Equal(t, uint32(3), u.Output)
}

func TestExtractOpenAIJSON_NoUsage(t *testing.T) {
	u := meter.ExtractOpenAIJSON([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

func TestExtractOpenAIJSON_Empty(t *testing.T) {
	u := meter.ExtractOpenAIJSON([]byte{})
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

func TestExtractOpenAIJSON_Invalid(t *testing.T) {
	u := meter.ExtractOpenAIJSON([]byte(`{not valid`))
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

// --- Anthropic JSON ---

func TestExtractAnthropicJSON_Basic(t *testing.T) {
	body := `{"id":"msg_123","type":"message","content":[{"type":"text","text":"Hi"}],"usage":{"input_tokens":42,"output_tokens":15}}`
	u := meter.ExtractAnthropicJSON([]byte(body))
	assert.Equal(t, uint32(42), u.Input)
	assert.Equal(t, uint32(15), u.Output)
}

func TestExtractAnthropicJSON_WithCacheTokens(t *testing.T) {
	body := `{"usage":{"input_tokens":8,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":30},"output_tokens":16}}`
	u := meter.ExtractAnthropicJSON([]byte(body))
	assert.Equal(t, uint32(8), u.Input)
	assert.Equal(t, uint32(16), u.Output)
	assert.Equal(t, uint32(100), u.CacheCreationInput)
	assert.Equal(t, uint32(200), u.CacheReadInput)
	assert.Equal(t, uint32(50), u.CacheEphemeral5m)
	assert.Equal(t, uint32(30), u.CacheEphemeral1h)
}

func TestExtractAnthropicJSON_NoUsage(t *testing.T) {
	u := meter.ExtractAnthropicJSON([]byte(`{"type":"error","error":{"type":"not_found_error"}}`))
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

func TestExtractAnthropicJSON_Empty(t *testing.T) {
	u := meter.ExtractAnthropicJSON([]byte{})
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

func TestExtractAnthropicJSON_Invalid(t *testing.T) {
	u := meter.ExtractAnthropicJSON([]byte(`{not valid`))
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}
