// Package e2e runs the llm-decider module against a real Envoy instance.
//
// One module registers at five entry points, and each has an observable effect
// here: the downstream filter decides, the cluster extension routes to the host
// serving the decided model, the upstream filter shapes the request, the LB policy
// serves its own cluster, and the access logger fires on stream end.
//
// Prerequisites:
//   - Envoy at .bin/envoy in the transit root (make download-envoy), or ENVOY_BIN.
//   - The shared library built first: make -C examples/llm-decider build
//
// Run:
//
//	make -C examples/llm-decider e2e
package e2e

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	examplesRoot string

	// The judge stub's verdict is swappable between subtests, and it records what
	// it was asked so the request shape is assertable.
	judgeAdvantage atomic.Value // float64
	judgeCalls     atomic.Int64
	judgeLastBody  atomic.Value // []byte
)

// envoyPorts are the template's substitution points.
type envoyPorts struct {
	ProxyPort int
	AdminPort int
	JudgePort int
	SmartPort int
	CheapPort int
}

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// llm-decider/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}
	if err := e2etest.CheckSharedLibrary(examplesRoot, "llm-decider", "libllm-decider.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	judgeAdvantage.Store(0.77)
	judgePort := startJudge()
	smartPort := startUpstream("smart")
	cheapPort := startUpstream("cheap")

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("transit-llm-decider-e2e", envoyConfigTmpl, envoyPorts{
		ProxyPort: proxyPort,
		AdminPort: adminPort,
		JudgePort: judgePort,
		SmartPort: smartPort,
		CheapPort: cheapPort,
	})

	// The module reads its configuration from the environment at load time.
	env := []string{
		"LLM_DECIDER_JUDGE_CLUSTER=llm-decider-judge",
		"LLM_DECIDER_JUDGE_HOST=judge.local",
		"LLM_DECIDER_JUDGE_PATH=/v1/systemone",
		"LLM_DECIDER_CAPABLE_MODEL=smart",
		"LLM_DECIDER_EFFICIENT_MODEL=cheap",
		"LLM_DECIDER_CUTOFF=0.4",
	}
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, filepath.Join(examplesRoot, "llm-decider"), adminPort, env)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestAboveCutoffRoutesToTheCapableHost covers the downstream filter making the
// decision and the cluster extension routing on it.
func TestAboveCutoffRoutesToTheCapableHost(t *testing.T) {
	judgeAdvantage.Store(0.77)

	got := decide(t, "Refactor the payment module onto the new ledger abstraction.", "")
	require.Equal(t, "smart", got["upstream"], "the cluster extension should route to the host owning the decided model")
	require.Equal(t, "smart", got["decided"], "the downstream filter should stamp the decision")
	require.Equal(t, "smart", got["target"], "the upstream filter should shape the request for the decided model")
	require.Equal(t, "decision_classifier", got["decisionFrom"])
}

// TestBelowCutoffRoutesToTheEfficientHost is the same path with the opposite
// verdict, so the routing is shown to follow the decision rather than a default.
func TestBelowCutoffRoutesToTheEfficientHost(t *testing.T) {
	judgeAdvantage.Store(0.05)

	got := decide(t, "write a haiku", "")
	require.Equal(t, "cheap", got["upstream"])
	require.Equal(t, "cheap", got["decided"])
}

// TestSessionLatchSkipsTheJudge covers the session latch end to end: a second
// request in the same session reuses the target without calling the judge.
func TestSessionLatchSkipsTheJudge(t *testing.T) {
	judgeAdvantage.Store(0.77)

	before := judgeCalls.Load()
	first := decide(t, "Refactor the payment module.", "tenant-a/e2e-session")
	second := decide(t, "and again", "tenant-a/e2e-session")

	require.Equal(t, "smart", first["upstream"])
	require.Equal(t, "smart", second["upstream"])
	require.Equal(t, int64(1), judgeCalls.Load()-before, "the latch should reduce this to one judge call")
	require.Equal(t, "retained", second["decisionFrom"], "the second turn should be attributed to the latch")
}

// TestJudgeReceivesTheQuestion covers what the module sends: the decision request
// the routing core built, encoded for the judge.
func TestJudgeReceivesTheQuestion(t *testing.T) {
	judgeAdvantage.Store(0.77)

	_ = decide(t, "refactor the payment module", "")

	raw, _ := judgeLastBody.Load().([]byte)
	require.NotEmpty(t, raw)

	var sent struct {
		Model     string `json:"model"`
		Questions map[string]struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.Equal(t, "jev-latest", sent.Model)

	question, ok := sent.Questions["route"]
	require.True(t, ok, "the judge should be asked the route question")
	require.Equal(t, "choice", question.Type)
	require.Contains(t, string(question.Criteria), "advantage")
	require.Contains(t, string(question.Criteria), "no_advantage")
}

// TestLBSurfaceServesItsOwnCluster covers the LB policy as a separate surface:
// Envoy owns this cluster's hosts and the module only chooses among them.
func TestLBSurfaceServesItsOwnCluster(t *testing.T) {
	resp, err := http.Get(proxyURL + "/lb")
	require.NoError(t, err)
	defer resp.Body.Close()

	// A bodiless request has nothing to decide on, but that must not fail the
	// request: it still reaches the LB-policy cluster.
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var seen map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&seen))
	require.Equal(t, "cheap", seen["upstream"])
}

// decide posts a prompt through the gateway and returns what the upstream saw.
func decide(t *testing.T, prompt, session string) map[string]string {
	t.Helper()

	request, err := http.NewRequest(http.MethodPost, proxyURL+"/",
		strings.NewReader(fmt.Sprintf(`{"prompt":%q}`, prompt)))
	require.NoError(t, err)
	request.Header.Set("content-type", "application/json")
	if session != "" {
		request.Header.Set("x-switchyard-session-id", session)
	}

	resp, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var seen map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&seen))
	return seen
}

// startJudge runs the stub judge. It answers in the System One wire shape, which
// is what the module's callout decodes.
func startJudge() int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startJudge: " + err.Error())
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		judgeCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		judgeLastBody.Store(body)

		advantage, _ := judgeAdvantage.Load().(float64)
		choice := "no_advantage"
		if advantage > 0.4 {
			choice = "advantage"
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w,
			`{"model":"jev-test","answers":{"route":{"type":"choice","choice":%q,`+
				`"confidence":0.9,"probabilities":{"advantage":%v,"no_advantage":%v}}},`+
				`"usage":{"input_tokens":10,"output_tokens":2}}`,
			choice, advantage, 1-advantage)
	})

	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	return listener.Addr().(*net.TCPAddr).Port
}

// startUpstream runs an upstream that reports which host answered, and the
// decision headers it received.
func startUpstream(name string) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"upstream":     name,
			"decided":      r.Header.Get("x-llm-decided-model"),
			"target":       r.Header.Get("x-llm-target-model"),
			"decisionFrom": r.Header.Get("x-llm-decision-source"),
		})
	})
	go func() { _ = http.Serve(listener, mux) }()
	return listener.Addr().(*net.TCPAddr).Port
}
