package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// Request holds the per-request fields populated before the handler is called.
type Request struct {
	Method  string
	Path    string
	Host    string
	headers shared.HeaderMap
}

// Header returns the first value of the named request header, or "" if absent.
func (r *Request) Header(name string) string {
	if r.headers == nil {
		return ""
	}
	v := r.headers.GetOne(name)
	if v.Len == 0 {
		return ""
	}
	return v.ToString()
}

func newRequest(headers shared.HeaderMap) *Request {
	r := &Request{headers: headers}
	if v := headers.GetOne(":method"); v.Len > 0 {
		r.Method = v.ToString()
	}
	if v := headers.GetOne(":path"); v.Len > 0 {
		r.Path = v.ToString()
	}
	if v := headers.GetOne(":authority"); v.Len > 0 {
		r.Host = v.ToString()
	}
	return r
}
