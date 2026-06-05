package translator

import (
	"fmt"
	"path"
	"strconv"

	"github.com/tidwall/sjson"
)

// openAIToOpenAITranslatorV1Images is a passthrough translator for the
// OpenAI Image Generations API (POST /v1/images/generations). It rewrites
// the :path and optionally overrides the model name; everything else passes
// through unchanged.
type openAIToOpenAITranslatorV1Images struct {
	backendModel string
	path         string
}

func (o *openAIToOpenAITranslatorV1Images) RequestHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (o *openAIToOpenAITranslatorV1Images) RequestBody(raw []byte) (newHeaders []Header, newBody []byte, err error) {
	if o.backendModel != "" {
		newBody, err = sjson.SetBytesOptions(raw, "model", o.backendModel, sjsonOptions)
		if err != nil {
			return nil, nil, fmt.Errorf("openai images: set model: %w", err)
		}
	}
	newHeaders = []Header{{pathHeaderName, o.path}}
	if len(newBody) > 0 {
		newHeaders = append(newHeaders, Header{contentLengthHeaderName, strconv.Itoa(len(newBody))})
	}
	return newHeaders, newBody, nil
}

func (o *openAIToOpenAITranslatorV1Images) ResponseHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (o *openAIToOpenAITranslatorV1Images) ResponseBody(_ []byte, _ bool) ([]Header, []byte, error) {
	return nil, nil, nil
}

func init() {
	Register("openai:images", func(cfg ProviderConfig) Translator {
		return &openAIToOpenAITranslatorV1Images{
			backendModel: cfg.BackendModel,
			path:         path.Join("/", cfg.PathPrefix, "images/generations"),
		}
	})
}
