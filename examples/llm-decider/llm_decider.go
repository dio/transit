// Package llmdecider routes a request to a model tier by asking a judge model.
//
// It is the transit side of the decision seam. The routing core comes from
// github.com/dio/libsygo, which performs no I/O of its own;
// this package supplies the judge call over Envoy's own egress and applies the
// result. Egress, credentials and retries stay with Envoy.
//
// One module, five entry points — every surface a single dynamic module can
// register at once:
//
//	llm-decider            downstream HTTP filter. Parses the prompt, asks the
//	                       judge, and stamps the decision onto the request.
//	llm-decider-upstream   upstream HTTP filter. Turns the decided model into
//	                       upstream request shaping.
//	llm-decider-cluster    cluster extension. Owns the host set and selects the
//	                       host serving the decided model.
//	llm-decider-lb         LB policy. Chooses a host within a cluster Envoy owns.
//	llm-decider-accesslog  access logger. Emits the decision.
//
// The decision travels between them on request headers, so every surface reads
// the same facts the downstream filter recorded.
package llmdecider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/dio/libsygo"

	"github.com/dio/transit/examples/llm-decider/internal/config"
	"github.com/dio/transit/up"
)

// Registration names. The dynamic module config's filter_name / cluster_name must
// match these exactly.
const (
	FilterName         = "llm-decider"
	UpstreamFilterName = "llm-decider-upstream"
	ClusterName        = "llm-decider-cluster"
	LBPolicyName       = "llm-decider-lb"
	AccessLoggerName   = "llm-decider-accesslog"
)

// Headers that carry a decision from the filter that made it to the surfaces that
// consume it.
const (
	// HeaderDecidedModel is the model id the router selected.
	HeaderDecidedModel = "x-llm-decided-model"
	// HeaderDecisionSource explains the decision: decision_classifier, retained,
	// or fail_open.
	HeaderDecisionSource = "x-llm-decision-source"
	// HeaderDecisionError is set when the turn failed rather than mis-routed.
	HeaderDecisionError = "x-llm-decision-error"
	// HeaderSession keys the per-session latch. It must be tenant-namespaced.
	HeaderSession = "x-switchyard-session-id"
)

// Config is this module's runtime configuration.
type Config = config.Config

// ConfigFromEnv reads configuration from the process environment. Every value
// has a default, so the example runs with no environment at all.
func ConfigFromEnv() (Config, error) {
	return config.FromEnv()
}

// Register wires every entry point. Call it from init(), because Envoy loads the
// shared library and never calls main().
func Register(cfg Config) error {
	router, err := cfg.Router()
	if err != nil {
		return err
	}

	up.Register(FilterName, sessionHandler, up.WithBody(decideHandler(router, cfg)))
	up.Register(UpstreamFilterName, upstreamHandler)
	up.RegisterCluster(ClusterName, &modelClusterFactory{})
	up.RegisterLBPolicy(LBPolicyName, &firstHealthyFactory{})
	up.RegisterAccessLogger(AccessLoggerName, &decisionAccessLoggerFactory{})

	log.Printf("llm-decider: registered %s, %s, %s, %s, %s",
		FilterName, UpstreamFilterName, ClusterName, LBPolicyName, AccessLoggerName)
	return nil
}

// sessionHandler records the session key for the stream. The latch is keyed by it
// and the body handler runs later, so it has to be carried across.
//
// Request.Context is the per-stream slot: it is the same value the body handler
// sees as BodyChunk.Context.
func sessionHandler(_ *up.Writer, r *up.Request) {
	if r.Context == nil {
		return
	}
	*r.Context = r.Header(HeaderSession)
}

// decideHandler returns the body handler that makes the decision.
//
// It runs in a goroutine because the judge is a network call — 70-500 ms for
// System One — and blocking the Envoy worker on it would stall the stream. That
// goroutine is also where Writer.Do is legal.
func decideHandler(router *libsygo.Router, cfg Config) up.RequestBodyHandlerFunc {
	return func(w *up.Writer, chunk *up.BodyChunk) {
		// Buffered mode: one call, with the whole body, at end of stream.
		if !chunk.EndStream {
			return
		}

		prompt, err := promptFromBody(chunk.Data)
		if err != nil {
			w.SetRequestHeader(HeaderDecisionError, err.Error())
			return
		}

		session := ""
		if chunk.Context != nil {
			if value, ok := (*chunk.Context).(string); ok {
				session = value
			}
		}

		w.Go(func(ctx context.Context) {
			outcome := router.Route(ctx,
				libsygo.Turn{Prompt: prompt, Session: session},
				judgeDecider{w: w, config: cfg})
			applyOutcome(w, outcome)
		})
	}
}

// applyOutcome records the decision on the request.
//
// It cannot answer from here: SendLocalResponse is only honoured from a filter
// callback, not from a scheduled goroutine. A failed decision is therefore
// recorded as a header, which the route and the access log can act on.
func applyOutcome(w *up.Writer, outcome libsygo.Outcome) {
	if outcome.Error != "" {
		w.SetRequestHeader(HeaderDecisionError, outcome.Error)
		return
	}
	w.SetRequestHeader(HeaderDecidedModel, outcome.Model)
	if source := decisionSource(outcome); source != "" {
		w.SetRequestHeader(HeaderDecisionSource, source)
	}
}

// decisionSource names the rule that produced the outcome, from the evidence the
// router records.
func decisionSource(outcome libsygo.Outcome) string {
	var evidence struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(outcome.Evidence, &evidence); err == nil && evidence.Source != "" {
		return evidence.Source
	}
	if outcome.Latched {
		return "retained"
	}
	return ""
}

// promptFromBody extracts the prompt the judge evaluates.
//
// The body shape is deliberately minimal: a real deployment would take the
// normalized request from its own translation layer rather than parsing here.
func promptFromBody(body []byte) (string, error) {
	if len(body) == 0 {
		return "", errors.New("request has no body to decide on")
	}
	var parsed struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parsing request body: %w", err)
	}
	if parsed.Prompt == "" {
		return "", errors.New(`request body must carry a non-empty "prompt"`)
	}
	return parsed.Prompt, nil
}
