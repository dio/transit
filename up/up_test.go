package up

import (
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/dio/transit/up/testutil"
)

// --- newRequest ---

func TestNewRequest_allHeaders(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		":method":    {"GET"},
		":path":      {"/test"},
		":authority": {"example.com"},
	})
	r := newRequest(headers, "")
	require.Equal(t, "GET", r.Method)
	require.Equal(t, "/test", r.Path)
	require.Equal(t, "example.com", r.Host)
}

func TestNewRequest_missingHeaders(t *testing.T) {
	r := newRequest(fake.NewFakeHeaderMap(nil), "")
	require.Empty(t, r.Method)
	require.Empty(t, r.Path)
	require.Empty(t, r.Host)
}

func TestNewRequest_partialHeaders(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		":method": {"POST"},
	})
	r := newRequest(headers, "")
	require.Equal(t, "POST", r.Method)
	require.Empty(t, r.Path)
	require.Empty(t, r.Host)
}

func TestNewRequest_variousMethods(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
		r := newRequest(fake.NewFakeHeaderMap(map[string][]string{":method": {method}}), "")
		require.Equal(t, method, r.Method)
	}
}

// --- Request.AllHeaders ---

func TestRequest_AllHeaders(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		":method":      {"GET"},
		":path":        {"/hello"},
		"content-type": {"application/json"},
	})
	r := newRequest(headers, "")
	got := r.AllHeaders()
	require.Len(t, got, 3)
	found := map[string]string{}
	for _, h := range got {
		found[h[0]] = h[1]
	}
	require.Equal(t, "GET", found[":method"])
	require.Equal(t, "/hello", found[":path"])
	require.Equal(t, "application/json", found["content-type"])
}

func TestRequest_AllHeaders_nil(t *testing.T) {
	r := &Request{}
	require.Nil(t, r.AllHeaders())
}

// --- ResponseChunk.AllHeaders ---

func TestResponseChunk_AllHeaders(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		":status":      {"200"},
		"content-type": {"text/plain"},
	})
	chunk := &ResponseChunk{StatusCode: 200, Headers: headers}
	got := chunk.AllHeaders()
	require.Len(t, got, 2)
	found := map[string]string{}
	for _, h := range got {
		found[h[0]] = h[1]
	}
	require.Equal(t, "200", found[":status"])
	require.Equal(t, "text/plain", found["content-type"])
}

func TestResponseChunk_AllHeaders_nil(t *testing.T) {
	chunk := &ResponseChunk{}
	require.Nil(t, chunk.AllHeaders())
}

// --- Writer ---

func TestWriter_Log_delegatesToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelError, "msg: %s", "arg")

	w := NewWriter(handle)
	w.Log(LogError, "msg: %s", "arg")
}

func TestWriter_Log_allLevels(t *testing.T) {
	levels := []struct {
		up  LogLevel
		raw shared.LogLevel
	}{
		{up: LogTrace, raw: shared.LogLevelTrace},
		{up: LogDebug, raw: shared.LogLevelDebug},
		{up: LogInfo, raw: shared.LogLevelInfo},
		{up: LogWarn, raw: shared.LogLevelWarn},
		{up: LogError, raw: shared.LogLevelError},
		{up: LogCritical, raw: shared.LogLevelCritical},
	}
	for _, level := range levels {
		ctrl := gomock.NewController(t)
		handle := newMockHandle(ctrl)
		handle.EXPECT().Log(level.raw, "test", gomock.Any())
		NewWriter(handle).Log(level.up, "test")
	}
}

func TestNewWriter_returnsWriterBackedByHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelInfo, "hi")

	w := NewWriter(handle)
	require.NotNil(t, w)
	w.Log(LogInfo, "hi")
}

func TestWriter_SetFilterState_delegatesToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().SetFilterState("llm.target", []byte("api.openai.com:443"))

	w := NewWriter(handle)
	w.SetFilterState("llm.target", "api.openai.com:443")
	w.f.flush(false) // mutations queue unconditionally; flush applies them
}

func TestWriter_SetUpstreamOverrideHost_delegatesToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().SetUpstreamOverrideHost("api.openai.com:443", true).Return(true)

	w := NewWriter(handle)
	ok := w.SetUpstreamOverrideHost("api.openai.com:443", true)
	w.f.flush(false)

	require.True(t, ok)
}

// --- configFactory ---

func TestConfigFactory_Create_returnsFilterFactory(t *testing.T) {
	h := HandlerFunc(func(w *Writer, r *Request) {})
	cf := &configFactory{handler: h}

	ff, err := cf.Create(nil, nil)
	require.NoError(t, err)
	require.NotNil(t, ff)
	_, ok := ff.(*filterFactory)
	require.True(t, ok)
}

func TestConfigFactory_Create_preservesHandler(t *testing.T) {
	called := false
	h := HandlerFunc(func(w *Writer, r *Request) { called = true })
	cf := &configFactory{handler: h}

	ff, _ := cf.Create(nil, nil)
	fac := ff.(*filterFactory)
	require.NotNil(t, fac.handler)
	fac.handler(nil, nil)
	require.True(t, called)
}

func TestConfigFactory_CreatePerRoute_returnsNil(t *testing.T) {
	cf := &configFactory{}
	v, err := cf.CreatePerRoute(nil)
	require.NoError(t, err)
	require.Nil(t, v)
}

// --- filterFactory ---

func TestFilterFactory_Create_returnsFilter(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	ff := &filterFactory{handler: func(w *Writer, r *Request) {}}
	f := ff.Create(handle)
	require.NotNil(t, f)
	_, ok := f.(*filter)
	require.True(t, ok)
}

func TestFilterFactory_Create_wiredToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelWarn, "check")

	ff := &filterFactory{handler: func(w *Writer, r *Request) {
		w.Log(LogWarn, "check")
	}}
	f := ff.Create(handle).(*filter)
	f.handler(f.writer(), nil)
}

// --- filter.OnRequestHeaders ---

func TestFilter_OnRequestHeaders_returnsStatusContinue(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	f := &filter{handle: handle, handler: func(w *Writer, r *Request) {}}
	status := f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), false)
	require.Equal(t, shared.HeadersStatusContinue, status)
}

func TestFilter_OnRequestHeaders_passesRequestFields(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var got Request
	f := &filter{handle: handle, handler: func(w *Writer, r *Request) { got = *r }}

	headers := fake.NewFakeHeaderMap(map[string][]string{
		":method":    {"DELETE"},
		":path":      {"/api/v1"},
		":authority": {"api.example.com"},
	})
	f.OnRequestHeaders(headers, false)

	require.Equal(t, "DELETE", got.Method)
	require.Equal(t, "/api/v1", got.Path)
	require.Equal(t, "api.example.com", got.Host)
}

func TestFilter_OnRequestHeaders_writerDelegatesToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelInfo, "test: %d", 42)

	f := &filter{
		handle:  handle,
		handler: func(w *Writer, r *Request) { w.Log(LogInfo, "test: %d", 42) },
	}
	f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), false)
}

func TestFilter_OnRequestHeaders_endOfStreamFlagIgnored(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	calls := 0
	f := &filter{handle: handle, handler: func(w *Writer, r *Request) { calls++ }}

	f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), false)
	f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), true)
	require.Equal(t, 2, calls)
}

// --- Request.Header ---

func TestRequest_Header_present(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		"x-api-key": {"secret"},
	})
	r := newRequest(headers, "")
	require.Equal(t, "secret", r.Header("x-api-key"))
}

func TestRequest_Header_missing(t *testing.T) {
	r := newRequest(fake.NewFakeHeaderMap(nil), "")
	require.Empty(t, r.Header("x-api-key"))
}

func TestRequest_Header_nilHeaders(t *testing.T) {
	r := &Request{}
	require.Empty(t, r.Header("x-api-key"))
}

func TestRequest_Header_caseInsensitive(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		"X-Request-ID": {"abc-123"},
	})
	r := newRequest(headers, "")
	require.Equal(t, "abc-123", r.Header("x-request-id"))
}

// --- Writer.SendLocalResponse ---

func TestWriter_SendLocalResponse_delegatesToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().SendLocalResponse(uint32(401), gomock.Any(), []byte(`{"error":"no key"}`), "")

	w := NewWriter(handle)
	w.SendLocalResponse(401, []byte(`{"error":"no key"}`))
	require.True(t, w.f.stopped)
	w.f.flush(false)
}

func TestWriter_SendLocalResponse_setsStoppedFlag(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().SendLocalResponse(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any())

	w := NewWriter(handle)
	require.False(t, w.f.stopped)
	w.SendLocalResponse(200, nil)
	require.True(t, w.f.stopped)
	w.f.flush(false)
}

func TestFilter_OnRequestHeaders_stopsAfterLocalResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	handle.EXPECT().SendLocalResponse(uint32(401), gomock.Any(), gomock.Any(), gomock.Any())

	f := &filter{
		handle: handle,
		handler: func(w *Writer, r *Request) {
			w.SendLocalResponse(401, []byte(`{"error":"unauthorized"}`))
		},
	}
	status := f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), false)
	require.Equal(t, shared.HeadersStatusStop, status)
}

// --- Phase 2 flush / stale-queue correctness ---

func TestFilter_syncSetRequestHeader_applied(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{":method": "GET", ":path": "/"}),
	)
	f := &filter{
		handle:  handle,
		handler: func(w *Writer, _ *Request) { w.SetRequestHeader("x-added", "yes") },
	}
	f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, "yes", handle.RequestHeaders().GetOne("x-added").ToString())
}

func TestFilter_onRequestBodyMutation_applied(t *testing.T) {
	handle := testutil.NewFilterHandle()
	f := &filter{
		handle:  handle,
		handler: func(w *Writer, _ *Request) {},
		requestBodyHandler: func(w *Writer, _ *BodyChunk) {
			w.SetRequestHeader("x-body", "seen")
		},
	}
	f.OnRequestHeaders(handle.RequestHeaders(), false)
	f.OnRequestBody(fake.NewFakeBodyBuffer(nil), true)
	require.Equal(t, "seen", handle.RequestHeaders().GetOne("x-body").ToString())
}

func TestFilter_staleQueueNotReplayed(t *testing.T) {
	// Verify that mutations queued in one OnRequestHeaders call are not applied
	// again when a second call runs (same filter instance, different invocation).
	var call int
	handle := testutil.NewFilterHandle()
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			call++
			if call == 1 {
				w.SetRequestHeader("x-stale", "first")
			}
			// second call sets nothing
		},
	}
	f.OnRequestHeaders(handle.RequestHeaders(), false)
	require.Equal(t, "first", handle.RequestHeaders().GetOne("x-stale").ToString())
	handle.RequestHeaders().Remove("x-stale")

	f.OnRequestHeaders(handle.RequestHeaders(), false)
	require.Empty(t, handle.RequestHeaders().GetOne("x-stale").ToString())
}

// --- Framing strip ordering ---

func TestFilter_OnRequestHeaders_stripsFramingOnSyncContinue(t *testing.T) {
	// On the synchronous-continue path, content-length and transfer-encoding must
	// be stripped AFTER flush so that a handler-queued framing mutation cannot
	// survive and reach upstream before OnRequestBody writes the replacement value.
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{
			":method":           "POST",
			":path":             "/upload",
			"content-length":    "100",
			"transfer-encoding": "chunked",
		}),
	)
	f := &filter{
		handle:             handle,
		bufferBody:         true,
		mutableRequestBody: true,
		handler: func(w *Writer, _ *Request) {
			// Handler queues a stale framing value; strip must undo it.
			w.SetRequestHeader("content-length", "999")
		},
		requestBodyHandler: func(_ *Writer, _ *BodyChunk) {},
	}
	status := f.OnRequestHeaders(handle.RequestHeaders(), false)
	require.Equal(t, shared.HeadersStatusStop, status)
	// Strip happened inside flush, after the queued mutation was applied.
	require.Empty(t, handle.RequestHeaders().GetOne("content-length").ToString())
	require.Empty(t, handle.RequestHeaders().GetOne("transfer-encoding").ToString())
}

func TestFilter_flush_stripsFramingOnAsyncResume(t *testing.T) {
	// On the async-resume path (flush(true)), content-length and transfer-encoding
	// must be stripped after applying queued mutations so that ContinueRequest
	// never forwards stale framing upstream.
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{
			"content-length":    "100",
			"transfer-encoding": "chunked",
		}),
	)
	f := &filter{
		handle:               handle,
		bufferBody:           true,
		mutableRequestBody:   true,
		requestBodyHandler:   func(_ *Writer, _ *BodyChunk) {},
		stripFramingOnResume: true,
		// Pre-queue a stale content-length to verify strip runs after mutations.
		reqHeaders: []requestHeaderMutation{{name: "content-length", value: "999"}},
	}
	f.flush(true)
	require.Empty(t, handle.RequestHeaders().GetOne("content-length").ToString())
	require.Empty(t, handle.RequestHeaders().GetOne("transfer-encoding").ToString())
	require.Equal(t, 1, handle.ContinuedReq)
}

// --- Direct-write SendLocalResponse deduplication ---

func TestWriter_SendLocalResponse_directWriteIgnoresDuplicate(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	// Expect exactly one call despite two invocations.
	handle.EXPECT().SendLocalResponse(uint32(403), gomock.Any(), []byte("no"), "").Times(1)

	w := NewWriter(handle)
	w.SendLocalResponse(403, []byte("no"))
	w.SendLocalResponse(200, []byte("ok")) // must be silently ignored
}

// --- Register ---

func TestRegister_panicOnDuplicate(t *testing.T) {
	const name = "up-test-register-dup"
	h := HandlerFunc(func(w *Writer, r *Request) {})

	// Pre-populate registry directly to test up's own duplicate check without
	// involving the SDK registry (which also panics and is not reset between tests).
	registry[name] = h
	t.Cleanup(func() { delete(registry, name) })

	require.Panics(t, func() { Register(name, h) })
}

// writer is a helper for tests that need a Writer backed by this filter.
func (f *filter) writer() *Writer { return &Writer{f: f} }

// --- OnStreamComplete callback ---

func TestFilter_OnStreamComplete_invokesCallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	called := 0
	f := &filter{
		handle:           handle,
		handler:          func(w *Writer, r *Request) {},
		onStreamComplete: func(ctx *any) { called++ },
	}
	f.OnStreamComplete()
	require.Equal(t, 1, called)
}

func TestFilter_OnStreamComplete_passesContextPointer(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	type state struct{ token string }
	var seen *state
	f := &filter{
		handle: handle,
		handler: func(w *Writer, r *Request) {
			*r.Context = &state{token: "abc"}
		},
		onStreamComplete: func(ctx *any) {
			if s, ok := (*ctx).(*state); ok {
				seen = s
			}
		},
	}
	f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), false)
	f.OnStreamComplete()
	require.NotNil(t, seen)
	require.Equal(t, "abc", seen.token)
}

func TestFilter_OnStreamComplete_runsWithoutHandlers(t *testing.T) {
	// No body handler, no response handler — the callback must still fire.
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	called := false
	f := &filter{
		handle:           handle,
		handler:          func(w *Writer, r *Request) {},
		onStreamComplete: func(ctx *any) { called = true },
	}
	f.OnStreamComplete()
	require.True(t, called)
}

func TestFilter_OnStreamComplete_nilCallbackIsNoop(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	f := &filter{handle: handle, handler: func(w *Writer, r *Request) {}}
	require.NotPanics(t, func() { f.OnStreamComplete() })
}

func TestWithOnStreamComplete_setsConfigFactoryField(t *testing.T) {
	cf := &configFactory{}
	fn := func(ctx *any) {}
	WithOnStreamComplete(fn)(cf)
	require.NotNil(t, cf.onStreamComplete)
}

func TestWithOnStreamFinalized_setsConfigFactoryField(t *testing.T) {
	cf := &configFactory{}
	fn := func(_ *any, _ FinalizedInfo) {}
	WithOnStreamFinalized(fn)(cf)
	require.NotNil(t, cf.onStreamFinalized)
}

func TestFilter_OnStreamComplete_doesNotDrainFinalizedEntry(t *testing.T) {
	// OnStreamComplete must NOT drain the registry entry: in Envoy the HTTP
	// filter's OnStreamComplete fires before the access logger's OnLog at
	// AccessLogTypeDownstreamEnd, so draining here would race the SDK-internal
	// access logger and prevent OnStreamFinalized from ever firing.
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	const key = "test-finalized-noop\x00abc"
	putFinalizedEntry(key, &streamFinalizedEntry{
		fn:  func(_ *any, _ FinalizedInfo) {},
		ctx: new(any),
	})
	t.Cleanup(func() { _, _ = takeFinalizedEntry(key) })

	f := &filter{
		handle:       handle,
		handler:      func(w *Writer, r *Request) {},
		finalizedKey: key,
	}
	f.OnStreamComplete()

	_, ok := takeFinalizedEntry(key)
	require.True(t, ok, "OnStreamComplete must leave the finalized entry for the access logger")
}

func TestStreamFinalizedRegistry_takeIsOnce(t *testing.T) {
	const key = "registry-take-once\x00xyz"
	putFinalizedEntry(key, &streamFinalizedEntry{
		fn:  func(_ *any, _ FinalizedInfo) {},
		ctx: new(any),
	})
	_, ok := takeFinalizedEntry(key)
	require.True(t, ok)
	_, ok = takeFinalizedEntry(key)
	require.False(t, ok, "second take must miss")
}

func TestRegister_appliesFilterOptions(t *testing.T) {
	const name = "up-test-register-opts"
	registry[name] = HandlerFunc(func(w *Writer, r *Request) {})
	t.Cleanup(func() { delete(registry, name) })

	// Re-register via Register would panic on the duplicate; instead exercise
	// applyFilterOptions directly to verify wiring.
	cf := &configFactory{}
	applyFilterOptions(cf, []FilterOption{WithOnStreamComplete(func(ctx *any) {})})
	require.NotNil(t, cf.onStreamComplete)
}
