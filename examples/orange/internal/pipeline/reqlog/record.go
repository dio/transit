package reqlog

// Record is the finalized per-request audit record produced by the reqlog
// filter. It combines HTTP semantics, orange LLM metadata, upstream routing
// details, and Envoy stream-finalized counters into a single value.
type Record struct {
	// Identity
	RequestID string `json:"request_id,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
	SpanID    string `json:"span_id,omitempty"`

	// Request
	Method          string      `json:"method,omitempty"`
	Path            string      `json:"path,omitempty"`
	Host            string      `json:"host,omitempty"`
	RequestHeaders  [][2]string `json:"request_headers,omitempty"`
	RequestBody     string      `json:"request_body,omitempty"`
	RequestTruncated bool       `json:"request_truncated,omitempty"`

	// Response
	StatusCode       int         `json:"status_code,omitempty"`
	ResponseHeaders  [][2]string `json:"response_headers,omitempty"`
	ResponseBody     string      `json:"response_body,omitempty"`
	ResponseTruncated bool       `json:"response_truncated,omitempty"`

	// Orange LLM metadata
	Model                    string `json:"model,omitempty"`
	ProviderBackend          string `json:"provider_backend,omitempty"`
	ProviderKind             string `json:"provider_kind,omitempty"`
	Endpoint                 string `json:"endpoint,omitempty"`
	BackendModel             string `json:"backend_model,omitempty"`
	Passthrough              string `json:"passthrough,omitempty"`
	GatewayClient            string `json:"gateway_client,omitempty"`
	InputTokens              string `json:"input_tokens,omitempty"`
	OutputTokens             string `json:"output_tokens,omitempty"`
	CachedInputTokens        string `json:"cached_input_tokens,omitempty"`
	ReasoningOutputTokens    string `json:"reasoning_output_tokens,omitempty"`
	CacheCreationInputTokens string `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     string `json:"cache_read_input_tokens,omitempty"`
	ImageCount               string `json:"image_count,omitempty"`
	ImageSize                string `json:"image_size,omitempty"`
	ImageQuality             string `json:"image_quality,omitempty"`
	ResponseModalities       string `json:"response_modalities,omitempty"`

	// Upstream
	UpstreamAddress      string `json:"upstream_address,omitempty"`
	UpstreamLocalAddress string `json:"upstream_local_address,omitempty"`
	UpstreamAttempts     uint32 `json:"upstream_attempts,omitempty"`
	Protocol             string `json:"protocol,omitempty"`

	// Timing (nanoseconds; -1 means unavailable)
	DurationMs                   float64 `json:"duration_ms"`
	FirstUpstreamTxByteSentNs    int64   `json:"first_upstream_tx_byte_sent_ns,omitempty"`
	LastUpstreamRxByteReceivedNs int64   `json:"last_upstream_rx_byte_received_ns,omitempty"`
	UpstreamCxPoolReadyMs        float64 `json:"upstream_cx_pool_ready_ms,omitempty"`

	// Byte counts
	RequestSizeBytes  uint64 `json:"request_size_bytes,omitempty"`
	ResponseSizeBytes uint64 `json:"response_size_bytes,omitempty"`
	WireBytesReceived uint64 `json:"wire_bytes_received,omitempty"`
	WireBytesSent     uint64 `json:"wire_bytes_sent,omitempty"`

	// Error / flags
	HasError        bool   `json:"has_error"`
	ResponseFlags   string `json:"response_flags,omitempty"`
	ResponseDetails string `json:"response_details,omitempty"`
	UpstreamFailure string `json:"upstream_failure,omitempty"`
	LocalReplyBody  string `json:"local_reply_body,omitempty"`
}
