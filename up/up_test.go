package up

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/mocks"
)

// --- newRequest ---

func TestNewRequest_allHeaders(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		":method":    {"GET"},
		":path":      {"/test"},
		":authority": {"example.com"},
	})
	r := newRequest(headers)
	require.Equal(t, "GET", r.Method)
	require.Equal(t, "/test", r.Path)
	require.Equal(t, "example.com", r.Host)
}

func TestNewRequest_missingHeaders(t *testing.T) {
	r := newRequest(fake.NewFakeHeaderMap(nil))
	require.Empty(t, r.Method)
	require.Empty(t, r.Path)
	require.Empty(t, r.Host)
}

func TestNewRequest_partialHeaders(t *testing.T) {
	headers := fake.NewFakeHeaderMap(map[string][]string{
		":method": {"POST"},
	})
	r := newRequest(headers)
	require.Equal(t, "POST", r.Method)
	require.Empty(t, r.Path)
	require.Empty(t, r.Host)
}

func TestNewRequest_variousMethods(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
		r := newRequest(fake.NewFakeHeaderMap(map[string][]string{":method": {method}}))
		require.Equal(t, method, r.Method)
	}
}

// --- Writer ---

func TestWriter_Log_delegatesToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelError, "msg: %s", "arg")

	w := &Writer{handle: handle}
	w.Log(shared.LogLevelError, "msg: %s", "arg")
}

func TestWriter_Log_allLevels(t *testing.T) {
	levels := []shared.LogLevel{
		shared.LogLevelTrace, shared.LogLevelDebug, shared.LogLevelInfo,
		shared.LogLevelWarn, shared.LogLevelError, shared.LogLevelCritical,
	}
	for _, level := range levels {
		ctrl := gomock.NewController(t)
		handle := mocks.NewMockHttpFilterHandle(ctrl)
		handle.EXPECT().Log(level, "test", gomock.Any())
		NewWriter(handle).Log(level, "test")
	}
}

func TestNewWriter_returnsWriterBackedByHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelInfo, "hi")

	w := NewWriter(handle)
	require.NotNil(t, w)
	w.Log(shared.LogLevelInfo, "hi")
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
	handle := mocks.NewMockHttpFilterHandle(ctrl)

	ff := &filterFactory{handler: func(w *Writer, r *Request) {}}
	f := ff.Create(handle)
	require.NotNil(t, f)
	_, ok := f.(*filter)
	require.True(t, ok)
}

func TestFilterFactory_Create_wiredToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelWarn, "check")

	ff := &filterFactory{handler: func(w *Writer, r *Request) {
		w.Log(shared.LogLevelWarn, "check")
	}}
	f := ff.Create(handle).(*filter)
	f.handler(f.writer(), nil)
}

// --- filter.OnRequestHeaders ---

func TestFilter_OnRequestHeaders_returnsStatusContinue(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)

	f := &filter{handle: handle, handler: func(w *Writer, r *Request) {}}
	status := f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), false)
	require.Equal(t, shared.HeadersStatusContinue, status)
}

func TestFilter_OnRequestHeaders_passesRequestFields(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)

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
	handle := mocks.NewMockHttpFilterHandle(ctrl)
	handle.EXPECT().Log(shared.LogLevelInfo, "test: %d", 42)

	f := &filter{
		handle:  handle,
		handler: func(w *Writer, r *Request) { w.Log(shared.LogLevelInfo, "test: %d", 42) },
	}
	f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), false)
}

func TestFilter_OnRequestHeaders_endOfStreamFlagIgnored(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)

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
	r := newRequest(headers)
	require.Equal(t, "secret", r.Header("x-api-key"))
}

func TestRequest_Header_missing(t *testing.T) {
	r := newRequest(fake.NewFakeHeaderMap(nil))
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
	r := newRequest(headers)
	require.Equal(t, "abc-123", r.Header("x-request-id"))
}

// --- Writer.SendLocalResponse ---

func TestWriter_SendLocalResponse_delegatesToHandle(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)
	handle.EXPECT().SendLocalResponse(uint32(401), gomock.Any(), []byte(`{"error":"no key"}`), "")

	w := &Writer{handle: handle}
	w.SendLocalResponse(401, []byte(`{"error":"no key"}`))
	require.True(t, w.stopped)
}

func TestWriter_SendLocalResponse_setsStoppedFlag(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)
	handle.EXPECT().SendLocalResponse(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any())

	w := NewWriter(handle)
	require.False(t, w.stopped)
	w.SendLocalResponse(200, nil)
	require.True(t, w.stopped)
}

func TestFilter_OnRequestHeaders_stopsAfterLocalResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := mocks.NewMockHttpFilterHandle(ctrl)
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

// writer is a helper for tests that need a Writer backed by filter.handle.
func (f *filter) writer() *Writer { return &Writer{handle: f.handle} }
