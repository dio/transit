// Package config holds the llm-decider module's runtime configuration.
package config

import (
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/dio/switchyard-capi/go/decision"
)

// Config is the module's runtime configuration. Every field has a default, so
// the example runs with no environment at all; the e2e overrides them.
type Config struct {
	// JudgeCluster is the Envoy cluster carrying the judge endpoint. Credentials
	// and TLS for it are Envoy's, via the cluster, not this module's.
	JudgeCluster string
	// JudgePath is the judge endpoint path, e.g. /v1/systemone.
	JudgePath string
	// JudgeHost is the host header to present to the judge.
	JudgeHost string
	// JudgeTimeout bounds the judge call; it is part of the request's budget.
	JudgeTimeout time.Duration

	// Cutoff routes to the capable tier only when the advantage score is
	// strictly greater.
	Cutoff float64
	// CapableModel and EfficientModel are the model ids the router chooses
	// between. They are ids the rest of the dataplane already understands.
	CapableModel   string
	EfficientModel string
	// JudgeModel is the judge model id sent to the judge endpoint.
	JudgeModel string

	// FailOpen routes an unusable verdict to the capable tier instead of failing
	// the request.
	FailOpen bool
}

// FromEnv reads configuration from the process environment.
func FromEnv() (Config, error) {
	cfg := Config{
		JudgeCluster:   env("LLM_DECIDER_JUDGE_CLUSTER", "llm-decider-judge"),
		JudgePath:      env("LLM_DECIDER_JUDGE_PATH", "/v1/systemone"),
		JudgeHost:      env("LLM_DECIDER_JUDGE_HOST", "judge.local"),
		JudgeTimeout:   time.Duration(envInt("LLM_DECIDER_JUDGE_TIMEOUT_MS", 5000)) * time.Millisecond,
		Cutoff:         envFloat("LLM_DECIDER_CUTOFF", 0.4),
		CapableModel:   env("LLM_DECIDER_CAPABLE_MODEL", "smart"),
		EfficientModel: env("LLM_DECIDER_EFFICIENT_MODEL", "cheap"),
		JudgeModel:     env("LLM_DECIDER_JUDGE_MODEL", "jev-latest"),
		FailOpen:       env("LLM_DECIDER_FAIL_OPEN", "true") != "false",
	}

	// The candidate labels and evidence are what let the judge reason about the
	// models without brand names. A materializer would author these; the example
	// derives them from the two tiers.
	_ = json.RawMessage(nil)
	return cfg, nil
}

// Profile builds the routing profile described by this configuration.
func (c Config) Profile() decision.Profile {
	failOpen := c.FailOpen
	return decision.Profile{
		ID:              FilterID,
		Algorithm:       "llm_classifier",
		Mode:            "capability",
		FailOpen:        &failOpen,
		SessionAffinity: true,
		Judge: &decision.Judge{
			Type:   "decision",
			Cutoff: c.Cutoff,
			Candidates: map[string]string{
				"a": c.CapableModel,
				"b": c.EfficientModel,
			},
			Evidence: json.RawMessage(`{"candidates":{` +
				`"a":"Capable tier: used when the efficient tier is likely to fail the task",` +
				`"b":"Efficient tier: used when it is likely to succeed"}}`),
		},
	}
}

// Models builds the routable pool for this configuration.
//
// "any" must list every routable target: the selected target is resolved against
// it, and the order is the fallback order. It mirrors the reference runner's
// capability pool, [efficient, capable].
func (c Config) Models() decision.Models {
	return decision.Models{
		"capable":   {c.CapableModel},
		"efficient": {c.EfficientModel},
		"judge":     {c.JudgeModel},
		"any":       {c.EfficientModel, c.CapableModel},
	}
}

// Router builds the routing core for this configuration.
func (c Config) Router() (*decision.Router, error) {
	return decision.New(c.Profile(), c.Models())
}

// FilterID names this profile, for use as a cache key.
const FilterID = "llm-decider/default"

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return fallback
}
