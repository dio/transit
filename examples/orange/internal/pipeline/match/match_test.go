package match

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/down"
	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

func loadTestConfig(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	t.Setenv("GROQ_API_KEY", "sk-test-groq")
	t.Setenv(config.EnvVar, "testdata/match_test.yaml")
	t.Cleanup(config.MustReload)
	config.MustReload()
}

func newPostStream(t *testing.T) (*up.Writer, *testutil.FakeFilterHandle, *up.Request, *any) {
	t.Helper()
	hdr := map[string]string{
		":method":      "POST",
		":path":        "/v1/chat/completions",
		"content-type": "application/json",
	}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx
	return w, h, r, &ctx
}

// runFlow runs router.Dispatch and (if body != nil) bodyHandler, sharing the
// per-stream context slot the SDK normally provides via *f.context.
func runFlow(t *testing.T, body []byte) (*testutil.FakeFilterHandle, *up.StreamPromise[Decision]) {
	t.Helper()
	w, h, r, ctx := newPostStream(t)
	router.Dispatch(w, r)
	if body == nil {
		p, _ := (*ctx).(*up.StreamPromise[Decision])
		return h, p
	}
	chunk := &up.BodyChunk{Data: body, EndStream: true, Context: ctx}
	bodyHandler(w, chunk)
	p, _ := (*ctx).(*up.StreamPromise[Decision])
	return h, p
}

// streamObjectNonce reads the "up.stream_object_id" filter state from the
// handle; returns "" if absent.
func streamObjectNonce(h *testutil.FakeFilterHandle) string {
	v, ok := h.FilterStateString(down.StreamObjectIDKey)
	if !ok {
		return ""
	}
	return v
}

func TestHeaders_storesPendingInStreamObjectBag(t *testing.T) {
	loadTestConfig(t)
	w, h, r, ctx := newPostStream(t)
	router.Dispatch(w, r)

	p, ok := (*ctx).(*up.StreamPromise[Decision])
	require.True(t, ok && p != nil, "expected promise to be stashed in r.Context")

	// The promise must be in the stream-object bag under DecisionKey.
	nonce := streamObjectNonce(h)
	require.NotEmpty(t, nonce, "stream-object nonce not written to filter state")

	bag, ok := down.LookupStreamObjectBag(nonce)
	require.True(t, ok, "bag not found for nonce")

	v, ok := bag.Get(DecisionKey.Key())
	require.True(t, ok, "DecisionKey not in bag")

	promise, ok := v.(*up.StreamPromise[Decision])
	require.True(t, ok, "bag entry has wrong type: %T", v)
	require.Same(t, p, promise, "bag entry is not the same promise as the context slot")

	// Cleanup: drain the bag (normally done by OnStreamComplete).
	down.DropStreamObjectBag(nonce)
}

func TestBody_knownModel_resolvesUpstream(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	h, p := runFlow(t, body)

	require.Empty(t, h.LocalResponses, "unexpected local response")

	res, ok := p.Result()
	require.True(t, ok, "promise not resolved")
	require.Empty(t, res.Err)
	require.Equal(t, "openai_direct", res.ProviderBackend)
	require.Equal(t, "openai", res.ProviderKind)
	require.Equal(t, "gpt-4o-mini", res.Model)

	v, ok := h.Metadata(MetadataNamespace, MetadataKeyUpstream)
	require.True(t, ok)
	require.Equal(t, "openai_direct", v)

	// Bag entry must still be present before onStreamComplete (SDK owns the drain).
	nonce := streamObjectNonce(h)
	require.NotEmpty(t, nonce, "nonce missing from filter state")
	_, ok = down.LookupStreamObjectBag(nonce)
	require.True(t, ok, "bag entry unexpectedly missing before stream complete")

	// Simulate OnStreamComplete: call onStreamComplete then drain the bag.
	var ctx any = p
	onStreamComplete(&ctx)
	down.DropStreamObjectBag(nonce)

	// After drain, bag must be gone.
	_, ok = down.LookupStreamObjectBag(nonce)
	require.False(t, ok, "bag entry not gone after stream complete + drain")

	// Resolve must remain the bodyHandler's (CAS loses races silently).
	got, _ := p.Result()
	require.Empty(t, got.Err, "post-cleanup err should be empty (CAS should preserve body result)")
}

func TestOnStreamComplete_cleansUpWhenBodyNeverRan(t *testing.T) {
	loadTestConfig(t)
	// Headers-only flow: simulates downstream disconnect / timeout after
	// headers and before the body handler runs.
	h, p := runFlow(t, nil)
	require.NotNil(t, p, "expected promise from headers phase")

	nonce := streamObjectNonce(h)
	require.NotEmpty(t, nonce, "nonce missing from filter state")

	_, ok := down.LookupStreamObjectBag(nonce)
	require.True(t, ok, "bag entry missing before onStreamComplete")

	var ctx any = p
	onStreamComplete(&ctx)

	// onStreamComplete must have resolved the promise with ErrStreamTerminated.
	res, ok := p.Result()
	require.True(t, ok, "promise should be resolved by onStreamComplete")
	require.Equal(t, ErrStreamTerminated, res.Err)

	// SDK drains the bag; simulate that here.
	down.DropStreamObjectBag(nonce)
	_, ok = down.LookupStreamObjectBag(nonce)
	require.False(t, ok, "bag still present after drain")
}

func TestOnStreamComplete_nilContextIsNoop(t *testing.T) {
	loadTestConfig(t)
	// SDK calls onStreamComplete even for requests that never matched the
	// route (no promise was ever stashed). Must not panic.
	var ctx any
	onStreamComplete(&ctx)
	onStreamComplete(nil)
}

func TestBody_missingModel_400(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"messages":[]}`)
	h, p := runFlow(t, body)

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 400, h.LocalResponses[0].Status)

	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(h.LocalResponses[0].Body, &got))
	require.Equal(t, ErrModelRequired, got.Error.Code)

	res, _ := p.Result()
	require.Equal(t, ErrModelRequired, res.Err)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_unknownModel_404(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gemini-1.5"}`)
	h, p := runFlow(t, body)

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 404, h.LocalResponses[0].Status)

	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(h.LocalResponses[0].Body, &got))
	require.Equal(t, "orange.model_not_found", got.Error.Code)

	res, _ := p.Result()
	require.Equal(t, "orange.model_not_found", res.Err)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestHeaders_nonMatchingRequest_404(t *testing.T) {
	loadTestConfig(t)
	hdr := map[string]string{":method": "GET", ":path": "/healthz"}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx

	router.Dispatch(w, r)
	require.Nil(t, ctx, "expected no promise for non-matching request")
	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 404, h.LocalResponses[0].Status)
}

// TestPick_getStreamObject: verifies that a FakeClusterLBContext backed
// by the same handle can retrieve the promise via DecisionKey.GetFromCtx.
func TestPick_getStreamObject(t *testing.T) {
	loadTestConfig(t)
	w, h, r, _ := newPostStream(t)
	router.Dispatch(w, r)

	ctx := testutil.NewFakeClusterLBContext(h)
	promise, ok := DecisionKey.GetFromCtx(ctx)
	require.True(t, ok, "DecisionKey.GetFromCtx returned false — nonce not propagated to filter state")
	require.NotNil(t, promise)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// newPostStreamTo is like newPostStream but uses the given path.
func newPostStreamTo(t *testing.T, path string) (*up.Writer, *testutil.FakeFilterHandle, *up.Request, *any) {
	t.Helper()
	hdr := map[string]string{
		":method":      "POST",
		":path":        path,
		"content-type": "application/json",
	}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx
	return w, h, r, &ctx
}

func newHeaderStream(t *testing.T, method, path string) (*up.Writer, *testutil.FakeFilterHandle, *up.Request, *any) {
	t.Helper()
	hdr := map[string]string{":method": method, ":path": path, ":authority": "orange.local"}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx
	return w, h, r, &ctx
}

func TestHeaders_messagesPath(t *testing.T) {
	loadTestConfig(t)
	w, h, r, ctx := newPostStreamTo(t, pathV1Messages)
	router.Dispatch(w, r)

	p, ok := (*ctx).(*up.StreamPromise[Decision])
	require.True(t, ok && p != nil, "expected promise for /v1/messages")
	require.NotEmpty(t, streamObjectNonce(h))

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestHeaders_getOnKnownPath_404(t *testing.T) {
	loadTestConfig(t)
	hdr := map[string]string{":method": "GET", ":path": pathV1ChatCompletions}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx

	router.Dispatch(w, r)
	require.Nil(t, ctx, "GET on /v1/chat/completions must not create a promise")
	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 404, h.LocalResponses[0].Status)
}

func TestHeaders_getV1Models_returnsLocalModelList(t *testing.T) {
	loadTestConfig(t)
	w, h, r, ctx := newHeaderStream(t, http.MethodGet, pathV1Models)

	router.Dispatch(w, r)

	require.Nil(t, *ctx, "GET /v1/models must not create a stream promise")
	require.Empty(t, streamObjectNonce(h), "GET /v1/models must not create a stream-object bag")
	_, ok := h.Metadata(MetadataNamespace, MetadataKeyModel)
	require.False(t, ok, "GET /v1/models must not write routing metadata")

	_, ok = h.FilterStateString(StateModel)
	require.False(t, ok, "GET /v1/models must not write routing filter state")
	authority := h.RequestHeaders().GetOne(":authority").ToString()
	require.Equal(t, "orange.local", authority, "GET /v1/models must not rewrite :authority")

	require.Len(t, h.LocalResponses, 1)
	resp := h.LocalResponses[0]
	require.EqualValues(t, http.StatusOK, resp.Status)
	require.Equal(t, [2]string{"content-type", "application/json"}, resp.Headers[0])

	var got config.V1ModelList
	require.NoError(t, json.Unmarshal(resp.Body, &got))
	require.Equal(t, "list", got.Object)
	require.Len(t, got.Data, 4)
	require.Equal(t, "claude-3-5-sonnet-20241022", got.Data[0].ID)
	require.Equal(t, "claude-sonnet", got.Data[1].ID)
	require.Equal(t, "gpt-4o-mini", got.Data[2].ID)
	require.Equal(t, "groq/llama-3.1-8b-instant", got.Data[3].ID)
	require.Equal(t, "openai_direct", got.Data[2].OwnedBy)
	require.Equal(t, map[string]any{"tier": "fast"}, got.Data[2].Metadata)
}

func TestHeaders_postUnknownPath_404(t *testing.T) {
	loadTestConfig(t)
	w, h, r, ctx := newPostStreamTo(t, "/v1/models")
	router.Dispatch(w, r)
	require.Nil(t, *ctx, "POST to untracked path must not create a promise")
	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 404, h.LocalResponses[0].Status)
}

func TestBody_anthropicKind(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[]}`)
	h, p := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "anthropic_direct", res.ProviderBackend)
	require.Equal(t, "anthropic", res.ProviderKind)
	require.Equal(t, "claude-3-5-sonnet-20241022", res.Model)

	v, ok := h.Metadata(MetadataNamespace, MetadataKeyProvider)
	require.True(t, ok)
	require.Equal(t, "anthropic", v)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// TestBody_modelWithNameOverride verifies that when a ModelEntry has a Name
// override (e.g. "claude-sonnet" → "claude-3-5-sonnet-20241022"), Decision.Model
// still carries the client-facing key, not the backend name.
func TestBody_modelWithNameOverride(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"claude-sonnet","messages":[]}`)
	h, p := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "anthropic_direct", res.ProviderBackend)
	require.Equal(t, "claude-sonnet", res.Model, "Decision.Model must be the client-facing name, not the backend alias")

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_compoundModelName(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"groq/llama-3.1-8b-instant","messages":[]}`)
	h, p := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "groq", res.ProviderBackend)
	require.Equal(t, "openai", res.ProviderKind)
	require.Equal(t, "groq/llama-3.1-8b-instant", res.Model)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_authorityRewrite(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	h, _ := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	authority := h.RequestHeaders().GetOne(":authority").ToString()
	require.Equal(t, "api.openai.com", authority, ":authority must be rewritten to the provider host")

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_filterStatePopulated(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	h, _ := runFlow(t, body)

	require.Empty(t, h.LocalResponses)

	model, ok := h.FilterStateString(StateModel)
	require.True(t, ok)
	require.Equal(t, "gpt-4o-mini", model)

	upstream, ok := h.FilterStateString(StateUpstream)
	require.True(t, ok)
	require.Equal(t, "openai_direct", upstream)

	provider, ok := h.FilterStateString(StateProvider)
	require.True(t, ok)
	require.Equal(t, "openai", provider)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// --- POST /v1/responses tests ---

// runResponsesFlow runs the match pipeline for POST /v1/responses.
func runResponsesFlow(t *testing.T, body []byte) (*testutil.FakeFilterHandle, *up.StreamPromise[Decision]) {
	t.Helper()
	hdr := map[string]string{
		":method":      "POST",
		":path":        pathV1Responses,
		"content-type": "application/json",
	}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx
	router.Dispatch(w, r)
	if body == nil {
		p, _ := ctx.(*up.StreamPromise[Decision])
		return h, p
	}
	chunk := &up.BodyChunk{Data: body, EndStream: true, Context: &ctx}
	bodyHandler(w, chunk)
	p, _ := ctx.(*up.StreamPromise[Decision])
	return h, p
}

func TestResponses_knownModel_resolvesUpstreamWithEndpoint(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-4o-mini","input":"hello"}`)
	h, p := runResponsesFlow(t, body)

	require.Empty(t, h.LocalResponses, "unexpected local response")

	res, ok := p.Result()
	require.True(t, ok, "promise not resolved")
	require.Empty(t, res.Err)
	require.Equal(t, "openai_direct", res.ProviderBackend)
	require.Equal(t, "openai", res.ProviderKind)
	require.Equal(t, "gpt-4o-mini", res.Model)
	require.Equal(t, EndpointResponses, res.Endpoint, "endpoint must be 'responses'")

	ep, ok := h.Metadata(MetadataNamespace, MetadataKeyEndpoint)
	require.True(t, ok, "endpoint metadata must be written by Apply")
	require.Equal(t, EndpointResponses, ep)

	epState, ok := h.FilterStateString(StateEndpoint)
	require.True(t, ok, "endpoint filter state must be written by Apply")
	require.Equal(t, EndpointResponses, epState)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestResponses_missingModel_400(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"input":"hello"}`)
	h, p := runResponsesFlow(t, body)

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 400, h.LocalResponses[0].Status)

	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(h.LocalResponses[0].Body, &got))
	require.Equal(t, ErrModelRequired, got.Error.Code)

	res, _ := p.Result()
	require.Equal(t, ErrModelRequired, res.Err)
	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestResponses_unknownModel_404(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-99-turbo","input":"hello"}`)
	h, p := runResponsesFlow(t, body)

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 404, h.LocalResponses[0].Status)

	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(h.LocalResponses[0].Body, &got))
	require.Equal(t, ErrUnknownModel, got.Error.Code)

	res, _ := p.Result()
	require.Equal(t, ErrUnknownModel, res.Err)
	down.DropStreamObjectBag(streamObjectNonce(h))
}

// TestGetV1Responses_WebSocketPassthrough verifies that GET /v1/responses is
// handled by the passthrough handler (no promise, no local response).
func TestGetV1Responses_WebSocketPassthrough(t *testing.T) {
	loadTestConfig(t)
	hdr := map[string]string{":method": "GET", ":path": pathV1Responses, ":authority": "orange.local"}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx

	router.Dispatch(w, r)

	require.Nil(t, ctx, "GET /v1/responses must not create a promise")
	require.Empty(t, h.LocalResponses, "GET /v1/responses must not send a local response (WS passthrough)")
}

// TestChatCompletions_endpointMetadata verifies that the existing chat
// completions flow writes EndpointChatCompletions (regression guard).
func TestChatCompletions_endpointMetadata(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	h, p := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Equal(t, EndpointChatCompletions, res.Endpoint)

	ep, ok := h.Metadata(MetadataNamespace, MetadataKeyEndpoint)
	require.True(t, ok)
	require.Equal(t, EndpointChatCompletions, ep)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_partialChunk_skipped(t *testing.T) {
	loadTestConfig(t)
	w, h, r, ctx := newPostStream(t)
	router.Dispatch(w, r)
	p := (*ctx).(*up.StreamPromise[Decision])

	// Deliver a non-terminal chunk — bodyHandler must not resolve the promise.
	chunk := &up.BodyChunk{Data: []byte(`{"model":"gpt-4o-mini"}`), EndStream: false, Context: ctx}
	bodyHandler(w, chunk)

	_, resolved := p.Result()
	require.False(t, resolved, "promise must not be resolved on a non-terminal chunk")
	require.Empty(t, h.LocalResponses)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// --- parseBearerToken --------------------------------------------------------

func TestParseBearerToken(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		wantID string
	}{
		{"standard", "Bearer sk-abc123", "sk-abc123"},
		{"lowercase_prefix", "bearer sk-abc123", "sk-abc123"},
		{"mixed_case_prefix", "BEARER sk-abc123", "sk-abc123"},
		{"compound_key_id", "Bearer acme/alice/sk-abc123", "acme/alice/sk-abc123"},
		{"empty", "", ""},
		{"no_bearer", "Token sk-abc123", ""},
		{"bearer_only", "Bearer ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBearerToken(tc.input)
			require.Equal(t, tc.wantID, got)
		})
	}
}

// --- ResolveKey / key-mode ---------------------------------------------------

func loadKeysConfig(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv(config.EnvVar, "testdata/match_keys_test.yaml")
	t.Cleanup(config.MustReload)
	config.MustReload()
}

func newPostStreamWithAuth(t *testing.T, authHeader string) (*up.Writer, *testutil.FakeFilterHandle, *up.Request, *any) {
	t.Helper()
	hdr := map[string]string{
		":method":       "POST",
		":path":         "/v1/chat/completions",
		"content-type":  "application/json",
		"authorization": authHeader,
	}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx
	return w, h, r, &ctx
}

func TestResolveKey_legacyMode_returnsNil(t *testing.T) {
	loadTestConfig(t) // no keys[] in this config
	kb, keyID, ok := ResolveKey("Bearer acme/alice/sk-test-001")
	require.False(t, ok, "legacy mode must return ok=false")
	require.Nil(t, kb)
	require.Empty(t, keyID)
}

func TestResolveKey_knownKey(t *testing.T) {
	loadKeysConfig(t)
	kb, keyID, ok := ResolveKey("Bearer acme/alice/sk-test-001")
	require.True(t, ok, "known key must resolve")
	require.NotNil(t, kb)
	require.Equal(t, "acme/alice/sk-test-001", keyID)
	require.Equal(t, "acme", kb.Workspace)
	require.Equal(t, "alice", kb.User)
}

func TestResolveKey_unknownKey(t *testing.T) {
	loadKeysConfig(t)
	kb, keyID, ok := ResolveKey("Bearer acme/alice/does-not-exist")
	require.False(t, ok, "unknown key must return ok=false")
	require.Nil(t, kb)
	require.Equal(t, "acme/alice/does-not-exist", keyID, "keyID is still returned on miss for logging")
}

func TestHeaders_unknownKey_401(t *testing.T) {
	loadKeysConfig(t)
	w, h, r, ctx := newPostStreamWithAuth(t, "Bearer acme/alice/not-a-real-key")
	router.Dispatch(w, r)

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 401, h.LocalResponses[0].Status)

	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(h.LocalResponses[0].Body, &got))
	require.Equal(t, ErrUnknownKey, got.Error.Code)

	// Promise must be resolved with the unknown-key error.
	p, ok := (*ctx).(*up.StreamPromise[Decision])
	require.True(t, ok && p != nil)
	res, resolved := p.Result()
	require.True(t, resolved)
	require.Equal(t, ErrUnknownKey, res.Err)

	// reject_reason metadata must be set.
	v, ok := h.Metadata(MetadataNamespace, MetadataKeyRejectReason)
	require.True(t, ok)
	require.Equal(t, ErrUnknownKey, v)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestHeaders_knownKey_attributionMetadata(t *testing.T) {
	loadKeysConfig(t)
	w, h, r, ctx := newPostStreamWithAuth(t, "Bearer acme/alice/sk-test-001")
	router.Dispatch(w, r)

	require.Empty(t, h.LocalResponses, "known key must not produce a local response")

	// Attribution metadata must be written at headers phase.
	ws, ok := h.Metadata(MetadataNamespace, MetadataKeyAttributionWorkspace)
	require.True(t, ok)
	require.Equal(t, "acme", ws)

	user, ok := h.Metadata(MetadataNamespace, MetadataKeyAttributionUser)
	require.True(t, ok)
	require.Equal(t, "alice", user)

	keyID, ok := h.Metadata(MetadataNamespace, MetadataKeyAttributionKeyID)
	require.True(t, ok)
	require.Equal(t, "acme/alice/sk-test-001", keyID)

	p, ok := (*ctx).(*up.StreamPromise[Decision])
	require.True(t, ok && p != nil)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_keyMode_knownModel_resolves(t *testing.T) {
	loadKeysConfig(t)
	w, h, r, ctx := newPostStreamWithAuth(t, "Bearer acme/alice/sk-test-001")
	router.Dispatch(w, r)
	require.Empty(t, h.LocalResponses)

	chunk := &up.BodyChunk{Data: []byte(`{"model":"gpt-4o-mini","messages":[]}`), EndStream: true, Context: ctx}
	bodyHandler(w, chunk)

	p, _ := (*ctx).(*up.StreamPromise[Decision])
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "openai_direct", res.ProviderBackend)
	require.Equal(t, "openai", res.ProviderKind)
	require.Equal(t, "gpt-4o-mini", res.Model)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_keyMode_unknownModel_404(t *testing.T) {
	loadKeysConfig(t)
	w, h, r, ctx := newPostStreamWithAuth(t, "Bearer acme/alice/sk-test-001")
	router.Dispatch(w, r)
	require.Empty(t, h.LocalResponses)

	// gpt-99 is not in the key blob's model table.
	chunk := &up.BodyChunk{Data: []byte(`{"model":"gpt-99","messages":[]}`), EndStream: true, Context: ctx}
	bodyHandler(w, chunk)

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 404, h.LocalResponses[0].Status)

	p, _ := (*ctx).(*up.StreamPromise[Decision])
	res, _ := p.Result()
	require.Equal(t, ErrUnknownModel, res.Err)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// --- binding tests -----------------------------------------------------------

func loadBindingsConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	t.Setenv(config.EnvVar, "testdata/match_bindings_test.yaml")
	t.Cleanup(config.MustReload)
	config.MustReload()
}

// TestBody_binding_setsDecisionBinding verifies that Decision.Binding is set
// when a model entry carries a binding field.
func TestBody_binding_setsDecisionBinding(t *testing.T) {
	loadBindingsConfig(t)
	body := []byte(`{"model":"claude-east","messages":[]}`)
	h, p := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "anthropic", res.ProviderBackend)
	require.Equal(t, "us-east", res.Binding, "Decision.Binding must be set from the model entry")

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// TestBody_binding_westEntry verifies the us-west binding is threaded through.
func TestBody_binding_westEntry(t *testing.T) {
	loadBindingsConfig(t)
	body := []byte(`{"model":"claude-west","messages":[]}`)
	h, p := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "us-west", res.Binding)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// TestBody_binding_filterState verifies StateBinding filter state is written.
func TestBody_binding_filterState(t *testing.T) {
	loadBindingsConfig(t)
	body := []byte(`{"model":"claude-east","messages":[]}`)
	h, _ := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	b, ok := h.FilterStateString(StateBinding)
	require.True(t, ok)
	require.Equal(t, "us-east", b, "StateBinding filter state must carry the binding name")

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// TestBody_binding_authorityRewrite verifies that :authority is rewritten to
// the binding's endpoint host, not the provider's top-level Host().
func TestBody_binding_authorityRewrite(t *testing.T) {
	loadBindingsConfig(t)
	body := []byte(`{"model":"claude-west","messages":[]}`)
	h, _ := runFlow(t, body)

	require.Empty(t, h.LocalResponses)
	authority := h.RequestHeaders().GetOne(":authority").ToString()
	require.Equal(t, "api-west.anthropic.com", authority,
		":authority must be rewritten to the binding endpoint host")

	down.DropStreamObjectBag(streamObjectNonce(h))
}

// --- split routing tests ---

func loadSplitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	t.Setenv(config.EnvVar, "testdata/match_split_test.yaml")
	t.Cleanup(config.MustReload)
	config.MustReload()
}

func runSplitFlow(t *testing.T, model string) (*testutil.FakeFilterHandle, *up.StreamPromise[Decision]) {
	t.Helper()
	body := []byte(`{"model":"` + model + `","messages":[]}`)
	return runFlow(t, body)
}

// TestBody_split_allArmsReachable verifies that over N requests, all three
// arms of a three-way split are selected at least once.
func TestBody_split_allArmsReachable(t *testing.T) {
	loadSplitConfig(t)
	seen := map[string]bool{}
	const N = 300
	for range N {
		h, p := runSplitFlow(t, "split-model")
		require.Empty(t, h.LocalResponses)
		res, ok := p.Result()
		require.True(t, ok)
		require.Empty(t, res.Err)
		seen[res.ProviderBackend] = true
		down.DropStreamObjectBag(streamObjectNonce(h))
	}
	require.True(t, seen["provider_a"], "provider_a must be selected at least once")
	require.True(t, seen["provider_b"], "provider_b must be selected at least once")
	require.True(t, seen["provider_c"], "provider_c must be selected at least once")
}

// TestBody_split_modelIsMapKey verifies that Decision.Model is always the
// client-facing model key regardless of which arm is sampled.
func TestBody_split_modelIsMapKey(t *testing.T) {
	loadSplitConfig(t)
	const N = 20
	for range N {
		h, p := runSplitFlow(t, "split-model")
		require.Empty(t, h.LocalResponses)
		res, ok := p.Result()
		require.True(t, ok)
		require.Equal(t, "split-model", res.Model)
		down.DropStreamObjectBag(streamObjectNonce(h))
	}
}

// TestBody_split_backendModelFromTargetName verifies Decision.BackendModel
// equals the sampled child's Target.Name.
func TestBody_split_backendModelFromTargetName(t *testing.T) {
	loadSplitConfig(t)
	const N = 20
	for range N {
		h, p := runSplitFlow(t, "split-model")
		require.Empty(t, h.LocalResponses)
		res, ok := p.Result()
		require.True(t, ok)
		require.Equal(t, "split-model", res.BackendModel)
		down.DropStreamObjectBag(streamObjectNonce(h))
	}
}

// TestBody_split_backendModelFallsBackToMapKey verifies that when Target.Name
// is unset, Decision.BackendModel falls back to the model map key.
func TestBody_split_backendModelFallsBackToMapKey(t *testing.T) {
	loadSplitConfig(t)
	const N = 20
	for range N {
		h, p := runSplitFlow(t, "split-no-name")
		require.Empty(t, h.LocalResponses)
		res, ok := p.Result()
		require.True(t, ok)
		require.Equal(t, "split-no-name", res.BackendModel)
		down.DropStreamObjectBag(streamObjectNonce(h))
	}
}

// TestBody_split_chainArm_fallbacksPopulated verifies that when a split arm
// is a chain, Decision.Fallbacks is populated from the chain's remaining children.
func TestBody_split_chainArm_fallbacksPopulated(t *testing.T) {
	loadSplitConfig(t)
	// split-with-chain: arm 0 = chain(provider_a → provider_b), arm 1 = provider_c
	// We need to hit arm 0 at least once to check fallbacks. Run many times.
	hitChainArm := false
	const N = 100
	for range N {
		h, p := runSplitFlow(t, "split-with-chain")
		require.Empty(t, h.LocalResponses)
		res, ok := p.Result()
		require.True(t, ok)
		if res.ProviderBackend == "provider_a" {
			hitChainArm = true
			require.Len(t, res.Fallbacks, 1, "chain arm must produce 1 fallback")
			require.Equal(t, "provider_b", res.Fallbacks[0].ProviderBackend)
		}
		down.DropStreamObjectBag(streamObjectNonce(h))
	}
	require.True(t, hitChainArm, "chain arm (provider_a primary) must be selected at least once in %d tries", N)
}

// TestBody_sugar_regression verifies sugar path still resolves correctly after
// the resolveRouting refactor.
func TestBody_sugar_regression(t *testing.T) {
	loadSplitConfig(t)
	h, p := runSplitFlow(t, "sugar-model")
	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "single_provider", res.ProviderBackend)
	require.Equal(t, "anthropic", res.ProviderKind)
	require.Equal(t, "sugar-model", res.Model)
	require.Equal(t, "claude-haiku-4-5", res.BackendModel)
	down.DropStreamObjectBag(streamObjectNonce(h))
}

// TestBody_chain_regression verifies chain routing still resolves correctly.
func TestBody_chain_regression(t *testing.T) {
	loadSplitConfig(t)
	h, p := runSplitFlow(t, "chain-model")
	require.Empty(t, h.LocalResponses)
	res, ok := p.Result()
	require.True(t, ok)
	require.Empty(t, res.Err)
	require.Equal(t, "provider_a", res.ProviderBackend)
	require.Equal(t, "chain-model", res.Model)
	require.Len(t, res.Fallbacks, 1)
	require.Equal(t, "provider_b", res.Fallbacks[0].ProviderBackend)
	down.DropStreamObjectBag(streamObjectNonce(h))
}
