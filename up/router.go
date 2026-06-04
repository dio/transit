package up

import "net/http"

// Router dispatches a request to a handler matched by HTTP method and exact
// path. Unmatched requests are forwarded to the notFound func; if notFound is
// nil, they are silently passed through.
type Router struct {
	routes   []routeEntry
	notFound func(*Writer, *Request)
}

type routeEntry struct {
	method  string
	path    string
	handler func(*Writer, *Request)
}

// NewRouter returns a Router that calls notFound for unmatched requests.
func NewRouter(notFound func(*Writer, *Request)) *Router {
	return &Router{notFound: notFound}
}

// Handle registers handler for the given HTTP method and exact path.
// Returns r for chaining.
func (r *Router) Handle(method, path string, handler func(*Writer, *Request)) *Router {
	r.routes = append(r.routes, routeEntry{method: method, path: path, handler: handler})
	return r
}

// GET registers handler for GET requests to the exact path.
func (r *Router) GET(path string, handler func(*Writer, *Request)) *Router {
	return r.Handle(http.MethodGet, path, handler)
}

// POST registers handler for POST requests to the exact path.
func (r *Router) POST(path string, handler func(*Writer, *Request)) *Router {
	return r.Handle(http.MethodPost, path, handler)
}

// Dispatch is the request handler func for [Register].
func (r *Router) Dispatch(w *Writer, req *Request) {
	for _, rt := range r.routes {
		if req.Method == rt.method && req.Path == rt.path {
			rt.handler(w, req)
			return
		}
	}
	if r.notFound != nil {
		r.notFound(w, req)
	}
}
