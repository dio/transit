package translator

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/dio/transit/examples/orange/internal/apischema/openai"
)

// geminiImageRequest is the minimal Gemini generateContent request for image generation.
type geminiImageRequest struct {
	Contents         []geminiImageContent `json:"contents"`
	GenerationConfig geminiImageGenConfig `json:"generationConfig"`
}

type geminiImageContent struct {
	Parts []geminiImagePart `json:"parts"`
}

type geminiImagePart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiImageGenConfig struct {
	ResponseModalities []string `json:"responseModalities"`
	CandidateCount     int      `json:"candidateCount,omitempty"`
}

// geminiImageResponse is the minimal Gemini generateContent response for image generation.
type geminiImageResponse struct {
	Candidates    []geminiImageCandidate    `json:"candidates"`
	UsageMetadata *geminiImageUsageMetadata `json:"usageMetadata,omitempty"`
}

type geminiImageCandidate struct {
	Content geminiImageContent `json:"content"`
}

type geminiImageUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// gcpVertexAIToOpenAITranslatorV1Images translates OpenAI POST /v1/images/generations
// to the Gemini generateContent API (Google AI Studio) and maps the response back.
type gcpVertexAIToOpenAITranslatorV1Images struct {
	backendModel string
	// accumulated response body chunks (non-streaming)
	buf []byte
}

func (t *gcpVertexAIToOpenAITranslatorV1Images) RequestHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (t *gcpVertexAIToOpenAITranslatorV1Images) RequestBody(raw []byte) (newHeaders []Header, newBody []byte, err error) {
	var req openai.ImageGenerationRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		return nil, nil, fmt.Errorf("gemini images: decode request: %w", err)
	}

	model := req.Model
	if t.backendModel != "" {
		model = t.backendModel
	}

	gemReq := geminiImageRequest{
		Contents: []geminiImageContent{
			{Parts: []geminiImagePart{{Text: req.Prompt}}},
		},
		GenerationConfig: geminiImageGenConfig{
			ResponseModalities: []string{"IMAGE", "TEXT"},
		},
	}
	if req.N > 1 {
		gemReq.GenerationConfig.CandidateCount = req.N
	}

	newBody, err = json.Marshal(gemReq)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini images: marshal request: %w", err)
	}

	path := fmt.Sprintf("/v1beta/models/%s:generateContent", model)
	newHeaders = []Header{
		{pathHeaderName, path},
		{contentLengthHeaderName, strconv.Itoa(len(newBody))},
	}
	return newHeaders, newBody, nil
}

func (t *gcpVertexAIToOpenAITranslatorV1Images) ResponseHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (t *gcpVertexAIToOpenAITranslatorV1Images) ResponseBody(chunk []byte, endOfStream bool) (newHeaders []Header, newBody []byte, err error) {
	t.buf = append(t.buf, chunk...)
	if !endOfStream {
		return nil, []byte{}, nil
	}

	var gemResp geminiImageResponse
	if err = json.Unmarshal(t.buf, &gemResp); err != nil {
		return nil, nil, fmt.Errorf("gemini images: decode response: %w", err)
	}

	var data []openai.ImageGenerationResponseData
	for _, cand := range gemResp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				data = append(data, openai.ImageGenerationResponseData{
					B64JSON: part.InlineData.Data,
				})
			}
		}
	}

	resp := openai.ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    data,
	}
	var inputTokens, outputTokens int
	if m := gemResp.UsageMetadata; m != nil {
		inputTokens = m.PromptTokenCount
		outputTokens = m.CandidatesTokenCount
	}
	// Gemini 2.5 Flash Image does not report image output tokens in usageMetadata;
	// each generated image costs a fixed 1290 tokens.
	// https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/gemini/2-5-flash-image
	if outputTokens == 0 && len(data) > 0 {
		outputTokens = len(data) * 1290
	}
	if outputTokens > 0 || inputTokens > 0 {
		resp.Usage = &openai.ImageGenerationUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		}
	}
	newBody, err = json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini images: marshal response: %w", err)
	}

	newHeaders = []Header{
		{contentTypeHeaderName, jsonContentType},
		{contentLengthHeaderName, strconv.Itoa(len(newBody))},
	}
	return newHeaders, newBody, nil
}

func init() {
	Register("gcpvertexai:images", func(cfg ProviderConfig) Translator {
		return &gcpVertexAIToOpenAITranslatorV1Images{
			backendModel: cfg.BackendModel,
		}
	})
}
