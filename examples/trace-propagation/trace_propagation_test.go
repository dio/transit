package tracepropagation

import (
	"net/http"
	"testing"
)

func TestCopyTraceHeaders_copiesAll(t *testing.T) {
	src := http.Header{
		"Traceparent":  []string{"00-aabbccddee-ffgg-01"},
		"Tracestate":   []string{"vendor=value"},
		"X-Request-Id": []string{"req-123"},
	}
	dst := http.Header{}
	CopyTraceHeaders(dst, src)

	for _, h := range []string{"traceparent", "tracestate", "x-request-id"} {
		if dst.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}

func TestCopyTraceHeaders_skipsEmpty(t *testing.T) {
	src := http.Header{}
	dst := http.Header{}
	CopyTraceHeaders(dst, src)
	if len(dst) != 0 {
		t.Errorf("expected no headers, got %v", dst)
	}
}

func TestCopyTraceHeaders_doesNotCopyOtherHeaders(t *testing.T) {
	src := http.Header{
		"X-Custom-Header": []string{"should-not-copy"},
		"Traceparent":     []string{"00-abc-def-01"},
	}
	dst := http.Header{}
	CopyTraceHeaders(dst, src)
	if dst.Get("x-custom-header") != "" {
		t.Error("should not have copied x-custom-header")
	}
	if dst.Get("traceparent") == "" {
		t.Error("should have copied traceparent")
	}
}

func TestCopyTraceHeaders_traceparentValue(t *testing.T) {
	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	src := http.Header{"Traceparent": []string{want}}
	dst := http.Header{}
	CopyTraceHeaders(dst, src)
	if got := dst.Get("traceparent"); got != want {
		t.Errorf("traceparent: want %q, got %q", want, got)
	}
}
