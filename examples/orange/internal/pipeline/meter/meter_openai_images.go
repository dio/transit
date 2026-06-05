package meter

import "encoding/json"

// ImageGenerationResult holds observable data extracted from an OpenAI-compatible
// image generation response. Token fields are populated only for models that
// include a usage object (e.g. gpt-image-1); they are zero for DALL-E 2/3.
// Size and Quality come from the response body when the model echoes them back;
// they fall back to request-side metadata stored by the match filter.
type ImageGenerationResult struct {
	Count   uint32
	Size    string
	Quality string
	Tokens  TokenUsage
}

// ExtractOpenAIImageGenerationsJSON extracts metering data from a non-streaming
// OpenAI Image Generations response body.
//
// Response shape:
//
//	{
//	  "created": N,
//	  "data": [{...}, ...],
//	  "usage": {"input_tokens": N, "output_tokens": N},  // gpt-image-1 only
//	  "size": "1024x1024",    // gpt-image-1 only
//	  "quality": "standard"   // gpt-image-1 only
//	}
func ExtractOpenAIImageGenerationsJSON(body []byte) ImageGenerationResult {
	var resp struct {
		Data    []struct{} `json:"data"`
		Size    string     `json:"size,omitempty"`
		Quality string     `json:"quality,omitempty"`
		Usage   *struct {
			InputTokens  uint32 `json:"input_tokens"`
			OutputTokens uint32 `json:"output_tokens"`
			InputTokensDetails *struct {
				ImageTokens uint32 `json:"image_tokens"`
			} `json:"input_tokens_details,omitempty"`
		} `json:"usage,omitempty"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return ImageGenerationResult{}
	}
	r := ImageGenerationResult{
		Count:   uint32(len(resp.Data)),
		Size:    resp.Size,
		Quality: resp.Quality,
	}
	if resp.Usage != nil {
		r.Tokens.Input = resp.Usage.InputTokens
		r.Tokens.Output = resp.Usage.OutputTokens
		if resp.Usage.InputTokensDetails != nil {
			r.Tokens.ImageInput = resp.Usage.InputTokensDetails.ImageTokens
		}
	}
	return r
}
