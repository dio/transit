package up

import (
	"net/http"
	"strings"
)

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
	prefix  bool
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

// HandlePrefix registers handler for the given HTTP method and any path that
// starts with the given prefix. Exact routes are checked first; prefix routes
// are checked in registration order among themselves.
func (r *Router) HandlePrefix(method, pathPrefix string, handler func(*Writer, *Request)) *Router {
	r.routes = append(r.routes, routeEntry{method: method, path: pathPrefix, prefix: true, handler: handler})
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

// DELETE registers handler for DELETE requests to the exact path.
func (r *Router) DELETE(path string, handler func(*Writer, *Request)) *Router {
	return r.Handle(http.MethodDelete, path, handler)
}

// GETPrefix registers handler for GET requests to any path with the given prefix.
func (r *Router) GETPrefix(prefix string, handler func(*Writer, *Request)) *Router {
	return r.HandlePrefix(http.MethodGet, prefix, handler)
}

// POSTPrefix registers handler for POST requests to any path with the given prefix.
func (r *Router) POSTPrefix(prefix string, handler func(*Writer, *Request)) *Router {
	return r.HandlePrefix(http.MethodPost, prefix, handler)
}

// DELETEPrefix registers handler for DELETE requests to any path with the given prefix.
func (r *Router) DELETEPrefix(prefix string, handler func(*Writer, *Request)) *Router {
	return r.HandlePrefix(http.MethodDelete, prefix, handler)
}

// pathHasPrefix reports whether path equals prefix or starts with prefix+"/",
// preventing bare-word collisions like "/mcp" matching "/mcp-other".
func pathHasPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	return rest == "" || rest[0] == '/'
}

// Dispatch is the request handler func for [Register]. Exact routes take
// priority over prefix routes; among each group, first registration wins.
func (r *Router) Dispatch(w *Writer, req *Request) {
	// Strip the query string before matching so that paths like
	// /v1/messages?beta=true route the same as /v1/messages.
	path, _, _ := strings.Cut(req.Path, "?")
	var prefixMatch *routeEntry
	for i := range r.routes {
		rt := &r.routes[i]
		if req.Method != rt.method {
			continue
		}
		if !rt.prefix {
			if path == rt.path {
				rt.handler(w, req)
				return
			}
		} else if prefixMatch == nil && pathHasPrefix(path, rt.path) {
			prefixMatch = rt
		}
	}
	if prefixMatch != nil {
		prefixMatch.handler(w, req)
		return
	}
	if r.notFound != nil {
		r.notFound(w, req)
	}
}
