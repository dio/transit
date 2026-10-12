package config

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/libsygo"
	"github.com/dio/libsygo/jev"
)

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("LLM_DECIDER_CUTOFF", "")
	t.Setenv("LLM_DECIDER_CAPABLE_MODEL", "")

	cfg, err := FromEnv()
	require.NoError(t, err)
	require.Equal(t, "llm-decider-judge", cfg.JudgeCluster)
	require.Equal(t, "/v1/systemone", cfg.JudgePath)
	require.Equal(t, 0.4, cfg.Cutoff)
	require.Equal(t, "smart", cfg.CapableModel)
	require.Equal(t, "cheap", cfg.EfficientModel)
	require.True(t, cfg.FailOpen)
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("LLM_DECIDER_CUTOFF", "0.75")
	t.Setenv("LLM_DECIDER_CAPABLE_MODEL", "gpt-capable")
	t.Setenv("LLM_DECIDER_FAIL_OPEN", "false")

	cfg, err := FromEnv()
	require.NoError(t, err)
	require.Equal(t, 0.75, cfg.Cutoff)
	require.Equal(t, "gpt-capable", cfg.CapableModel)
	require.False(t, cfg.FailOpen)
}

// The pool must satisfy the any-subset invariant, and the capability pool order
// mirrors the reference runner: [efficient, capable].
func TestModelsMirrorTheReferencePool(t *testing.T) {
	cfg := Config{CapableModel: "smart", EfficientModel: "cheap", JudgeModel: "jev-latest"}
	models := cfg.Models()

	require.Equal(t, []string{"smart"}, models["capable"])
	require.Equal(t, []string{"cheap"}, models["efficient"])
	require.Equal(t, []string{"jev-latest"}, models["judge"])
	require.Equal(t, []string{"cheap", "smart"}, models["any"])
}

// End to end through the module's own configuration: a judge verdict above the
// cutoff selects the capable tier, with the fallback order and evidence the
// surfaces downstream read.
func TestRouterSelectsTheCapableTierAboveCutoff(t *testing.T) {
	cfg := Config{
		CapableModel:   "smart",
		EfficientModel: "cheap",
		JudgeModel:     "jev-latest",
		Cutoff:         0.4,
		FailOpen:       true,
	}

	router, err := cfg.Router()
	require.NoError(t, err)

	outcome := router.Route(context.Background(),
		libsygo.Turn{Request: libsygo.TextRequest("refactor the payment module"), Session: "tenant-a/s-1"},
		fakeDecider{advantage: 0.77})

	require.Empty(t, outcome.Error)
	require.Equal(t, "smart", outcome.Model)
	require.Equal(t, []string{"smart", "cheap"}, outcome.SelectedModelIDs)
	require.Contains(t, string(outcome.Evidence), `"score":0.77`)
	require.Contains(t, string(outcome.Evidence), `"threshold":0.4`)
}

func TestRouterSelectsTheEfficientTierBelowCutoff(t *testing.T) {
	cfg := Config{CapableModel: "smart", EfficientModel: "cheap", JudgeModel: "jev-latest", Cutoff: 0.4}

	router, err := cfg.Router()
	require.NoError(t, err)

	outcome := router.Route(context.Background(),
		libsygo.Turn{Request: libsygo.TextRequest("write a haiku"), Session: "s-1"},
		fakeDecider{advantage: 0.05})

	require.Equal(t, "cheap", outcome.Model)
	require.Equal(t, []string{"cheap", "smart"}, outcome.SelectedModelIDs)
}

// A judge failure with fail-open leaves a usable, auditable outcome rather than
// an unrouted request.
func TestRouterFailsOpenToCapable(t *testing.T) {
	cfg := Config{CapableModel: "smart", EfficientModel: "cheap", JudgeModel: "jev-latest", Cutoff: 0.4, FailOpen: true}

	router, err := cfg.Router()
	require.NoError(t, err)

	outcome := router.Route(context.Background(),
		libsygo.Turn{Request: libsygo.TextRequest("anything"), Session: "s-1"},
		fakeDecider{err: errJudge})

	require.Empty(t, outcome.Error)
	require.Equal(t, "smart", outcome.Model)
	require.Contains(t, string(outcome.Evidence), `"reason_code":"call_error"`)
}

// fail_open = false propagates instead, so a tenant can choose to fail rather
// than be routed on a guess.
func TestRouterFailsClosedWhenConfigured(t *testing.T) {
	cfg := Config{CapableModel: "smart", EfficientModel: "cheap", JudgeModel: "jev-latest", Cutoff: 0.4, FailOpen: false}

	router, err := cfg.Router()
	require.NoError(t, err)

	outcome := router.Route(context.Background(),
		libsygo.Turn{Request: libsygo.TextRequest("anything"), Session: "s-1"},
		fakeDecider{err: errJudge})

	require.NotEmpty(t, outcome.Error)
	require.Empty(t, outcome.Model)
}

var errJudge = &judgeError{}

type judgeError struct{}

func (e *judgeError) Error() string { return "judge unavailable" }

type fakeDecider struct {
	advantage float64
	err       error
}

func (f fakeDecider) Call(context.Context, jev.DecisionRequest) (jev.DecisionResponse, error) {
	if f.err != nil {
		return jev.DecisionResponse{}, f.err
	}
	data, err := json.Marshal(map[string]any{
		"selected":      "advantage",
		"probabilities": map[string]float64{"advantage": f.advantage},
	})
	if err != nil {
		return jev.DecisionResponse{}, err
	}
	return jev.DecisionResponse{
		Answers: map[string]jev.DecisionAnswer{
			"route": {Value: jev.Tagged{Type: "choice", Data: data}},
		},
	}, nil
}
