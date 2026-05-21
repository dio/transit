package hello

import "github.com/dio/transit/up"

func Handler(w *up.Writer, r *up.Request) {
	w.Log(up.LogWarn, "hello: %s %s", r.Method, r.Path)
}
