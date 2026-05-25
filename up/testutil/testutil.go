// Package testutil provides test helpers for up package filters.
// Import only from _test.go files.
package testutil

import (
	"sync"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
)

// LocalResponse records a single SendLocalResponse call for test assertions.
type LocalResponse struct {
	Status  uint32
	Headers [][2]string
	Body    []byte
	Detail  string
}

// FilterHandleOption configures a FakeFilterHandle.
type FilterHandleOption func(*FakeFilterHandle)

// HTTPCalloutFunc simulates Envoy HTTP callout initialization.
type HTTPCalloutFunc func(cluster string, headers [][2]string, body []byte, timeoutMs uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64)

// WithHeaders sets the request headers on the fake handle.
func WithHeaders(headers map[string]string) FilterHandleOption {
	return func(h *FakeFilterHandle) {
		m := make(map[string][]string, len(headers))
		for k, v := range headers {
			m[k] = []string{v}
		}
		h.reqHeaders = fake.NewFakeHeaderMap(m)
	}
}

// WithResponseHeaders sets the response headers on the fake handle.
func WithResponseHeaders(headers map[string]string) FilterHandleOption {
	return func(h *FakeFilterHandle) {
		m := make(map[string][]string, len(headers))
		for k, v := range headers {
			m[k] = []string{v}
		}
		h.respHeaders = fake.NewFakeHeaderMap(m)
	}
}

// WithHTTPCalloutFunc configures fake HTTP callout behavior.
func WithHTTPCalloutFunc(fn HTTPCalloutFunc) FilterHandleOption {
	return func(h *FakeFilterHandle) {
		h.httpCallout = fn
	}
}

// NewFilterHandle constructs a FakeFilterHandle with the given options applied.
func NewFilterHandle(opts ...FilterHandleOption) *FakeFilterHandle {
	h := &FakeFilterHandle{
		reqHeaders:       fake.NewFakeHeaderMap(nil),
		respHeaders:      fake.NewFakeHeaderMap(nil),
		reqBody:          fake.NewFakeBodyBuffer(nil),
		respBody:         fake.NewFakeBodyBuffer(nil),
		metadata:         make(map[string]map[string]any),
		ContinueRequestC: make(chan struct{}),
		LocalResponseC:   make(chan struct{}),
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// FakeFilterHandle implements shared.HttpFilterHandle for unit tests.
// SendLocalResponse records calls into LocalResponses for assertions.
// All other methods are no-ops returning zero values.
type FakeFilterHandle struct {
	mu sync.Mutex

	reqHeaders  *fake.FakeHeaderMap
	respHeaders *fake.FakeHeaderMap
	reqBody     *fake.FakeBodyBuffer
	respBody    *fake.FakeBodyBuffer
	metadata    map[string]map[string]any

	// LocalResponses records every SendLocalResponse call.
	LocalResponses   []LocalResponse
	ContinuedReq     int
	ContinueRequestC chan struct{}
	LocalResponseC   chan struct{}
	continueOnce     sync.Once
	localOnce        sync.Once
	httpCallout      HTTPCalloutFunc
}

// -- HeaderMap accessors --

func (h *FakeFilterHandle) RequestHeaders() shared.HeaderMap  { return h.reqHeaders }
func (h *FakeFilterHandle) ResponseHeaders() shared.HeaderMap { return h.respHeaders }
func (h *FakeFilterHandle) RequestTrailers() shared.HeaderMap {
	return fake.NewFakeHeaderMap(nil)
}
func (h *FakeFilterHandle) ResponseTrailers() shared.HeaderMap {
	return fake.NewFakeHeaderMap(nil)
}

// -- Body accessors --

func (h *FakeFilterHandle) BufferedRequestBody() shared.BodyBuffer  { return h.reqBody }
func (h *FakeFilterHandle) ReceivedRequestBody() shared.BodyBuffer  { return h.reqBody }
func (h *FakeFilterHandle) BufferedResponseBody() shared.BodyBuffer { return h.respBody }
func (h *FakeFilterHandle) ReceivedResponseBody() shared.BodyBuffer { return h.respBody }
func (h *FakeFilterHandle) ReceivedBufferedRequestBody() bool       { return false }
func (h *FakeFilterHandle) ReceivedBufferedResponseBody() bool      { return false }

// -- Flow control --

func (h *FakeFilterHandle) ContinueRequest() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ContinuedReq++
	h.continueOnce.Do(func() { close(h.ContinueRequestC) })
}
func (h *FakeFilterHandle) ContinueResponse()    {}
func (h *FakeFilterHandle) ClearRouteCache()     {}
func (h *FakeFilterHandle) RefreshRouteCluster() {}

// -- Local response --

func (h *FakeFilterHandle) SendLocalResponse(status uint32, headers [][2]string, body []byte, detail string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.LocalResponses = append(h.LocalResponses, LocalResponse{
		Status:  status,
		Headers: headers,
		Body:    body,
		Detail:  detail,
	})
	h.localOnce.Do(func() { close(h.LocalResponseC) })
}

func (h *FakeFilterHandle) SendResponseHeaders(_ [][2]string, _ bool) {}
func (h *FakeFilterHandle) SendResponseData(_ []byte, _ bool)         {}
func (h *FakeFilterHandle) SendResponseTrailers(_ [][2]string)        {}

// -- Metadata --

func (h *FakeFilterHandle) SetMetadata(ns, key string, value any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.metadata[ns] == nil {
		h.metadata[ns] = make(map[string]any)
	}
	h.metadata[ns][key] = value
}

func (h *FakeFilterHandle) GetMetadataString(_ shared.MetadataSourceType, ns, key string) (shared.UnsafeEnvoyBuffer, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.metadata[ns][key]; ok {
		if s, ok := v.(string); ok {
			if len(s) == 0 {
				return shared.UnsafeEnvoyBuffer{}, true
			}
			b := []byte(s)
			return shared.UnsafeEnvoyBuffer{Ptr: &b[0], Len: uint64(len(b))}, true
		}
	}
	return shared.UnsafeEnvoyBuffer{}, false
}

func (h *FakeFilterHandle) GetMetadataNumber(_ shared.MetadataSourceType, ns, key string) (float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.metadata[ns][key]; ok {
		if f, ok := v.(float64); ok {
			return f, true
		}
	}
	return 0, false
}

func (h *FakeFilterHandle) GetMetadataBool(_ shared.MetadataSourceType, ns, key string) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.metadata[ns][key]; ok {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

// Metadata returns the raw stored value for (ns, key) as a string and whether it exists.
// Only usable when the value was stored as a string.
func (h *FakeFilterHandle) Metadata(ns, key string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.metadata[ns][key]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// MetadataNumber returns the raw stored value for (ns, key) as float64 and whether it exists.
func (h *FakeFilterHandle) MetadataNumber(ns, key string) (float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.metadata[ns][key]; ok {
		if f, ok := v.(float64); ok {
			return f, true
		}
	}
	return 0, false
}

// MetadataBool returns the raw stored value for (ns, key) as bool and whether it exists.
func (h *FakeFilterHandle) MetadataBool(ns, key string) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.metadata[ns][key]; ok {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

// ClearMetadata removes a single metadata entry for test isolation.
func (h *FakeFilterHandle) ClearMetadata(ns, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.metadata[ns] != nil {
		delete(h.metadata[ns], key)
	}
}

func (h *FakeFilterHandle) GetMetadataKeys(_ shared.MetadataSourceType, _ string) []shared.UnsafeEnvoyBuffer {
	return nil
}
func (h *FakeFilterHandle) GetMetadataNamespaces(_ shared.MetadataSourceType) []shared.UnsafeEnvoyBuffer {
	return nil
}
func (h *FakeFilterHandle) AddMetadataListNumber(_, _ string, _ float64) bool { return false }
func (h *FakeFilterHandle) AddMetadataListString(_, _, _ string) bool         { return false }
func (h *FakeFilterHandle) AddMetadataListBool(_, _ string, _ bool) bool      { return false }
func (h *FakeFilterHandle) GetMetadataListSize(_ shared.MetadataSourceType, _, _ string) (int, bool) {
	return 0, false
}
func (h *FakeFilterHandle) GetMetadataListNumber(_ shared.MetadataSourceType, _, _ string, _ int) (float64, bool) {
	return 0, false
}
func (h *FakeFilterHandle) GetMetadataListString(_ shared.MetadataSourceType, _, _ string, _ int) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (h *FakeFilterHandle) GetMetadataListBool(_ shared.MetadataSourceType, _, _ string, _ int) (bool, bool) {
	return false, false
}

// -- Attributes --

func (h *FakeFilterHandle) GetAttributeString(_ shared.AttributeID) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (h *FakeFilterHandle) GetAttributeNumber(_ shared.AttributeID) (float64, bool) {
	return 0, false
}
func (h *FakeFilterHandle) GetAttributeBool(_ shared.AttributeID) (bool, bool) { return false, false }

// -- Filter state --

func (h *FakeFilterHandle) GetFilterState(_ string) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (h *FakeFilterHandle) SetFilterState(_ string, _ []byte)           {}
func (h *FakeFilterHandle) SetFilterStateTyped(_ string, _ []byte) bool { return false }
func (h *FakeFilterHandle) GetFilterStateTyped(_ string) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}

// -- Cross-phase data --

func (h *FakeFilterHandle) GetData(_ string) any       { return nil }
func (h *FakeFilterHandle) SetData(_ string, _ any)    {}
func (h *FakeFilterHandle) GetMostSpecificConfig() any { return nil }

// -- Logging --

func (h *FakeFilterHandle) Log(_ shared.LogLevel, _ string, _ ...any) {}
func (h *FakeFilterHandle) LogEnabled(_ shared.LogLevel) bool         { return false }

// -- Scheduler: runs fn() synchronously --

func (h *FakeFilterHandle) GetScheduler() shared.Scheduler { return &fakeScheduler{} }

type fakeScheduler struct{}

func (s *fakeScheduler) Schedule(fn func()) { fn() }

// -- HTTP callout / stream (no-ops) --

func (h *FakeFilterHandle) HttpCallout(cluster string, headers [][2]string, body []byte, timeoutMs uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
	if h.httpCallout != nil {
		return h.httpCallout(cluster, headers, body, timeoutMs, cb)
	}
	return shared.HttpCalloutInitClusterNotFound, 0
}
func (h *FakeFilterHandle) StartHttpStream(_ string, _ [][2]string, _ []byte, _ bool, _ uint64, _ shared.HttpStreamCallback) (shared.HttpCalloutInitResult, uint64) {
	return shared.HttpCalloutInitClusterNotFound, 0
}
func (h *FakeFilterHandle) SendHttpStreamData(_ uint64, _ []byte, _ bool) bool  { return false }
func (h *FakeFilterHandle) SendHttpStreamTrailers(_ uint64, _ [][2]string) bool { return false }
func (h *FakeFilterHandle) ResetHttpStream(_ uint64)                            {}

// -- Watermarks --

func (h *FakeFilterHandle) SetDownstreamWatermarkCallbacks(_ shared.DownstreamWatermarkCallbacks) {}
func (h *FakeFilterHandle) ClearDownstreamWatermarkCallbacks()                                    {}

// -- Metrics (no-ops) --

func (h *FakeFilterHandle) RecordHistogramValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (h *FakeFilterHandle) SetGaugeValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (h *FakeFilterHandle) IncrementGaugeValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (h *FakeFilterHandle) DecrementGaugeValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (h *FakeFilterHandle) IncrementCounterValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}

// -- Misc --

func (h *FakeFilterHandle) AddCustomFlag(_ string)  {}
func (h *FakeFilterHandle) GetWorkerIndex() uint32  { return 0 }
func (h *FakeFilterHandle) GetBufferLimit() uint64  { return 0 }
func (h *FakeFilterHandle) SetBufferLimit(_ uint64) {}
func (h *FakeFilterHandle) GetActiveSpan() shared.Span {
	return nil
}
func (h *FakeFilterHandle) GetClusterName() (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (h *FakeFilterHandle) GetClusterHostCounts(_ uint32) (shared.ClusterHostCounts, bool) {
	return shared.ClusterHostCounts{}, false
}
func (h *FakeFilterHandle) SetUpstreamOverrideHost(_ string, _ bool) bool              { return false }
func (h *FakeFilterHandle) ResetStream(_ shared.HttpFilterStreamResetReason, _ string) {}
func (h *FakeFilterHandle) SendGoAwayAndClose(_ bool)                                  {}
func (h *FakeFilterHandle) RecreateStream(_ [][2]string) bool                          { return false }
func (h *FakeFilterHandle) SetSocketOptionInt(_, _ int64, _ shared.SocketOptionState, _ shared.SocketDirection, _ int64) bool {
	return false
}
func (h *FakeFilterHandle) SetSocketOptionBytes(_, _ int64, _ shared.SocketOptionState, _ shared.SocketDirection, _ []byte) bool {
	return false
}
func (h *FakeFilterHandle) GetSocketOptionInt(_, _ int64, _ shared.SocketOptionState, _ shared.SocketDirection) (int64, bool) {
	return 0, false
}
func (h *FakeFilterHandle) GetSocketOptionBytes(_, _ int64, _ shared.SocketOptionState, _ shared.SocketDirection) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
