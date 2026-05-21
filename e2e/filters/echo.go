// echo is a pass-through filter that logs the request method and path at INFO
// level. Used by EchoSuite to verify that requests reach the filter and that
// non-stopping handlers do not interfere with the response.
package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("echo", func(w *up.Writer, r *up.Request) {
		w.Log(up.LogInfo, "echo: %s %s", r.Method, r.Path)
	})
}
