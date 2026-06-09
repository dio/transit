package meter

import "encoding/json"

// ExtractGeminiImageGenerationsJSON extracts metering data from a native Gemini
// generateContent response for image generation.
//
// Response shape:
//
//	{
//	  "candidates": [
//	    {"content": {"parts": [{"inlineData": {"mimeType": "image/png", "data": "..."}}]}}
//	  ],
//	  "usageMetadata": {"promptTokenCount": N, "candidatesTokenCount": N, "totalTokenCount": N}
//	}
//
// Gemini 2.5 Flash Image does not report output tokens in usageMetadata; each
// generated image is counted at a fixed 1290 tokens as a fallback.
func ExtractGeminiImageGenerationsJSON(body []byte) ImageGenerationResult {
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						Data string `json:"data"`
					} `json:"inlineData,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     uint32 `json:"promptTokenCount"`
			CandidatesTokenCount uint32 `json:"candidatesTokenCount"`
		} `json:"usageMetadata,omitempty"`
	}
	if json.Unmarshal(body, &resp) != nil || len(resp.Candidates) == 0 {
		return ImageGenerationResult{}
	}

	var count uint32
	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				count++
			}
		}
	}

	r := ImageGenerationResult{Count: count}
	if m := resp.UsageMetadata; m != nil {
		r.Tokens.Input = m.PromptTokenCount
		r.Tokens.Output = m.CandidatesTokenCount
	}
	if r.Tokens.Output == 0 && count > 0 {
		r.Tokens.Output = count * 1290
	}
	return r
}
