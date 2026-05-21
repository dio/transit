package up

import (
	"strconv"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type configFactory struct {
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
}

func (f *configFactory) Create(_ shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
	return &filterFactory{
		handler:            f.handler,
		responseHandler:    f.responseHandler,
		requestBodyHandler: f.requestBodyHandler,
		bufferBody:         f.bufferBody,
	}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

type filterFactory struct {
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &filter{
		handle:             handle,
		handler:            f.handler,
		responseHandler:    f.responseHandler,
		requestBodyHandler: f.requestBodyHandler,
		bufferBody:         f.bufferBody,
	}
}

func (f *filterFactory) OnDestroy() {}

type filter struct {
	shared.EmptyHttpFilter
	handle             shared.HttpFilterHandle
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
	stopped            bool
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
	f.handler(w, newRequest(headers))
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
		return shared.HeadersStatusContinue
	}

	// Body is coming: hold headers together with the body in buffered mode so
	// Content-Length can be updated after the body is replaced.
	if f.bufferBody && f.requestBodyHandler != nil {
		return shared.HeadersStatusStopAllAndBuffer
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
		// Headers were held by StopAllAndBuffer; update Content-Length now.
		f.handle.RequestHeaders().Remove("transfer-encoding")
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
		return shared.HeadersStatusContinue
	}

	// Hold response headers together with the body so Content-Length can be
	// updated after the body is replaced.
	if f.bufferBody {
		return shared.HeadersStatusStopAllAndBuffer
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
		// Headers were held by StopAllAndBuffer; update Content-Length now.
		f.handle.ResponseHeaders().Remove("transfer-encoding")
		f.handle.ResponseHeaders().Set("content-length", strconv.Itoa(len(w.responseBodyReplacement)))
	}
	return shared.BodyStatusContinue
}
