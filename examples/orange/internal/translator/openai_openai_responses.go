package translator

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"

	"github.com/tidwall/sjson"
)

// NewResponsesOpenAIToOpenAITranslator implements [Factory] for native OpenAI
// Responses API passthrough. It rewrites the model field and :path, fixes
// content-length when the body changes, and forwards the response body
// (JSON or SSE) unchanged.
func NewResponsesOpenAIToOpenAITranslator(cfg ProviderConfig) Translator {
	return &openAIToOpenAITranslatorV1Responses{
		backendModel: cfg.BackendModel,
		path:         path.Join("/", cfg.PathPrefix, "responses"),
	}
}

type openAIToOpenAITranslatorV1Responses struct {
	backendModel string
	// path is the upstream :path value, honouring PathPrefix.
	path string
	// stream records whether the request had "stream": true, for future
	// instrumentation; the response body is forwarded regardless.
	stream bool
}

// RequestHeaders implements Translator.RequestHeaders. No mutations needed at
// header phase; model is only available in the body.
func (o *openAIToOpenAITranslatorV1Responses) RequestHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

// RequestBody implements Translator.RequestBody. Rewrites model when a backend
// override is configured, sets :path to the responses endpoint, and updates
// content-length when the body changes.
func (o *openAIToOpenAITranslatorV1Responses) RequestBody(raw []byte) (newHeaders []Header, newBody []byte, err error) {
	// Capture stream flag for instrumentation (response body is forwarded as-is
	// regardless, so the flag only matters for future metering hooks).
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(raw, &req)
	o.stream = req.Stream

	if o.backendModel != "" {
		newBody, err = sjson.SetBytesOptions(raw, "model", o.backendModel, sjsonOptions)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to set model in responses body: %w", err)
		}
	}

	newHeaders = []Header{{pathHeaderName, o.path}}
	if len(newBody) > 0 {
		newHeaders = append(newHeaders, Header{contentLengthHeaderName, strconv.Itoa(len(newBody))})
	}
	return
}

// ResponseHeaders implements Translator.ResponseHeaders. No rewriting needed;
// the upstream returns correct content-type for both JSON and SSE responses.
func (o *openAIToOpenAITranslatorV1Responses) ResponseHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

// ResponseBody implements Translator.ResponseBody. Forwards all chunks
// unchanged — the native Responses response format (JSON object or SSE event
// stream) is already compatible with what the client expects.
func (o *openAIToOpenAITranslatorV1Responses) ResponseBody(_ []byte, _ bool) ([]Header, []byte, error) {
	return nil, nil, nil
}

func init() {
	Register("openai:responses", func(cfg ProviderConfig) Translator {
		return NewResponsesOpenAIToOpenAITranslator(cfg)
	})
}
