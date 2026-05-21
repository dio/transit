package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("echo", func(w *up.Writer, r *up.Request) {
		w.Log(up.LogInfo, "echo: %s %s", r.Method, r.Path)
	})
}
