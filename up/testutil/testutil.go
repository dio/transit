// Package testutil provides test helpers for up package filters.
// Import only from _test.go files.
package testutil

import (
	"sync"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"

	"github.com/dio/transit/down"
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
		filterState:      make(map[string][]byte),
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
// Filter state is stored in a real map so SetStreamObject / GetStreamObject
// round-trips work in tests (the nonce is written to filter state on first Set).
type FakeFilterHandle struct {
	mu sync.Mutex

	reqHeaders  *fake.FakeHeaderMap
	respHeaders *fake.FakeHeaderMap
	reqBody     *fake.FakeBodyBuffer
	respBody    *fake.FakeBodyBuffer
	metadata    map[string]map[string]any
	filterState map[string][]byte // stores per-stream filter state for test round-trips

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

// SetFilterState stores the value under key. Enables SetStreamObject /
// GetStreamObject round-trips in tests (the nonce is written here on first Set).
func (h *FakeFilterHandle) SetFilterState(key string, value []byte) {
	h.mu.Lock()
	if h.filterState == nil {
		h.filterState = make(map[string][]byte)
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	h.filterState[key] = cp
	h.mu.Unlock()
}

// GetFilterState returns the value previously set by SetFilterState.
func (h *FakeFilterHandle) GetFilterState(key string) (shared.UnsafeEnvoyBuffer, bool) {
	h.mu.Lock()
	v, ok := h.filterState[key]
	h.mu.Unlock()
	if !ok || len(v) == 0 {
		return shared.UnsafeEnvoyBuffer{}, ok
	}
	return shared.UnsafeEnvoyBuffer{Ptr: &v[0], Len: uint64(len(v))}, true
}

func (h *FakeFilterHandle) SetFilterStateTyped(_ string, _ []byte) bool { return false }
func (h *FakeFilterHandle) GetFilterStateTyped(_ string) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}

// FilterStateString returns the filter state value for key as a string.
// Convenience helper for test assertions.
func (h *FakeFilterHandle) FilterStateString(key string) (string, bool) {
	h.mu.Lock()
	v, ok := h.filterState[key]
	h.mu.Unlock()
	return string(v), ok
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

// =============================================================================
// FakeClusterLBContext — implements down.ClusterLBContext for unit tests
// =============================================================================

// FakeClusterLBContext is a minimal ClusterLBContext for unit tests.
// It reads the stream-object nonce from the same FakeFilterHandle used by
// the Writer under test, so SetStreamObject / GetStreamObject round-trips
// work end-to-end in tests without real Envoy.
//
// After the Writer flushes (or in directWrite mode, immediately), call
// SyncFilterStateFrom(handle) to copy the handle's current filter state into
// this context. Then GetStreamObject will return the correct bag entry.
type FakeClusterLBContext struct {
	mu          sync.Mutex
	filterState map[string]string
	headers     map[string]string
}

// NewFakeClusterLBContext returns a FakeClusterLBContext pre-loaded with the
// filter state from handle. Call it AFTER the Writer has flushed so the
// stream-object nonce is present in the handle.
func NewFakeClusterLBContext(handle *FakeFilterHandle) *FakeClusterLBContext {
	ctx := &FakeClusterLBContext{
		filterState: make(map[string]string),
		headers:     make(map[string]string),
	}
	ctx.SyncFilterStateFrom(handle)
	return ctx
}

// SyncFilterStateFrom copies the current filter state from handle into this
// context. Use after calling Writer methods that queue filter-state mutations
// and after any flush (or when directWrite=true).
func (c *FakeClusterLBContext) SyncFilterStateFrom(handle *FakeFilterHandle) {
	handle.mu.Lock()
	for k, v := range handle.filterState {
		c.filterState[k] = string(v)
	}
	handle.mu.Unlock()
}

func (c *FakeClusterLBContext) GetFilterState(key string) (string, bool) {
	c.mu.Lock()
	v, ok := c.filterState[key]
	c.mu.Unlock()
	return v, ok
}

func (c *FakeClusterLBContext) GetStreamObject(key string) (any, bool) {
	nonce, ok := c.GetFilterState(down.StreamObjectIDKey)
	if !ok || nonce == "" {
		return nil, false
	}
	bag, ok := down.LookupStreamObjectBag(nonce)
	if !ok {
		return nil, false
	}
	return bag.Get(key)
}

// Remaining ClusterLBContext methods — all no-ops for test use.
func (c *FakeClusterLBContext) GetAllHeaders() [][2]string                                 { return nil }
func (c *FakeClusterLBContext) GetFilterStateTyped(_ string) (string, bool)                { return "", false }
func (c *FakeClusterLBContext) GetOverrideHost() (string, bool)                            { return "", false }
func (c *FakeClusterLBContext) GetHeader(_ string) (string, bool)                          { return "", false }
func (c *FakeClusterLBContext) GetDownstreamSNI() (string, bool)                           { return "", false }
func (c *FakeClusterLBContext) ComputeHashKey() (uint64, bool)                             { return 0, false }
func (c *FakeClusterLBContext) GetHostSelectionRetryCount() uint32                         { return 0 }
func (c *FakeClusterLBContext) ShouldSelectAnotherHost(_ down.ClusterLBHandle, _ uint32, _ int) bool {
	return false
}
func (c *FakeClusterLBContext) NewCompletion() *down.ClusterLBCompletion { return nil }
