package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

type configFactory struct{ handler HandlerFunc }

func (f *configFactory) Create(h shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
	return &filterFactory{handler: f.handler}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

type filterFactory struct{ handler HandlerFunc }

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &filter{handle: handle, handler: f.handler}
}

func (f *filterFactory) OnDestroy() {}

type filter struct {
	shared.EmptyHttpFilter
	handle  shared.HttpFilterHandle
	handler HandlerFunc
}

func (f *filter) OnRequestHeaders(headers shared.HeaderMap, _ bool) shared.HeadersStatus {
	w := &Writer{handle: f.handle}
	f.handler(w, newRequest(headers))
	if w.stopped {
		return shared.HeadersStatusStop
	}
	return shared.HeadersStatusContinue
}
