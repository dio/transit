package up

import (
	"fmt"
	"strconv"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type configFactory struct {
	name               string
	configFn           ConfigFunc
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
	group              *Group
}

// configHandleImpl wraps shared.HttpFilterConfigHandle to implement ConfigHandle.
type configHandleImpl struct{ h shared.HttpFilterConfigHandle }

func (c *configHandleImpl) DefineCounter(name string, tagKeys ...string) (MetricID, error) {
	id, res := c.h.DefineCounter(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineCounter %q failed (result=%d)", name, res)
	}
	return id, nil
}

func (f *configFactory) Create(h shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
	if f.configFn != nil {
		if err := f.configFn(&configHandleImpl{h: h}); err != nil {
			return nil, err
		}
	}
	var stop func()
	if f.group != nil {
		f.group.Start()
		stop = f.group.Stop
	}
	return &filterFactory{
		name:               f.name,
		handler:            f.handler,
		responseHandler:    f.responseHandler,
		requestBodyHandler: f.requestBodyHandler,
		bufferBody:         f.bufferBody,
		stop:               stop,
	}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

type filterFactory struct {
	name               string
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
	stop               func()
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &filter{
		name:               f.name,
		handle:             handle,
		handler:            f.handler,
		responseHandler:    f.responseHandler,
		requestBodyHandler: f.requestBodyHandler,
		bufferBody:         f.bufferBody,
	}
}

func (f *filterFactory) OnDestroy() {
	if f.stop != nil {
		f.stop()
	}
}

type filter struct {
	shared.EmptyHttpFilter
	name               string
	handle             shared.HttpFilterHandle
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
	context            any

	// captured from headers callbacks for use in body callbacks
	requestContentType      string
	requestContentEncoding  string
	responseContentType     string
	responseContentEncoding string
}

func (f *filter) OnRequestHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if f.requestBodyHandler != nil {
		f.requestContentType = headers.GetOne("content-type").ToString()
		f.requestContentEncoding = headers.GetOne("content-encoding").ToString()
	}

	w := &Writer{handle: f.handle}
	f.handler(w, newRequestWithContext(headers, f.name, &f.context))
	if w.stopped {
		return shared.HeadersStatusStop
	}

	if endOfStream {
		// Synthetic body call for bodyless requests (GET, DELETE, etc.).
		if f.requestBodyHandler != nil {
			f.requestBodyHandler(w, &BodyChunk{
				EndStream:       true,
				ContentType:     f.requestContentType,
				ContentEncoding: f.requestContentEncoding,
				Context:         &f.context,
			})
		}
		if w.stopped {
			return shared.HeadersStatusStop
		}
		return shared.HeadersStatusContinue
	}

	// Body is coming in buffered mode. Strip content-length and transfer-encoding
	// now so upstream never sees a stale value after SetRequestBody replaces the
	// body. Body chunks are accumulated via StopAndBuffer in OnRequestBody; the
	// correct content-length is written there after any replacement.
	//
	// NOTE: HeadersStatusStopAllAndBuffer is intentionally NOT returned here.
	// Returning it from OnRequestHeaders freezes the filter chain permanently —
	// Envoy buffers body data but never calls OnRequestBody because the SDK
	// has no asynchronous resume path.
	if f.bufferBody && f.requestBodyHandler != nil {
		headers.Remove("content-length")
		headers.Remove("transfer-encoding")
	}
	return shared.HeadersStatusContinue
}

func (f *filter) OnRequestBody(body shared.BodyBuffer, endOfStream bool) shared.BodyStatus {
	if f.requestBodyHandler == nil {
		return shared.BodyStatusContinue
	}
	if f.bufferBody && !endOfStream {
		return shared.BodyStatusStopAndBuffer
	}

	var data []byte
	src := body
	if f.bufferBody {
		src = f.handle.BufferedRequestBody()
	}
	for _, chunk := range src.GetChunks() {
		data = append(data, chunk.ToBytes()...)
	}

	w := &Writer{handle: f.handle}
	f.requestBodyHandler(w, &BodyChunk{
		Data:            data,
		EndStream:       endOfStream,
		ContentType:     f.requestContentType,
		ContentEncoding: f.requestContentEncoding,
		Context:         &f.context,
	})

	if f.bufferBody && w.hasRequestBodyReplacement {
		buf := f.handle.BufferedRequestBody()
		buf.Drain(buf.GetSize())
		buf.Append(w.requestBodyReplacement)
		// content-length was cleared in OnRequestHeaders; set the correct value now.
		f.handle.RequestHeaders().Set("content-length", strconv.Itoa(len(w.requestBodyReplacement)))
	}
	return shared.BodyStatusContinue
}

func (f *filter) OnResponseHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if f.responseHandler == nil {
		return shared.HeadersStatusContinue
	}

	f.responseContentType = headers.GetOne("content-type").ToString()
	f.responseContentEncoding = headers.GetOne("content-encoding").ToString()

	// Strip content-length upfront in buffered mode so downstream never sees a
	// stale value after SetResponseBody replaces the body. The correct value is
	// written in OnResponseBody after any replacement.
	//
	// NOTE: HeadersStatusStopAllAndBuffer is intentionally NOT returned here
	// for the same reason as OnRequestHeaders: it would freeze the filter chain.
	if f.bufferBody {
		headers.Remove("content-length")
	}

	statusCode := 0
	if v := headers.GetOne(":status"); v.Len > 0 {
		statusCode, _ = strconv.Atoi(v.ToString())
	}

	w := &Writer{handle: f.handle}
	f.responseHandler(w, &ResponseChunk{
		StatusCode:      statusCode,
		Headers:         headers,
		EndStream:       endOfStream,
		ContentEncoding: f.responseContentEncoding,
		ContentType:     f.responseContentType,
		Context:         &f.context,
	})

	if endOfStream {
		// Synthetic body call for bodyless responses (204, HEAD, etc.).
		f.responseHandler(w, &ResponseChunk{
			EndStream:       true,
			ContentEncoding: f.responseContentEncoding,
			ContentType:     f.responseContentType,
			Context:         &f.context,
		})
	}
	return shared.HeadersStatusContinue
}

func (f *filter) OnResponseBody(body shared.BodyBuffer, endOfStream bool) shared.BodyStatus {
	if f.responseHandler == nil {
		return shared.BodyStatusContinue
	}
	if f.bufferBody && !endOfStream {
		return shared.BodyStatusStopAndBuffer
	}

	var data []byte
	src := body
	if f.bufferBody {
		src = f.handle.BufferedResponseBody()
	}
	for _, chunk := range src.GetChunks() {
		data = append(data, chunk.ToBytes()...)
	}

	w := &Writer{handle: f.handle}
	f.responseHandler(w, &ResponseChunk{
		Data:            data,
		EndStream:       endOfStream,
		ContentEncoding: f.responseContentEncoding,
		ContentType:     f.responseContentType,
		Context:         &f.context,
	})

	if f.bufferBody && w.hasResponseBodyReplacement {
		buf := f.handle.BufferedResponseBody()
		buf.Drain(buf.GetSize())
		buf.Append(w.responseBodyReplacement)
		// content-length was cleared in OnResponseHeaders; set the correct value now.
		f.handle.ResponseHeaders().Set("content-length", strconv.Itoa(len(w.responseBodyReplacement)))
	}
	return shared.BodyStatusContinue
}
