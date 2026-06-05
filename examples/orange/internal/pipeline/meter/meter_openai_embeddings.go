package meter

import "encoding/json"

// ExtractOpenAIEmbeddingsJSON extracts token usage from a non-streaming OpenAI
// Embeddings response body.
//
// Embeddings usage shape (https://platform.openai.com/docs/api-reference/embeddings/create):
//
//	{"usage":{"prompt_tokens":N,"total_tokens":N}}
//
// Embeddings have no completion tokens; the count maps to Input only.
func ExtractOpenAIEmbeddingsJSON(body []byte) TokenUsage {
	var resp struct {
		Usage *struct {
			PromptTokens uint32 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return TokenUsage{}
	}
	return TokenUsage{Input: resp.Usage.PromptTokens}
}
