package tap_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dio/transit/examples/orange/tap"
	"github.com/dio/transit/up/buffer"
)

// --- ExtractUsageFromSSE ---

func feedSSE(chunks []string) tap.TokenUsage {
	ht := buffer.NewHeadTail(8*1024, 64*1024)
	for _, c := range chunks {
		ht.Write([]byte(c))
	}
	return tap.ExtractUsageFromSSE(ht.Head(), ht.Tail())
}

func TestExtractUsageFromSSE_OpenAI_Chat(t *testing.T) {
	sse := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n",
		"data: [DONE]\n\n",
	}, "")
	u := feedSSE([]string{sse})
	assert.Equal(t, uint32(10), u.Input)
	assert.Equal(t, uint32(5), u.Output)
}

func TestExtractUsageFromSSE_OpenAI_ResponsesAPI(t *testing.T) {
	sse := "event: response.completed\ndata: {\"usage\":{\"input_tokens\":20,\"output_tokens\":8}}\n\n"
	u := feedSSE([]string{sse})
	assert.Equal(t, uint32(20), u.Input)
	assert.Equal(t, uint32(8), u.Output)
}

func TestExtractUsageFromSSE_Anthropic(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start\n",
		"data: {\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"Hi\"}}\n\n",
		"event: message_delta\n",
		"data: {\"usage\":{\"output_tokens\":15}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}, "")
	u := feedSSE([]string{sse})
	assert.Equal(t, uint32(42), u.Input)
	assert.Equal(t, uint32(15), u.Output)
}

func TestExtractUsageFromSSE_LargeStream_HeadTailOnly(t *testing.T) {
	head := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":99}}}\n\n"
	middle := strings.Repeat("data: {\"delta\":{\"text\":\"x\"}}\n\n", 5000) // ~200 KB filler
	tail := "event: message_delta\ndata: {\"usage\":{\"output_tokens\":77}}\n\n"

	ht := buffer.NewHeadTail(8*1024, 64*1024)
	ht.Write([]byte(head))
	ht.Write([]byte(middle))
	ht.Write([]byte(tail))

	u := tap.ExtractUsageFromSSE(ht.Head(), ht.Tail())
	assert.Equal(t, uint32(99), u.Input)
	assert.Equal(t, uint32(77), u.Output)
}

func TestExtractUsageFromSSE_Empty(t *testing.T) {
	u := feedSSE(nil)
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

// --- ExtractUsageFromJSON ---

func TestExtractUsageFromJSON_OpenAI(t *testing.T) {
	body := `{"id":"chatcmpl-123","choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	u := tap.ExtractUsageFromJSON([]byte(body))
	assert.Equal(t, uint32(10), u.Input)
	assert.Equal(t, uint32(5), u.Output)
}

func TestExtractUsageFromJSON_Anthropic(t *testing.T) {
	body := `{"id":"msg_123","type":"message","content":[{"type":"text","text":"Hi"}],"usage":{"input_tokens":42,"output_tokens":15}}`
	u := tap.ExtractUsageFromJSON([]byte(body))
	assert.Equal(t, uint32(42), u.Input)
	assert.Equal(t, uint32(15), u.Output)
}

func TestExtractUsageFromJSON_ChunkedBody(t *testing.T) {
	// Simulate the body arriving in two chunks that get appended before EndStream.
	full := `{"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	// split arbitrarily at byte 20
	chunk1 := []byte(full[:20])
	chunk2 := []byte(full[20:])
	buf := append(chunk1, chunk2...)
	u := tap.ExtractUsageFromJSON(buf)
	assert.Equal(t, uint32(7), u.Input)
	assert.Equal(t, uint32(3), u.Output)
}

func TestExtractUsageFromJSON_NoUsageField(t *testing.T) {
	body := `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`
	u := tap.ExtractUsageFromJSON([]byte(body))
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

func TestExtractUsageFromJSON_Empty(t *testing.T) {
	u := tap.ExtractUsageFromJSON([]byte{})
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

func TestExtractUsageFromJSON_InvalidJSON(t *testing.T) {
	u := tap.ExtractUsageFromJSON([]byte(`{not valid`))
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}
