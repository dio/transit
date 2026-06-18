package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("e2e-openai-probe", func(w *up.Writer, _ *up.Request) {
		w.SetRequestHeader("x-transit-openai-probe", "1")
	})
}
