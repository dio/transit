package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// Request holds the per-request fields populated before the handler is called.
type Request struct {
	Method     string
	Path       string
	Host       string
	FilterName string
	Context    *any
	headers    shared.HeaderMap
}

// AllHeaders returns all request headers as copied Go strings.
func (r *Request) AllHeaders() [][2]string {
	if r.headers == nil {
		return nil
	}
	raw := r.headers.GetAll()
	out := make([][2]string, len(raw))
	for i, h := range raw {
		out[i] = [2]string{h[0].ToString(), h[1].ToString()}
	}
	return out
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

func newRequestWithContext(headers shared.HeaderMap, name string, ctx *any) *Request {
	r := &Request{headers: headers, FilterName: name, Context: ctx}
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

func newRequest(headers shared.HeaderMap, name string) *Request {
	return newRequestWithContext(headers, name, nil)
}

// NewRequest constructs a Request from a HeaderMap for use in tests.
func NewRequest(headers shared.HeaderMap, name string) *Request {
	return newRequest(headers, name)
}
