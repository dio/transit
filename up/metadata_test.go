package up

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/testutil"
)

// --- SetMetadata + GetMetadataString round-trip (direct mode via NewWriter) ---

func TestWriter_SetMetadata_GetMetadataString_roundtrip(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	w.SetMetadata("envoy.filters.http.ext_proc", "request_id", "abc-123")

	buf, ok := w.GetMetadataString(MetadataSourceDynamic, "envoy.filters.http.ext_proc", "request_id")
	require.True(t, ok)
	require.Equal(t, "abc-123", buf.String())
}

func TestWriter_SetMetadata_GetMetadataString_missing(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)

	_, ok := w.GetMetadataString(MetadataSourceDynamic, "ns", "missing-key")
	require.False(t, ok)
}

func TestWriter_SetMetadata_GetMetadataString_nonEmptyValue(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	w.SetMetadata("ns", "k", "hello")

	buf, ok := w.GetMetadataString(MetadataSourceDynamic, "ns", "k")
	require.True(t, ok)
	require.Equal(t, "hello", buf.String())
}

// --- SetMetadata + GetMetadataNumber round-trip ---

func TestWriter_SetMetadata_GetMetadataNumber_roundtrip(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	w.SetMetadata("my.ns", "score", float64(3.14))

	v, ok := w.GetMetadataNumber(MetadataSourceDynamic, "my.ns", "score")
	require.True(t, ok)
	require.InDelta(t, 3.14, v, 1e-9)
}

func TestWriter_SetMetadata_GetMetadataNumber_missing(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)

	v, ok := w.GetMetadataNumber(MetadataSourceDynamic, "ns", "no-key")
	require.False(t, ok)
	require.Zero(t, v)
}

func TestWriter_SetMetadata_GetMetadataNumber_wrongType(t *testing.T) {
	// Storing a string under a key and then reading it as a number should fail.
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	w.SetMetadata("ns", "k", "not-a-number")

	_, ok := w.GetMetadataNumber(MetadataSourceDynamic, "ns", "k")
	require.False(t, ok)
}

// --- SetMetadata + GetMetadataBool round-trip ---

func TestWriter_SetMetadata_GetMetadataBool_roundtrip_true(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	w.SetMetadata("auth.ns", "authenticated", true)

	v, ok := w.GetMetadataBool(MetadataSourceDynamic, "auth.ns", "authenticated")
	require.True(t, ok)
	require.True(t, v)
}

func TestWriter_SetMetadata_GetMetadataBool_roundtrip_false(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	w.SetMetadata("auth.ns", "authenticated", false)

	v, ok := w.GetMetadataBool(MetadataSourceDynamic, "auth.ns", "authenticated")
	require.True(t, ok)
	require.False(t, v)
}

func TestWriter_SetMetadata_GetMetadataBool_missing(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)

	v, ok := w.GetMetadataBool(MetadataSourceDynamic, "ns", "absent")
	require.False(t, ok)
	require.False(t, v)
}

func TestWriter_SetMetadata_GetMetadataBool_wrongType(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	w.SetMetadata("ns", "k", float64(1))

	_, ok := w.GetMetadataBool(MetadataSourceDynamic, "ns", "k")
	require.False(t, ok)
}

// --- SetMetadata queued mode: via filter.OnRequestHeaders lifecycle ---

// TestFilter_SetMetadata_queuedMode verifies that SetMetadata called from a
// request handler is batched and applied during flush(), not at call time.
func TestFilter_SetMetadata_queuedMode(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{":method": "GET", ":path": "/"}),
	)

	handlerCalled := false
	f := &filter{
		handle: handle,
		handler: func(w *Writer, r *Request) {
			handlerCalled = true
			// In queued mode, SetMetadata must be deferred until flush().
			w.SetMetadata("test.ns", "key", "value-from-handler")
		},
	}
	f.OnRequestHeaders(handle.RequestHeaders(), true)

	require.True(t, handlerCalled)
	// After OnRequestHeaders returns, flush() has been called and the metadata
	// must be present in the handle.
	buf, ok := handle.Metadata("test.ns", "key")
	require.True(t, ok, "metadata must be stored after flush")
	require.Equal(t, "value-from-handler", buf)
}

// TestFilter_SetMetadata_queuedMode_multipleValues verifies that multiple
// SetMetadata calls are all applied by flush().
func TestFilter_SetMetadata_queuedMode_multipleValues(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{":method": "GET", ":path": "/"}),
	)

	f := &filter{
		handle: handle,
		handler: func(w *Writer, r *Request) {
			w.SetMetadata("ns", "a", "alpha")
			w.SetMetadata("ns", "b", float64(42))
			w.SetMetadata("ns", "c", true)
		},
	}
	f.OnRequestHeaders(handle.RequestHeaders(), true)

	v, ok := handle.Metadata("ns", "a")
	require.True(t, ok)
	require.Equal(t, "alpha", v)

	n, ok := handle.MetadataNumber("ns", "b")
	require.True(t, ok)
	require.InDelta(t, 42.0, n, 1e-9)

	b, ok := handle.MetadataBool("ns", "c")
	require.True(t, ok)
	require.True(t, b)
}

// TestFilter_SetMetadata_queueNotReplayed verifies that metadata mutations
// queued in one handler call do not replay in a subsequent call.
func TestFilter_SetMetadata_queueNotReplayed(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{":method": "GET", ":path": "/"}),
	)

	call := 0
	f := &filter{
		handle: handle,
		handler: func(w *Writer, r *Request) {
			call++
			if call == 1 {
				w.SetMetadata("ns", "once", "first")
			}
			// second call sets nothing
		},
	}

	f.OnRequestHeaders(handle.RequestHeaders(), false)
	// Manually clear so we can verify the second call does not re-apply.
	handle.ClearMetadata("ns", "once")

	f.OnRequestHeaders(handle.RequestHeaders(), false)
	_, ok := handle.Metadata("ns", "once")
	require.False(t, ok, "metadata set in call 1 must not replay in call 2")
}

// --- MetadataSource constants ---

func TestMetadataSource_constants(t *testing.T) {
	// Verify constants are distinct and non-zero (Dynamic is 0 by upstream definition).
	seen := map[MetadataSource]bool{}
	for _, s := range []MetadataSource{
		MetadataSourceDynamic,
		MetadataSourceRoute,
		MetadataSourceCluster,
		MetadataSourceHost,
		MetadataSourceHostLocality,
	} {
		require.False(t, seen[s], "duplicate MetadataSource constant value %d", s)
		seen[s] = true
	}
}
