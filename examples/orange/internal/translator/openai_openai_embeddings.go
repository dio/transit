package translator

import (
	"fmt"
	"path"
	"strconv"

	"github.com/tidwall/sjson"
)

// openAIToOpenAITranslatorV1Embeddings is a passthrough translator for the
// OpenAI Embeddings API (POST /v1/embeddings). It rewrites the :path and
// optionally overrides the model name; everything else passes through unchanged.
type openAIToOpenAITranslatorV1Embeddings struct {
	backendModel string
	path         string
}

func (o *openAIToOpenAITranslatorV1Embeddings) RequestHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (o *openAIToOpenAITranslatorV1Embeddings) RequestBody(raw []byte) (newHeaders []Header, newBody []byte, err error) {
	if o.backendModel != "" {
		newBody, err = sjson.SetBytesOptions(raw, "model", o.backendModel, sjsonOptions)
		if err != nil {
			return nil, nil, fmt.Errorf("openai embeddings: set model: %w", err)
		}
	}
	newHeaders = []Header{{pathHeaderName, o.path}}
	if len(newBody) > 0 {
		newHeaders = append(newHeaders, Header{contentLengthHeaderName, strconv.Itoa(len(newBody))})
	}
	return newHeaders, newBody, nil
}

func (o *openAIToOpenAITranslatorV1Embeddings) ResponseHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (o *openAIToOpenAITranslatorV1Embeddings) ResponseBody(chunk []byte, _ bool) ([]Header, []byte, error) {
	return nil, nil, nil
}

func init() {
	Register("openai:embeddings", func(cfg ProviderConfig) Translator {
		return &openAIToOpenAITranslatorV1Embeddings{
			backendModel: cfg.BackendModel,
			path:         path.Join("/", cfg.PathPrefix, "embeddings"),
		}
	})
}
