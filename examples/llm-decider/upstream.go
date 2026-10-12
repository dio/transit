package llmdecider

import "github.com/dio/transit/up"

// upstreamHandler is the upstream HTTP filter surface. It runs after host
// selection, with the decision already made.
//
// This is where per-model shaping belongs: the decided model may live on a
// different provider than the original request implied, so credentials and
// endpoint hints are chosen here rather than before routing.
func upstreamHandler(w *up.Writer, r *up.Request) {
	model := r.Header(HeaderDecidedModel)
	if model == "" {
		return
	}
	w.SetRequestHeader("x-llm-target-model", model)
	if source := r.Header(HeaderDecisionSource); source != "" {
		w.SetRequestHeader("x-llm-decision-source", source)
	}
}
