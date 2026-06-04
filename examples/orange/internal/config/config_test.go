package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/v1models.response.schema.json
var v1ModelsSchema []byte

// setMinimalEnv sets the env vars required by testdata/valid_minimal.yaml.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_OPENAI_KEY", "sk-test-openai")
}

// setFullEnv sets every env var referenced by testdata/valid_full.yaml.
func setFullEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_OPENAI_KEY", "sk-test-openai")
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-test-anthropic")
	t.Setenv("TEST_AZURE_KEY", "sk-test-azure")
	t.Setenv("TEST_GROQ_KEY", "sk-test-groq")
	t.Setenv("TEST_DEEPINFRA_KEY", "sk-test-deepinfra")
	t.Setenv("TEST_GITHUB_TOKEN", "github-test-token")
	// aws and gcp providers carry no secret_ref — no env var needed.
}

// --- LoadFile: valid configs --------------------------------------------------

func TestLoadFile_minimal(t *testing.T) {
	setMinimalEnv(t)
	cfg, err := LoadFile("testdata/valid_minimal.yaml")
	require.NoError(t, err)

	require.Contains(t, cfg.Providers, "openai")
	p := cfg.Providers["openai"]
	assert.Equal(t, "openai", p.Kind)
	assert.Equal(t, "https://api.openai.com", p.Endpoint)
	assert.Equal(t, "bearer", p.Auth.Type)
	assert.Equal(t, "env://TEST_OPENAI_KEY", p.Auth.SecretRef)
	assert.Equal(t, "sk-test-openai", cfg.ProviderSecret("openai"))

	require.Contains(t, cfg.Models, "gpt-4o")
	assert.Equal(t, "openai", cfg.Models["gpt-4o"].Provider)
}

func TestLoadFile_full(t *testing.T) {
	setFullEnv(t)
	cfg, err := LoadFile("testdata/valid_full.yaml")
	require.NoError(t, err)

	// Spot-check providers.
	wantProviders := []string{
		"openai", "anthropic", "azure", "bedrock",
		"vertex", "vertex_anthropic", "groq", "deepinfra",
	}
	for _, name := range wantProviders {
		assert.Contains(t, cfg.Providers, name, "provider %q should be loaded", name)
	}

	// backend_schema defaults to kind when omitted.
	assert.Equal(t, "openai", cfg.Providers["openai"].EffectiveBackendSchema())
	assert.Equal(t, "azureopenai", cfg.Providers["azure"].EffectiveBackendSchema())
	assert.Equal(t, "awsanthropic", cfg.Providers["bedrock"].EffectiveBackendSchema())
	assert.Equal(t, "gcpvertexai", cfg.Providers["vertex"].EffectiveBackendSchema())
	assert.Equal(t, "gcpanthropic", cfg.Providers["vertex_anthropic"].EffectiveBackendSchema())

	// path_prefix defaults to /v1 when absent.
	assert.Equal(t, "/v1", cfg.Providers["openai"].ResolvedPathPrefix())
	assert.Equal(t, "/openai/deployments/my-deployment", cfg.Providers["azure"].ResolvedPathPrefix())
	assert.Equal(t, "/openai/v1", cfg.Providers["groq"].ResolvedPathPrefix())
	assert.Equal(t, "/v1/openai", cfg.Providers["deepinfra"].ResolvedPathPrefix())

	// Secrets resolved only for providers with secret_ref.
	assert.Equal(t, "sk-test-openai", cfg.ProviderSecret("openai"))
	assert.Equal(t, "sk-test-azure", cfg.ProviderSecret("azure"))
	assert.Empty(t, cfg.ProviderSecret("bedrock"), "aws auth has no secret_ref")
	assert.Empty(t, cfg.ProviderSecret("vertex"), "gcp auth has no secret_ref")

	// extra fields passed through.
	assert.Equal(t, "2023-06-01", cfg.Providers["anthropic"].Extra["anthropic_version"])
	assert.Equal(t, "us-west-2", cfg.Providers["bedrock"].Extra["aws_region"])
	assert.Equal(t, "my-project", cfg.Providers["vertex"].Extra["gcp_project"])

	// Models map populated.
	assert.Contains(t, cfg.Models, "gpt-4o")
	assert.Contains(t, cfg.Models, "claude-sonnet")
	assert.Contains(t, cfg.Models, "groq/llama-3.1-8b-instant")
	assert.Contains(t, cfg.Models, "deepinfra/microsoft/phi-4")
	assert.Contains(t, cfg.Models, "vertex/claude-3-5-sonnet")

	require.NotNil(t, cfg.MCP)
	defaultRoute := cfg.MCP.Routes["default"]
	require.Contains(t, defaultRoute.Backends, "kiwi")
	assert.Equal(t, "orange-mcp-kiwi", defaultRoute.Backends["kiwi"].Cluster)
	assert.Equal(t, "https://mcp.kiwi.com", defaultRoute.Backends["kiwi"].Endpoint)
	assert.Equal(t, []string{"search-flight"}, defaultRoute.Backends["kiwi"].Tools.Include)

	require.Contains(t, defaultRoute.Backends, "aws-knowledge")
	assert.Equal(t, "https://knowledge-mcp.global.api.aws", defaultRoute.Backends["aws-knowledge"].Endpoint)
	assert.Empty(t, cfg.MCPCredential("default", "aws-knowledge"), "public backend has no resolved credential")

	require.Contains(t, defaultRoute.Backends, "github")
	github := defaultRoute.Backends["github"]
	assert.Equal(t, "orange-mcp-github", github.Cluster)
	assert.Equal(t, "https://api.githubcopilot.com/mcp/", github.Endpoint)
	assert.Equal(t, "env://TEST_GITHUB_TOKEN", github.CredentialRef)
	assert.Equal(t, "github-test-token", cfg.MCPCredential("default", "github"))
}

func TestLoadFile_exampleOrangeYAMLIncludesGPT4oMiniMetadata(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	t.Setenv("GITHUB_TOKEN", "github-test-token")

	cfg, err := LoadFile("../../orange.yaml")
	require.NoError(t, err)

	entry := cfg.Models["gpt-4o-mini"]
	assert.Equal(t, "openai", entry.Provider)
	assert.Equal(t, map[string]any{
		"description":    "GPT-4o mini via OpenAI.",
		"context_length": 128000,
		"max_tokens":     16384,
		"tags":           []any{"chat", "responses", "fast", "vision"},
	}, entry.Metadata)
}

func TestLoadFile_hostHelper(t *testing.T) {
	setMinimalEnv(t)
	cfg, err := LoadFile("testdata/valid_minimal.yaml")
	require.NoError(t, err)
	assert.Equal(t, "api.openai.com", cfg.Providers["openai"].Host())
}

// --- LoadFile: invalid configs ------------------------------------------------

func TestLoadFile_unknownAuthType(t *testing.T) {
	_, err := LoadFile("testdata/invalid_unknown_auth_type.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema")
}

func TestLoadFile_missingSecretRef(t *testing.T) {
	// bearer without secret_ref violates the schema if/then constraint.
	_, err := LoadFile("testdata/invalid_missing_secret_ref.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema")
}

func TestLoadFile_missingEndpoint(t *testing.T) {
	_, err := LoadFile("testdata/invalid_missing_endpoint.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema")
}

func TestLoadFile_missingKind(t *testing.T) {
	_, err := LoadFile("testdata/invalid_missing_kind.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema")
}

func TestLoadFile_badProviderRef(t *testing.T) {
	// Passes schema validation but fails the semantic cross-reference check.
	setMinimalEnv(t)
	_, err := LoadFile("testdata/invalid_bad_provider_ref.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in providers")
	assert.NotContains(t, err.Error(), "schema", "should be a semantic error, not a schema error")
}

func TestLoadFile_missingEnvVar(t *testing.T) {
	// secret_ref present but env var not set → secret resolution error.
	// (Do NOT call setMinimalEnv here.)
	_, err := LoadFile("testdata/valid_minimal.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEST_OPENAI_KEY")
}

// --- LookupModel -------------------------------------------------------------

func TestLookupModel_exactMatch(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"openai": {Kind: "openai", Endpoint: "https://api.openai.com"}},
		Models: map[string]ModelEntry{
			"gpt-4o": {Provider: "openai"},
		},
	}
	prov, backend := cfg.LookupModel("gpt-4o")
	assert.Equal(t, "openai", prov)
	assert.Equal(t, "gpt-4o", backend, "backend defaults to the map key")
}

func TestLookupModel_nameOverride(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"anthropic": {Kind: "anthropic", Endpoint: "https://api.anthropic.com"}},
		Models: map[string]ModelEntry{
			"claude-sonnet": {Provider: "anthropic", Name: "claude-3-5-sonnet-20241022"},
		},
	}
	prov, backend := cfg.LookupModel("claude-sonnet")
	assert.Equal(t, "anthropic", prov)
	assert.Equal(t, "claude-3-5-sonnet-20241022", backend)
}

func TestLookupModel_compoundName(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"deepinfra": {Kind: "openai", Endpoint: "https://api.deepinfra.com"}},
		Models: map[string]ModelEntry{
			"deepinfra/microsoft/phi-4": {Provider: "deepinfra", Name: "microsoft/phi-4"},
		},
	}
	prov, backend := cfg.LookupModel("deepinfra/microsoft/phi-4")
	assert.Equal(t, "deepinfra", prov)
	assert.Equal(t, "microsoft/phi-4", backend)
}

func TestLookupModel_miss(t *testing.T) {
	cfg := &Config{Models: map[string]ModelEntry{}}
	prov, backend := cfg.LookupModel("unknown-model")
	assert.Empty(t, prov)
	assert.Empty(t, backend)
}

func TestLookupModel_emptyID(t *testing.T) {
	cfg := &Config{Models: map[string]ModelEntry{}}
	prov, backend := cfg.LookupModel("")
	assert.Empty(t, prov)
	assert.Empty(t, backend)
}

// --- OpenAIV1Models ----------------------------------------------------------

func TestOpenAIV1Models_shape(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"openai":    {Kind: "openai"},
			"anthropic": {Kind: "anthropic"},
		},
		Models: map[string]ModelEntry{
			"gpt-4o":        {Provider: "openai"},
			"claude-sonnet": {Provider: "anthropic", Name: "claude-3-5-sonnet-20241022"},
			"gpt-4.1":       {Provider: "openai", Metadata: map[string]any{"context_length": 1047576}},
		},
	}

	list := cfg.OpenAIV1Models()
	assert.Equal(t, "list", list.Object)
	require.Len(t, list.Data, 3)

	// Sorted alphabetically.
	assert.Equal(t, "claude-sonnet", list.Data[0].ID)
	assert.Equal(t, "gpt-4.1", list.Data[1].ID)
	assert.Equal(t, "gpt-4o", list.Data[2].ID)

	// object field is always "model".
	for _, m := range list.Data {
		assert.Equal(t, "model", m.Object)
	}

	// owned_by is provider name, not kind.
	assert.Equal(t, "anthropic", list.Data[0].OwnedBy)
	assert.Equal(t, "openai", list.Data[1].OwnedBy)

	// metadata threaded through.
	assert.Equal(t, map[string]any{"context_length": 1047576}, list.Data[1].Metadata)
	assert.Nil(t, list.Data[2].Metadata)
}

func TestOpenAIV1Models_empty(t *testing.T) {
	cfg := &Config{Models: map[string]ModelEntry{}}
	list := cfg.OpenAIV1Models()
	assert.Equal(t, "list", list.Object)
	assert.Empty(t, list.Data)
}

func TestOpenAIV1Models_schemaValid(t *testing.T) {
	setFullEnv(t)
	cfg, err := LoadFile("testdata/valid_full.yaml")
	require.NoError(t, err)

	list := cfg.OpenAIV1Models()
	jsonBytes, err := json.Marshal(list)
	require.NoError(t, err)

	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("v1models.response.schema.json", bytes.NewReader(v1ModelsSchema)))
	sch, err := c.Compile("v1models.response.schema.json")
	require.NoError(t, err)

	// Validate by roundtripping through interface{} (same normalisation as config).
	var doc interface{}
	require.NoError(t, json.Unmarshal(jsonBytes, &doc))
	assert.NoError(t, sch.Validate(doc))
}

// --- Get / MustReload singleton ----------------------------------------------

func TestGet_caches(t *testing.T) {
	setMinimalEnv(t)
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)
	t.Setenv(EnvVar, abs)
	t.Cleanup(MustReload)
	MustReload()

	first := Get()
	require.NotNil(t, first)
	second := Get()
	assert.Same(t, first, second, "Get must return the same pointer on repeated calls")
}

func TestGet_panic_unset(t *testing.T) {
	require.NoError(t, os.Unsetenv(EnvVar))
	t.Cleanup(MustReload)
	MustReload()

	assert.Panics(t, func() { Get() }, "Get must panic when ORANGE_CONFIG is unset")
}

func TestMustReload_resets(t *testing.T) {
	setMinimalEnv(t)
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)
	t.Setenv(EnvVar, abs)
	t.Cleanup(MustReload)
	MustReload()

	first := Get()
	require.NotNil(t, first)

	MustReload()
	second := Get()
	require.NotNil(t, second)
	assert.NotSame(t, first, second, "MustReload must cause Get to load a fresh Config")
}

func TestGet_concurrent(t *testing.T) {
	setMinimalEnv(t)
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)
	t.Setenv(EnvVar, abs)
	t.Cleanup(MustReload)
	MustReload()

	const n = 20
	results := make([]*Config, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			results[i] = Get()
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		assert.Same(t, results[0], results[i], "all goroutines must get the same *Config")
	}
}

// --- NewSource ---------------------------------------------------------------

func TestNewSource_filePath(t *testing.T) {
	setMinimalEnv(t)
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)

	src, _, err := NewSource(abs)
	require.NoError(t, err)
	data, err := src(t.Context())
	require.NoError(t, err)
	cfg, err := Load(data)
	require.NoError(t, err)
	assert.Contains(t, cfg.Providers, "openai")
}

func TestNewSource_fileScheme(t *testing.T) {
	setMinimalEnv(t)
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)

	src, _, err := NewSource("file://" + abs)
	require.NoError(t, err)
	data, err := src(t.Context())
	require.NoError(t, err)
	cfg, err := Load(data)
	require.NoError(t, err)
	assert.Contains(t, cfg.Providers, "openai")
}

func TestNewSource_relpath(t *testing.T) {
	setMinimalEnv(t)

	src, _, err := NewSource("testdata/valid_minimal.yaml")
	require.NoError(t, err)
	data, err := src(t.Context())
	require.NoError(t, err)
	cfg, err := Load(data)
	require.NoError(t, err)
	assert.Contains(t, cfg.Providers, "openai")
}

func TestNewSource_emptyDSN(t *testing.T) {
	_, _, err := NewSource("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvVar)
}

func TestNewSource_missingFile(t *testing.T) {
	src, _, err := NewSource("/no/such/file.yaml")
	require.NoError(t, err) // source construction succeeds; error surfaces on fetch
	_, err = src(t.Context())
	require.Error(t, err)
}

// --- NewSource: poll options via query string ---------------------------------

func TestNewSource_pollInterval(t *testing.T) {
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)

	_, opts, err := NewSource(abs + "?poll_interval=2m")
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, opts.Interval)
	assert.Zero(t, opts.Timeout)
	assert.Zero(t, opts.Jitter)
}

func TestNewSource_pollAllOptions(t *testing.T) {
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)

	_, opts, err := NewSource(abs + "?poll_interval=1m&poll_timeout=10s&poll_jitter=5s")
	require.NoError(t, err)
	assert.Equal(t, time.Minute, opts.Interval)
	assert.Equal(t, 10*time.Second, opts.Timeout)
	assert.Equal(t, 5*time.Second, opts.Jitter)
}

func TestNewSource_pollBadDuration(t *testing.T) {
	abs, err := filepath.Abs("testdata/valid_minimal.yaml")
	require.NoError(t, err)

	_, _, err = NewSource(abs + "?poll_interval=notaduration")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll_interval")
}

func TestNewSource_httpStripsOrangeParams(t *testing.T) {
	// poll_interval is an orange DSN param and must not appear in the HTTP
	// target URL. We use 127.0.0.1:1 which fails immediately with "connection
	// refused", letting us inspect the error URL without a real server.
	src, opts, err := NewSource("http://127.0.0.1:1/config.yaml?poll_interval=30s&other=keep")
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, opts.Interval)

	_, fetchErr := src(t.Context())
	require.Error(t, fetchErr)
	assert.Contains(t, fetchErr.Error(), "other=keep", "non-orange params must be preserved")
	assert.NotContains(t, fetchErr.Error(), "poll_interval", "orange params must be stripped from HTTP target")
}

// --- Provider helpers --------------------------------------------------------

func TestEffectiveBackendSchema_explicitOverride(t *testing.T) {
	p := Provider{Kind: "openai", BackendSchema: "azureopenai"}
	assert.Equal(t, "azureopenai", p.EffectiveBackendSchema())
}

func TestEffectiveBackendSchema_defaultsToKind(t *testing.T) {
	p := Provider{Kind: "anthropic"}
	assert.Equal(t, "anthropic", p.EffectiveBackendSchema())
}

func TestResolvedPathPrefix_explicit(t *testing.T) {
	prefix := "/openai/v1"
	p := Provider{PathPrefix: &prefix}
	assert.Equal(t, "/openai/v1", p.ResolvedPathPrefix())
}

func TestResolvedPathPrefix_default(t *testing.T) {
	p := Provider{}
	assert.Equal(t, "/v1", p.ResolvedPathPrefix())
}

func TestHost_valid(t *testing.T) {
	p := Provider{Endpoint: "https://api.openai.com"}
	assert.Equal(t, "api.openai.com", p.Host())
}

func TestHost_empty(t *testing.T) {
	p := Provider{}
	assert.Empty(t, p.Host())
}
