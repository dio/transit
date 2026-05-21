package up

import (
	"strconv"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type configFactory struct {
	handler         HandlerFunc
	responseHandler ResponseHandlerFunc
}

func (f *configFactory) Create(_ shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
	return &filterFactory{handler: f.handler, responseHandler: f.responseHandler}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

type filterFactory struct {
	handler         HandlerFunc
	responseHandler ResponseHandlerFunc
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &filter{handle: handle, handler: f.handler, responseHandler: f.responseHandler}
}

func (f *filterFactory) OnDestroy() {}

type filter struct {
	shared.EmptyHttpFilter
	handle          shared.HttpFilterHandle
	handler         HandlerFunc
	responseHandler ResponseHandlerFunc
	stopped         bool
	context         any
}

func (f *filter) OnRequestHeaders(headers shared.HeaderMap, _ bool) shared.HeadersStatus {
	w := &Writer{handle: f.handle}
	f.handler(w, newRequest(headers))
	if w.stopped {
		return shared.HeadersStatusStop
	}
	return shared.HeadersStatusContinue
}

func (f *filter) OnResponseHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if f.responseHandler == nil {
		return shared.HeadersStatusContinue
	}
	statusCode := 0
	if v := headers.GetOne(":status"); v.Len > 0 {
		statusCode, _ = strconv.Atoi(v.ToString())
	}
	f.responseHandler(&Writer{handle: f.handle}, &ResponseChunk{
		StatusCode: statusCode,
		Headers:    headers,
		EndStream:  endOfStream,
		Context:    &f.context,
	})
	return shared.HeadersStatusContinue
}

func (f *filter) OnResponseBody(body shared.BodyBuffer, endOfStream bool) shared.BodyStatus {
	if f.responseHandler == nil {
		return shared.BodyStatusContinue
	}
	var data []byte
	for _, chunk := range body.GetChunks() {
		data = append(data, chunk.ToBytes()...)
	}
	f.responseHandler(&Writer{handle: f.handle}, &ResponseChunk{
		Data:      data,
		EndStream: endOfStream,
		Context:   &f.context,
	})
	return shared.BodyStatusContinue
}
