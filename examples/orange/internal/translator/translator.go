package translator

import "github.com/tidwall/sjson"

// sjsonOptions are the options used for sjson operations in the translator.
// Copied from ai-gateway/internal/translator/translator.go.
var sjsonOptions = &sjson.Options{
	Optimistic: true,
	// DO NOT set ReplaceInPlace: true — the translation layer may be called
	// multiple times per retry; the original body must not be modified.
	ReplaceInPlace: false,
}

// sjsonOptionsInPlace are the options used for sjson operations that modify in place.
// Note: ensure the original body is not modified when using this.
var sjsonOptionsInPlace = &sjson.Options{
	Optimistic:     true,
	ReplaceInPlace: true,
}

// Header pseudo-header and well-known header name constants.
// Copied from ai-gateway/internal/translator/translator.go.
const (
	pathHeaderName          = ":path"
	statusHeaderName        = ":status"
	contentTypeHeaderName   = "content-type"
	contentLengthHeaderName = "content-length"
	awsErrorTypeHeaderName  = "x-amzn-errortype"
	jsonContentType         = "application/json"
	eventStreamContentType  = "text/event-stream"
	openAIBackendError      = "OpenAIBackendError"
	awsBedrockBackendError  = "AWSBedrockBackendError"
)

// Header is a name/value header pair. Use ":path", ":method", "content-length",
// "content-type" etc. for pseudo-headers and control headers.
type Header struct {
	Name  string
	Value string
}

// Translator handles the full request/response lifecycle for one backend provider.
// A single Translator instance is created per request; it may accumulate
// streaming state across multiple ResponseBody calls.
//
// Auth credential injection is NOT the translator's concern — auth.go handles
// that in a separate step after RequestHeaders returns.
type Translator interface {
	// RequestHeaders is called with the decoded incoming request headers before
	// the body is available. Translators may record metadata (e.g., streaming
	// flag from Accept or a custom header) but must NOT produce :path here
	// because the model name (needed for GCP/Bedrock paths) lives in the body.
	//
	// Returns headers to add/override on the upstream request.
	RequestHeaders(headers map[string]string) ([]Header, error)

	// RequestBody translates the OpenAI-format request body to the backend
	// format. This is where :path MUST be set (it can embed the model name),
	// along with content-length and content-type when the body is mutated.
	//
	// raw is the complete, buffered request body.
	// Returns mutated headers and the new body. If mutatedBody is nil, the
	// original body is forwarded unchanged.
	RequestBody(raw []byte) (newHeaders []Header, mutatedBody []byte, err error)

	// ResponseHeaders is called with the upstream response headers before any
	// body bytes arrive. Use this to rewrite content-type when changing the
	// streaming framing (e.g., Amazon EventStream → text/event-stream).
	ResponseHeaders(headers map[string]string) ([]Header, error)

	// ResponseBody is called once per body chunk for streaming responses, or
	// once with the full body for non-streaming responses. endOfStream is true
	// on the final (or only) call.
	//
	// chunk may be empty on intermediate calls (Envoy heartbeat). The
	// Translator must buffer partial frames internally when framing boundaries
	// do not align with chunk boundaries (e.g., Amazon EventStream).
	//
	// Returns header mutations (only meaningful on the first call or when
	// endOfStream is true) and the translated body bytes to emit downstream.
	// Return mutatedBody = []byte{} (non-nil empty slice) to suppress the raw
	// upstream bytes without emitting anything — important for streaming
	// translators that emit a reframed event later.
	ResponseBody(chunk []byte, endOfStream bool) (newHeaders []Header, mutatedBody []byte, err error)
}
