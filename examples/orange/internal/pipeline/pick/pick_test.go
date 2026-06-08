package pick

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		in, host, port string
		wantErr        bool
	}{
		{"https://api.openai.com", "api.openai.com", "443", false},
		{"http://localhost:8081", "localhost", "8081", false},
		{"https://us-east5-aiplatform.googleapis.com", "us-east5-aiplatform.googleapis.com", "443", false},
		{"http://example.com", "example.com", "80", false},
		{"", "", "", true},
		{"ftp://x", "", "", true},
		{"example.com", "", "", true},
		{"https://api.openai.com:8443", "api.openai.com", "8443", false},
	}
	for _, tc := range cases {
		h, p, err := splitEndpoint(tc.in)
		if tc.wantErr {
			require.Error(t, err, "splitEndpoint(%q) want err", tc.in)
			continue
		}
		require.NoError(t, err, "splitEndpoint(%q)", tc.in)
		require.Equal(t, tc.host, h, "splitEndpoint(%q) host", tc.in)
		require.Equal(t, tc.port, p, "splitEndpoint(%q) port", tc.in)
	}
}

// pickHostBump backs unique HostPtrs for pickRecordingHandle.
var pickHostBump atomic.Uint64

type pickHostCall struct {
	Specs []up.HostSpec
	Hosts []up.HostPtr
}

type pickRecordingHandle struct {
	mu      sync.Mutex
	calls   []pickHostCall
	removed [][]up.HostPtr
}

func (h *pickRecordingHandle) AddHosts(specs []up.HostSpec) []up.HostPtr {
	ptrs := make([]up.HostPtr, len(specs))
	for i := range specs {
		v := new(uint64)
		*v = pickHostBump.Add(1)
		ptrs[i] = up.HostPtr(unsafe.Pointer(v))
	}
	cp := make([]up.HostSpec, len(specs))
	copy(cp, specs)
	pcp := make([]up.HostPtr, len(ptrs))
	copy(pcp, ptrs)
	h.mu.Lock()
	h.calls = append(h.calls, pickHostCall{Specs: cp, Hosts: pcp})
	h.mu.Unlock()
	return ptrs
}

func (h *pickRecordingHandle) RemoveHosts(ptrs []up.HostPtr) {
	cp := make([]up.HostPtr, len(ptrs))
	copy(cp, ptrs)
	h.mu.Lock()
	h.removed = append(h.removed, cp)
	h.mu.Unlock()
}

func (h *pickRecordingHandle) UpdateHostHealth(_ up.HostPtr, _ up.HostHealth) {}
func (h *pickRecordingHandle) FindHostByAddress(_ string) up.HostPtr          { return nil }
func (h *pickRecordingHandle) PreInitComplete()                               {}
func (h *pickRecordingHandle) Schedule(fn func())                             { fn() }

// addCount returns the total number of hosts added across all AddHosts calls.
func (h *pickRecordingHandle) addCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		n += len(c.Specs)
	}
	return n
}

// removeCount returns the total number of HostPtrs across all RemoveHosts calls.
func (h *pickRecordingHandle) removeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.removed {
		n += len(r)
	}
	return n
}

// allAddedAddrs returns every HostSpec.Address passed to AddHosts.
func (h *pickRecordingHandle) allAddedAddrs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		for _, s := range c.Specs {
			out = append(out, s.Address)
		}
	}
	return out
}

func TestInit_HostnamePopulated(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GROQ_API_KEY", "sk-test")
	yamlBytes, err := os.ReadFile("../match/testdata/match_test.yaml")
	require.NoError(t, err)
	appState := config.NewAppState()
	require.NoError(t, appState.LoadConfig(yamlBytes))

	h := &pickRecordingHandle{}
	c := &cluster{appState: appState}
	c.Init(h)

	h.mu.Lock()
	calls := make([]pickHostCall, len(h.calls))
	copy(calls, h.calls)
	h.mu.Unlock()

	var found bool
	for _, call := range calls {
		for _, spec := range call.Specs {
			if spec.Hostname == "api.openai.com" {
				found = true
				require.Nil(t, spec.Metadata, "openai_direct Metadata should be nil; SNI is now carried by Hostname")
			}
		}
	}
	require.True(t, found, "expected AddHosts call with Hostname=api.openai.com for openai_direct")
}

// --- DNSDiscovery.earliestNext ---

func TestEarliestNext_nilMap(t *testing.T) {
	d := &DNSDiscovery{}
	before := time.Now()
	got := d.earliestNext()
	require.True(t, got.After(before.Add(defaultDNSRefreshInterval-time.Second)),
		"nil map: want ~now+defaultDNSRefreshInterval, got %v", got)
	require.True(t, got.Before(before.Add(defaultDNSRefreshInterval+time.Second)),
		"nil map: result too far in the future")
}

func TestEarliestNext_emptyMap(t *testing.T) {
	d := &DNSDiscovery{nextRefresh: map[provBindingKey]time.Time{}}
	before := time.Now()
	got := d.earliestNext()
	require.True(t, got.After(before.Add(defaultDNSRefreshInterval-time.Second)),
		"empty map: want ~now+defaultDNSRefreshInterval, got %v", got)
}

func TestEarliestNext_singleEntry(t *testing.T) {
	target := time.Now().Add(42 * time.Second)
	d := &DNSDiscovery{
		nextRefresh: map[provBindingKey]time.Time{
			{provider: "only", binding: "default"}: target,
		},
	}
	got := d.earliestNext()
	require.WithinDuration(t, target, got, time.Millisecond)
}

func TestEarliestNext_picksEarliest(t *testing.T) {
	soon := time.Now().Add(5 * time.Second)
	later := time.Now().Add(60 * time.Second)
	d := &DNSDiscovery{
		nextRefresh: map[provBindingKey]time.Time{
			{provider: "a", binding: "default"}: later,
			{provider: "b", binding: "default"}: soon,
			{provider: "c", binding: "default"}: later.Add(time.Minute),
		},
	}
	got := d.earliestNext()
	require.WithinDuration(t, soon, got, time.Millisecond,
		"must return the earliest nextRefresh across all entries")
}

// --- lookupHost ---

func makeTestHostPtr() up.HostPtr {
	v := new(uint64)
	*v = pickHostBump.Add(1)
	return up.HostPtr(unsafe.Pointer(v))
}

func TestLookupHost_errDecision(t *testing.T) {
	c := &cluster{}
	got := c.lookupHost(match.Decision{Err: "orange.model_required"})
	require.Equal(t, "orange.model_required", got.ErrDetail)
	require.Nil(t, got.Host)
}

func TestLookupHost_knownProvider(t *testing.T) {
	ptr := makeTestHostPtr()
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "openai_direct", binding: "default"}: {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{ptr}},
	}
	c.hosts.Store(&m)
	got := c.lookupHost(match.Decision{ProviderBackend: "openai_direct"})
	require.Equal(t, ptr, got.Host)
	require.Empty(t, got.ErrDetail)
}

func TestLookupHost_unknownProvider(t *testing.T) {
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "openai_direct", binding: "default"}: {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{makeTestHostPtr()}},
	}
	c.hosts.Store(&m)
	got := c.lookupHost(match.Decision{ProviderBackend: "anthropic_direct"})
	require.Equal(t, "orange.unknown_upstream", got.ErrDetail)
	require.Nil(t, got.Host)
}

func TestLookupHost_nilHosts(t *testing.T) {
	c := &cluster{}
	// hosts atomic pointer is nil — no map has been published yet.
	got := c.lookupHost(match.Decision{ProviderBackend: "anything"})
	require.Equal(t, "orange.unknown_upstream", got.ErrDetail)
}

func TestLookupHost_errTakesPrecedenceOverHosts(t *testing.T) {
	ptr := makeTestHostPtr()
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "p", binding: "default"}: {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{ptr}},
	}
	c.hosts.Store(&m)
	// Even when a matching host exists, Err must win.
	got := c.lookupHost(match.Decision{ProviderBackend: "p", Err: "orange.stream_terminated"})
	require.Equal(t, "orange.stream_terminated", got.ErrDetail)
	require.Nil(t, got.Host)
}

func TestLookupHost_roundRobins(t *testing.T) {
	ptr1, ptr2 := makeTestHostPtr(), makeTestHostPtr()
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "openai", binding: "default"}: {addrs: []string{"1.2.3.4:443", "5.6.7.8:443"}, ptrs: []up.HostPtr{ptr1, ptr2}},
	}
	c.hosts.Store(&m)
	// rr starts at 0; Add(1)%2: 1→ptr2, 2→ptr1, 3→ptr2 ...
	require.Equal(t, ptr2, c.lookupHost(match.Decision{ProviderBackend: "openai"}).Host)
	require.Equal(t, ptr1, c.lookupHost(match.Decision{ProviderBackend: "openai"}).Host)
	require.Equal(t, ptr2, c.lookupHost(match.Decision{ProviderBackend: "openai"}).Host)
}

func TestLBChooseHost_filterStateProvider(t *testing.T) {
	ptr := makeTestHostPtr()
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "openai", binding: "default"}: {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{ptr}},
	}
	c.hosts.Store(&m)

	l := &lb{
		sel:         nil,
		lookupHostN: c.lookupHostN,
		log:         slog.Default(),
	}
	handle := testutil.NewFilterHandle()
	w := up.NewWriter(handle)
	match.Decision{
		ProviderBackend: "openai",
		ProviderKind:    "openai",
		Model:           "gpt-4o-mini",
		BackendModel:    "gpt-4o-mini",
	}.Apply(w)
	ctx := testutil.NewFakeClusterLBContext(handle)

	got, completion := l.ChooseHost(nil, ctx)
	require.Equal(t, ptr, got)
	require.Nil(t, completion)
}

// --- DNSDiscovery.buildSnapshot / cluster.reconcileSnapshot ---

// loadPickConfig loads a testdata config file with PICK_TEST_KEY set and
// returns a populated AppState for injection into the test cluster.
func loadPickConfig(t *testing.T, path string) *config.AppState {
	t.Helper()
	t.Setenv("PICK_TEST_KEY", "sk-pick-test")
	yamlBytes, err := os.ReadFile(path)
	require.NoError(t, err, "loadPickConfig: read %s", path)
	appState := config.NewAppState()
	require.NoError(t, appState.LoadConfig(yamlBytes), "loadPickConfig: load %s", path)
	return appState
}

// newTestCluster returns a cluster backed by a DNSDiscovery with a stub
// resolveFunc so tests never make real DNS calls.
func newTestCluster(fn func(ctx context.Context, endpoint string) ([]string, time.Duration, error)) *cluster {
	return &cluster{
		disc:   &DNSDiscovery{resolveFunc: fn, logger: slog.Default().With("component", "orange/pick/test")},
		logger: slog.Default().With("component", "orange/pick/test"),
	}
}

// newTestClusterWithState returns a test cluster with a pre-loaded AppState.
func newTestClusterWithState(appState *config.AppState, fn func(ctx context.Context, endpoint string) ([]string, time.Duration, error)) *cluster {
	return &cluster{
		appState: appState,
		disc:     &DNSDiscovery{resolveFunc: fn, logger: slog.Default().With("component", "orange/pick/test"), appState: appState},
		logger:   slog.Default().With("component", "orange/pick/test"),
	}
}

// resolveAndApply is a test helper that runs buildSnapshot + reconcileSnapshot
// in sequence, mirroring the production Init path.
func resolveAndApply(c *cluster, h up.ClusterHandle) {
	snap := c.disc.buildSnapshot(context.Background())
	c.reconcileSnapshot(h, snap)
}

// fixedResolve returns a resolveFunc that always resolves any endpoint to the
// given addrs with the given TTL.
func fixedResolve(addr string, ttl time.Duration) func(context.Context, string) ([]string, time.Duration, error) {
	return func(_ context.Context, _ string) ([]string, time.Duration, error) {
		return []string{addr}, ttl, nil
	}
}

// errResolve returns a resolveFunc that always returns an error.
func errResolve(msg string) func(context.Context, string) ([]string, time.Duration, error) {
	return func(_ context.Context, _ string) ([]string, time.Duration, error) {
		return nil, 0, errors.New(msg)
	}
}

// TestResolveAll_newProvider verifies that a provider that has no prior entry
// is added via AddHosts and marked healthy.
func TestResolveAll_newProvider(t *testing.T) {
	appState := loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))

	resolveAndApply(c, h)

	require.Equal(t, 2, h.addCount(), "both providers must be added on first resolveAll")
	require.Equal(t, 0, h.removeCount(), "no removes on first pass")
}

// TestResolveAll_addrUnchanged verifies that when DNS returns the same address
// the existing HostPtr is preserved (no RemoveHosts/AddHosts churn).
func TestResolveAll_addrUnchanged(t *testing.T) {
	appState := loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))

	resolveAndApply(c, h)
	addCallsAfterFirst := h.addCount()
	require.Equal(t, 2, addCallsAfterFirst)

	// Capture the ptrs stored after the first pass.
	first := c.hosts.Load()
	ptrA := (*first)[provBindingKey{provider: "provider_a", binding: "default"}].ptrs[0]
	ptrB := (*first)[provBindingKey{provider: "provider_b", binding: "default"}].ptrs[0]
	require.NotNil(t, ptrA)
	require.NotNil(t, ptrB)

	resolveAndApply(c, h)

	// No new AddHosts or RemoveHosts calls when addr is unchanged.
	require.Equal(t, addCallsAfterFirst, h.addCount(), "no new AddHosts when addr unchanged")
	require.Equal(t, 0, h.removeCount(), "no RemoveHosts when addr unchanged")

	// The HostPtrs must be the same objects — no re-registration.
	second := c.hosts.Load()
	require.Equal(t, ptrA, (*second)[provBindingKey{provider: "provider_a", binding: "default"}].ptrs[0], "provider_a ptr must be preserved")
	require.Equal(t, ptrB, (*second)[provBindingKey{provider: "provider_b", binding: "default"}].ptrs[0], "provider_b ptr must be preserved")
}

// TestResolveAll_addrChanged verifies that when DNS returns a new address for an
// existing provider the old host is removed and the new one added.
func TestResolveAll_addrChanged(t *testing.T) {
	appState := loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))
	resolveAndApply(c, h)
	require.Equal(t, 2, h.addCount())

	oldPtrA := (*c.hosts.Load())[provBindingKey{provider: "provider_a", binding: "default"}].ptrs[0]

	// Simulate IP change for both providers.
	c.disc.resolveFunc = fixedResolve("9.9.9.9:443", 60*time.Second)
	resolveAndApply(c, h)

	require.Equal(t, 4, h.addCount(), "two new AddHosts calls for changed IPs")
	require.Equal(t, 2, h.removeCount(), "two RemoveHosts calls for stale IPs")

	// The new ptr for provider_a must differ from the old one.
	newPtrA := (*c.hosts.Load())[provBindingKey{provider: "provider_a", binding: "default"}].ptrs[0]
	require.NotEqual(t, oldPtrA, newPtrA, "HostPtr must change when addr changes")
}

// TestResolveAll_resolveFailKeepsOld verifies that a transient DNS failure does
// not evict a previously healthy host.
func TestResolveAll_resolveFailKeepsOld(t *testing.T) {
	appState := loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))
	resolveAndApply(c, h)
	require.Equal(t, 2, h.addCount())

	oldHosts := c.hosts.Load()
	ptrA := (*oldHosts)[provBindingKey{provider: "provider_a", binding: "default"}].ptrs[0]

	// Next resolve fails for all providers.
	c.disc.resolveFunc = errResolve("simulated DNS failure")
	now := time.Now()
	resolveAndApply(c, h)

	// No additional AddHosts or RemoveHosts.
	require.Equal(t, 2, h.addCount(), "no new AddHosts on DNS failure")
	require.Equal(t, 0, h.removeCount(), "no RemoveHosts on DNS failure — keep healthy host")

	// The preserved hostEntry must still hold the old HostPtr.
	m := c.hosts.Load()
	require.NotNil(t, m)
	entry := (*m)[provBindingKey{provider: "provider_a", binding: "default"}]
	require.Equal(t, ptrA, entry.ptrs[0], "HostPtr must be preserved after DNS failure")

	// DNSDiscovery must have scheduled a retry in the [minTTLFloor, 2*minTTLFloor) window.
	key := provBindingKey{provider: "provider_a", binding: "default"}
	nr := c.disc.nextRefresh[key]
	require.True(t, !nr.Before(now.Add(minTTLFloor-time.Second)),
		"nextRefresh must be at least ~now+minTTLFloor after DNS failure, got %v", nr)
	require.True(t, nr.Before(now.Add(2*minTTLFloor+time.Second)),
		"nextRefresh must be less than ~now+2*minTTLFloor after DNS failure, got %v", nr)
}

// TestResolveAll_providerDeleted verifies that a provider removed from config is
// evicted via RemoveHosts on the next reconcile cycle.
func TestResolveAll_providerDeleted(t *testing.T) {
	appState := loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))
	resolveAndApply(c, h)
	require.Equal(t, 2, h.addCount(), "both providers added on first pass")

	// Switch config to only provider_a.
	t.Setenv("PICK_TEST_KEY", "sk-pick-test")
	yamlBytes, err := os.ReadFile("testdata/one_provider.yaml")
	require.NoError(t, err)
	require.NoError(t, appState.LoadConfig(yamlBytes))

	resolveAndApply(c, h)

	// provider_b must have been removed; provider_a survives.
	require.Equal(t, 1, h.removeCount(), "provider_b must be evicted when deleted from config")
	m := c.hosts.Load()
	require.NotEmpty(t, (*m)[provBindingKey{provider: "provider_a", binding: "default"}].ptrs, "provider_a must survive config change")
	_, hasB := (*m)[provBindingKey{provider: "provider_b", binding: "default"}]
	require.False(t, hasB, "provider_b must be absent from hosts map after config change")
}

// --- pickAddrs ---

func ips(raw ...string) []net.IPAddr {
	out := make([]net.IPAddr, len(raw))
	for i, s := range raw {
		out[i] = net.IPAddr{IP: net.ParseIP(s)}
	}
	return out
}

func TestPickAddrs_sortStable(t *testing.T) {
	// DNS can return the same two IPs in alternating order each refresh;
	// pickAddrs must produce the same sorted slice regardless of input order.
	order1 := ips("172.66.0.243", "162.159.140.245")
	order2 := ips("162.159.140.245", "172.66.0.243")
	got1 := pickAddrs(order1, "443")
	got2 := pickAddrs(order2, "443")
	require.Equal(t, got1, got2, "same IPs in different order must produce the same sorted slice")
	require.Equal(t, []string{"162.159.140.245:443", "172.66.0.243:443"}, got1,
		"must return all IPv4 addresses sorted lexicographically")
}

func TestPickAddrs_prefersIPv4(t *testing.T) {
	addrs := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("1.2.3.4")},
	}
	got := pickAddrs(addrs, "443")
	require.Equal(t, []string{"1.2.3.4:443"}, got, "only IPv4 addresses returned when available")
}

func TestPickAddrs_fallbackToIPv6(t *testing.T) {
	addrs := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::2")},
		{IP: net.ParseIP("2001:db8::1")},
	}
	got := pickAddrs(addrs, "443")
	require.Len(t, got, 2, "all IPv6 addresses returned when no IPv4 available")
	require.Contains(t, got[0], "2001:db8::1", "IPv6 addresses must be sorted")
}

func TestPickAddrs_empty(t *testing.T) {
	require.Empty(t, pickAddrs(nil, "443"))
}

func TestPickAddrs_single(t *testing.T) {
	require.Equal(t, []string{"10.0.0.1:8080"}, pickAddrs(ips("10.0.0.1"), "8080"))
}

// TestResolveAll_multipleAddrs verifies that all IPs returned by DNS are
// registered as hosts so lookupHost can round-robin among them.
func TestResolveAll_multipleAddrs(t *testing.T) {
	appState := loadPickConfig(t, "testdata/one_provider.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, func(_ context.Context, _ string) ([]string, time.Duration, error) {
		return []string{"1.2.3.4:443", "5.6.7.8:443"}, 60 * time.Second, nil
	})

	resolveAndApply(c, h)

	require.Equal(t, 2, h.addCount(), "all IPs for provider_a must be registered")
	require.ElementsMatch(t, []string{"1.2.3.4:443", "5.6.7.8:443"}, h.allAddedAddrs())
	entry := (*c.hosts.Load())[provBindingKey{provider: "provider_a", binding: "default"}]
	require.Len(t, entry.ptrs, 2)
	require.Len(t, entry.addrs, 2)
}

// TestResolveAll_ttlFloor verifies that short TTLs are clamped to minTTLFloor
// in DNSDiscovery's scheduled next-refresh time.
func TestResolveAll_ttlFloor(t *testing.T) {
	appState := loadPickConfig(t, "testdata/one_provider.yaml")

	h := &pickRecordingHandle{}
	// Return a pathologically short TTL (1s < minTTLFloor).
	c := newTestClusterWithState(appState, func(_ context.Context, _ string) ([]string, time.Duration, error) {
		return []string{"1.2.3.4:443"}, time.Second, nil
	})
	resolveAndApply(c, h)

	key := provBindingKey{provider: "provider_a", binding: "default"}
	nr := c.disc.nextRefresh[key]
	require.True(t, nr.After(time.Now().Add(minTTLFloor-time.Second)),
		"DNSDiscovery nextRefresh must be at least minTTLFloor ahead even for short-TTL DNS responses")
}

// --- bindings ----------------------------------------------------------------

// TestResolveAll_twoBindings verifies that a provider with two bindings
// contributes two independent DNS-refresh entries to the hosts map.
func TestResolveAll_twoBindings(t *testing.T) {
	appState := loadPickConfig(t, "testdata/bindings_provider.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))

	resolveAndApply(c, h)

	require.Equal(t, 2, h.addCount(), "both bindings must produce separate AddHosts calls")
	require.Equal(t, 0, h.removeCount(), "no removes on first pass")

	m := c.hosts.Load()
	_, hasEast := (*m)[provBindingKey{provider: "anthropic", binding: "us-east"}]
	_, hasWest := (*m)[provBindingKey{provider: "anthropic", binding: "us-west"}]
	require.True(t, hasEast, "us-east binding must be in hosts map")
	require.True(t, hasWest, "us-west binding must be in hosts map")
}

// TestLookupHost_namedBinding verifies that a Decision with a binding lands
// on the correct entry rather than the implicit default.
func TestLookupHost_namedBinding(t *testing.T) {
	eastPtr := makeTestHostPtr()
	westPtr := makeTestHostPtr()
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "anthropic", binding: "us-east"}: {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{eastPtr}},
		{provider: "anthropic", binding: "us-west"}: {addrs: []string{"5.6.7.8:443"}, ptrs: []up.HostPtr{westPtr}},
	}
	c.hosts.Store(&m)

	gotEast := c.lookupHost(match.Decision{ProviderBackend: "anthropic", Binding: "us-east"})
	require.Equal(t, eastPtr, gotEast.Host, "us-east binding must route to east host")
	require.Empty(t, gotEast.ErrDetail)

	gotWest := c.lookupHost(match.Decision{ProviderBackend: "anthropic", Binding: "us-west"})
	require.Equal(t, westPtr, gotWest.Host, "us-west binding must route to west host")
	require.Empty(t, gotWest.ErrDetail)
}

// TestLookupHost_missingBinding verifies that an unknown binding name
// returns orange.unknown_upstream.
func TestLookupHost_missingBinding(t *testing.T) {
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "anthropic", binding: "us-east"}: {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{makeTestHostPtr()}},
	}
	c.hosts.Store(&m)

	got := c.lookupHost(match.Decision{ProviderBackend: "anthropic", Binding: "us-north"})
	require.Equal(t, "orange.unknown_upstream", got.ErrDetail)
	require.Nil(t, got.Host)
}

// TestLookupHost_emptyBindingNormalizesToDefault verifies that an empty
// Binding field is treated as "default" when no explicit binding is set.
func TestLookupHost_emptyBindingNormalizesToDefault(t *testing.T) {
	ptr := makeTestHostPtr()
	c := &cluster{}
	m := map[provBindingKey]*hostEntry{
		{provider: "openai", binding: "default"}: {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{ptr}},
	}
	c.hosts.Store(&m)

	got := c.lookupHost(match.Decision{ProviderBackend: "openai"}) // Binding is ""
	require.Equal(t, ptr, got.Host, "empty binding must normalize to 'default'")
	require.Empty(t, got.ErrDetail)
}

// TestResolveAll_endpointOnlyProvider verifies that a provider with only an
// endpoint (no explicit bindings) is registered under the "default" binding key.
func TestResolveAll_endpointOnlyProvider(t *testing.T) {
	appState := loadPickConfig(t, "testdata/one_provider.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))
	resolveAndApply(c, h)

	m := c.hosts.Load()
	_, hasDefault := (*m)[provBindingKey{provider: "provider_a", binding: "default"}]
	require.True(t, hasDefault, "endpoint-only provider must be stored under 'default' binding")
}

// allAddedHostnames returns every HostSpec.Hostname passed to AddHosts.
func (h *pickRecordingHandle) allAddedHostnames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		for _, s := range c.Specs {
			out = append(out, s.Hostname)
		}
	}
	return out
}

// TestResolveAll_unreferencedBindingsRegistered verifies that every
// (provider, binding) in the catalog is registered as a host even when no
// model entry in the config references that binding.
func TestResolveAll_unreferencedBindingsRegistered(t *testing.T) {
	appState := loadPickConfig(t, "testdata/unreferenced_binding.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))

	resolveAndApply(c, h)

	// us-east is referenced by a model; us-west and us-central are not.
	// All three must still be registered.
	require.Equal(t, 3, h.addCount(), "all three bindings must produce separate AddHosts calls regardless of model references")
	require.Equal(t, 0, h.removeCount())

	m := c.hosts.Load()
	_, hasEast := (*m)[provBindingKey{provider: "anthropic", binding: "us-east"}]
	_, hasWest := (*m)[provBindingKey{provider: "anthropic", binding: "us-west"}]
	_, hasCentral := (*m)[provBindingKey{provider: "anthropic", binding: "us-central"}]
	require.True(t, hasEast, "us-east must be in the hosts map")
	require.True(t, hasWest, "us-west must be in the hosts map even though no model references it")
	require.True(t, hasCentral, "us-central must be in the hosts map even though no model references it")
}

// TestResolveAll_bindingRemovedEvictsOnlyThatBinding verifies that a config
// reload that drops one binding calls RemoveHosts only for that binding while
// leaving the surviving binding's HostPtr untouched.
func TestResolveAll_bindingRemovedEvictsOnlyThatBinding(t *testing.T) {
	appState := loadPickConfig(t, "testdata/bindings_provider.yaml") // us-east + us-west

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))
	resolveAndApply(c, h)
	require.Equal(t, 2, h.addCount(), "both bindings added on first pass")
	require.Equal(t, 0, h.removeCount())

	first := c.hosts.Load()
	eastPtr := (*first)[provBindingKey{provider: "anthropic", binding: "us-east"}].ptrs[0]
	require.NotNil(t, eastPtr)

	// Reload: us-west is gone.
	t.Setenv("PICK_TEST_KEY", "sk-pick-test")
	yamlBytes, err := os.ReadFile("testdata/one_binding.yaml")
	require.NoError(t, err)
	require.NoError(t, appState.LoadConfig(yamlBytes))

	resolveAndApply(c, h)

	require.Equal(t, 1, h.removeCount(), "only us-west must be evicted via RemoveHosts")

	m := c.hosts.Load()
	_, hasWest := (*m)[provBindingKey{provider: "anthropic", binding: "us-west"}]
	require.False(t, hasWest, "us-west must be absent after it was removed from config")

	entry, hasEast := (*m)[provBindingKey{provider: "anthropic", binding: "us-east"}]
	require.True(t, hasEast, "us-east must survive the config reload")
	require.Equal(t, eastPtr, entry.ptrs[0], "us-east HostPtr must be preserved — no spurious re-registration")
}

// TestApplyResolved_hostnameSetForSNI verifies that reconcileSnapshot passes the
// correct Hostname (extracted from the binding's endpoint URL) in every
// HostSpec. This is the pick layer's contribution to TLS SNI: the custom
// Envoy build uses HostSpec.Hostname as the SNI value (auto_host_sni) when
// connecting to each runtime-added host.
func TestApplyResolved_hostnameSetForSNI(t *testing.T) {
	appState := loadPickConfig(t, "testdata/bindings_provider.yaml")

	h := &pickRecordingHandle{}
	c := newTestClusterWithState(appState, fixedResolve("1.2.3.4:443", 60*time.Second))
	resolveAndApply(c, h)

	require.Equal(t, 2, h.addCount())
	hostnames := h.allAddedHostnames()
	require.ElementsMatch(t,
		[]string{"api.anthropic.com", "api-west.anthropic.com"},
		hostnames,
		"HostSpec.Hostname must be the hostname from the binding endpoint so auto_host_sni sets the correct TLS SNI",
	)
}

// TestLookupWithTTL_ipLiteral verifies that an IP address string is returned
// directly without a DNS query, using the 24h synthetic TTL.
func TestLookupWithTTL_ipLiteral(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		input string
		want  string
	}{
		{"1.2.3.4", "1.2.3.4"},
		{"127.0.0.1", "127.0.0.1"},
		{"::1", "::1"},
	}
	for _, tc := range cases {
		addrs, ttl, err := lookupWithTTL(ctx, tc.input)
		require.NoError(t, err, "lookupWithTTL(%q) should not error for IP literal", tc.input)
		require.Len(t, addrs, 1, "lookupWithTTL(%q) must return exactly one address", tc.input)
		require.Equal(t, tc.want, addrs[0].IP.String(), "lookupWithTTL(%q) address mismatch", tc.input)
		require.Equal(t, 24*time.Hour, ttl, "IP literal TTL must be 24h")
	}
}

// TestShutdown_cancelsRefreshContext verifies that Shutdown cancels the refresh
// goroutine context and calls done().
func TestShutdown_cancelsRefreshContext(t *testing.T) {
	c := &cluster{
		logger: slog.Default().With("component", "orange/pick/test"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.stopRefresh = cancel

	done := make(chan struct{})
	c.Shutdown(nil, func() { close(done) })

	select {
	case <-ctx.Done():
		// refresh context was cancelled — correct
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel the refresh context within 1s")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not call done() within 1s")
	}
}
