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

	orangecfg "github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
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
	t.Setenv(orangecfg.EnvVar, "../match/testdata/match_test.yaml")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GROQ_API_KEY", "sk-test")
	orangecfg.MustReload()
	t.Cleanup(func() {
		os.Unsetenv(orangecfg.EnvVar)
		orangecfg.MustReload()
	})

	h := &pickRecordingHandle{}
	c := &cluster{}
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

// --- earliestNextRefresh ---

func TestEarliestNextRefresh_nilMap(t *testing.T) {
	c := &cluster{}
	before := time.Now()
	got := c.earliestNextRefresh()
	// Must be approximately now + defaultDNSRefreshInterval.
	require.True(t, got.After(before.Add(defaultDNSRefreshInterval-time.Second)),
		"nil map: want ~now+defaultDNSRefreshInterval, got %v", got)
	require.True(t, got.Before(before.Add(defaultDNSRefreshInterval+time.Second)),
		"nil map: result too far in the future")
}

func TestEarliestNextRefresh_emptyMap(t *testing.T) {
	c := &cluster{}
	m := map[string]*resolvedUpstream{}
	c.hosts.Store(&m)
	before := time.Now()
	got := c.earliestNextRefresh()
	require.True(t, got.After(before.Add(defaultDNSRefreshInterval-time.Second)),
		"empty map: want ~now+defaultDNSRefreshInterval, got %v", got)
}

func TestEarliestNextRefresh_singleEntry(t *testing.T) {
	c := &cluster{}
	target := time.Now().Add(42 * time.Second)
	m := map[string]*resolvedUpstream{
		"only": {nextRefresh: target},
	}
	c.hosts.Store(&m)
	got := c.earliestNextRefresh()
	require.WithinDuration(t, target, got, time.Millisecond)
}

func TestEarliestNextRefresh_picksEarliest(t *testing.T) {
	c := &cluster{}
	soon := time.Now().Add(5 * time.Second)
	later := time.Now().Add(60 * time.Second)
	m := map[string]*resolvedUpstream{
		"a": {nextRefresh: later},
		"b": {nextRefresh: soon},
		"c": {nextRefresh: later.Add(time.Minute)},
	}
	c.hosts.Store(&m)
	got := c.earliestNextRefresh()
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
	m := map[string]*resolvedUpstream{
		"openai_direct": {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{ptr}},
	}
	c.hosts.Store(&m)
	got := c.lookupHost(match.Decision{Provider: "openai_direct"})
	require.Equal(t, ptr, got.Host)
	require.Empty(t, got.ErrDetail)
}

func TestLookupHost_unknownProvider(t *testing.T) {
	c := &cluster{}
	m := map[string]*resolvedUpstream{
		"openai_direct": {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{makeTestHostPtr()}},
	}
	c.hosts.Store(&m)
	got := c.lookupHost(match.Decision{Provider: "anthropic_direct"})
	require.Equal(t, "orange.unknown_upstream", got.ErrDetail)
	require.Nil(t, got.Host)
}

func TestLookupHost_nilHosts(t *testing.T) {
	c := &cluster{}
	// hosts atomic pointer is nil — no map has been published yet.
	got := c.lookupHost(match.Decision{Provider: "anything"})
	require.Equal(t, "orange.unknown_upstream", got.ErrDetail)
}

func TestLookupHost_errTakesPrecedenceOverHosts(t *testing.T) {
	ptr := makeTestHostPtr()
	c := &cluster{}
	m := map[string]*resolvedUpstream{
		"p": {addrs: []string{"1.2.3.4:443"}, ptrs: []up.HostPtr{ptr}},
	}
	c.hosts.Store(&m)
	// Even when a matching host exists, Err must win.
	got := c.lookupHost(match.Decision{Provider: "p", Err: "orange.stream_terminated"})
	require.Equal(t, "orange.stream_terminated", got.ErrDetail)
	require.Nil(t, got.Host)
}

func TestLookupHost_roundRobins(t *testing.T) {
	ptr1, ptr2 := makeTestHostPtr(), makeTestHostPtr()
	c := &cluster{}
	m := map[string]*resolvedUpstream{
		"openai": {addrs: []string{"1.2.3.4:443", "5.6.7.8:443"}, ptrs: []up.HostPtr{ptr1, ptr2}},
	}
	c.hosts.Store(&m)
	// rr starts at 0; Add(1)%2: 1→ptr2, 2→ptr1, 3→ptr2 ...
	require.Equal(t, ptr2, c.lookupHost(match.Decision{Provider: "openai"}).Host)
	require.Equal(t, ptr1, c.lookupHost(match.Decision{Provider: "openai"}).Host)
	require.Equal(t, ptr2, c.lookupHost(match.Decision{Provider: "openai"}).Host)
}

// --- resolveAddrs / applyResolved reconciliation ---

// loadPickConfig loads a testdata config file with PICK_TEST_KEY set.
func loadPickConfig(t *testing.T, path string) {
	t.Helper()
	t.Setenv("PICK_TEST_KEY", "sk-pick-test")
	t.Setenv(orangecfg.EnvVar, path)
	orangecfg.MustReload()
	t.Cleanup(func() {
		os.Unsetenv(orangecfg.EnvVar)
		orangecfg.MustReload()
	})
}

// newTestCluster returns a cluster with a stub resolveFunc and an initialised logger.
func newTestCluster(fn func(ctx context.Context, endpoint string) ([]string, time.Duration, error)) *cluster {
	c := &cluster{
		resolveFunc: fn,
		logger:      slog.Default().With("component", "orange/pick/test"),
	}
	return c
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
	loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestCluster(fixedResolve("1.2.3.4:443", 60*time.Second))

	c.applyResolved(h, c.resolveAddrs(context.Background()))

	require.Equal(t, 2, h.addCount(), "both providers must be added on first resolveAll")
	require.Equal(t, 0, h.removeCount(), "no removes on first pass")
}

// TestResolveAll_addrUnchanged verifies that when DNS returns the same address
// the existing HostPtr is preserved (no RemoveHosts/AddHosts churn).
func TestResolveAll_addrUnchanged(t *testing.T) {
	loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestCluster(fixedResolve("1.2.3.4:443", 60*time.Second))

	c.applyResolved(h, c.resolveAddrs(context.Background()))
	addCallsAfterFirst := h.addCount()
	require.Equal(t, 2, addCallsAfterFirst)

	// Capture the ptrs stored after the first pass.
	first := c.hosts.Load()
	ptrA := (*first)["provider_a"].ptrs[0]
	ptrB := (*first)["provider_b"].ptrs[0]
	require.NotNil(t, ptrA)
	require.NotNil(t, ptrB)

	c.applyResolved(h, c.resolveAddrs(context.Background()))

	// No new AddHosts or RemoveHosts calls when addr is unchanged.
	require.Equal(t, addCallsAfterFirst, h.addCount(), "no new AddHosts when addr unchanged")
	require.Equal(t, 0, h.removeCount(), "no RemoveHosts when addr unchanged")

	// The HostPtrs must be the same objects — no re-registration.
	second := c.hosts.Load()
	require.Equal(t, ptrA, (*second)["provider_a"].ptrs[0], "provider_a ptr must be preserved")
	require.Equal(t, ptrB, (*second)["provider_b"].ptrs[0], "provider_b ptr must be preserved")
}

// TestResolveAll_addrChanged verifies that when DNS returns a new address for an
// existing provider the old host is removed and the new one added.
func TestResolveAll_addrChanged(t *testing.T) {
	loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestCluster(fixedResolve("1.2.3.4:443", 60*time.Second))
	c.applyResolved(h, c.resolveAddrs(context.Background()))
	require.Equal(t, 2, h.addCount())

	oldPtrA := (*c.hosts.Load())["provider_a"].ptrs[0]

	// Simulate IP change for both providers.
	c.resolveFunc = fixedResolve("9.9.9.9:443", 60*time.Second)
	c.applyResolved(h, c.resolveAddrs(context.Background()))

	require.Equal(t, 4, h.addCount(), "two new AddHosts calls for changed IPs")
	require.Equal(t, 2, h.removeCount(), "two RemoveHosts calls for stale IPs")

	// The new ptr for provider_a must differ from the old one.
	newPtrA := (*c.hosts.Load())["provider_a"].ptrs[0]
	require.NotEqual(t, oldPtrA, newPtrA, "HostPtr must change when addr changes")
}

// TestResolveAll_resolveFailKeepsOld verifies that a transient DNS failure does
// not evict a previously healthy host.
func TestResolveAll_resolveFailKeepsOld(t *testing.T) {
	loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestCluster(fixedResolve("1.2.3.4:443", 60*time.Second))
	c.applyResolved(h, c.resolveAddrs(context.Background()))
	require.Equal(t, 2, h.addCount())

	oldHosts := c.hosts.Load()
	ptrA := (*oldHosts)["provider_a"].ptrs[0]

	// Next resolve fails for all providers.
	c.resolveFunc = errResolve("simulated DNS failure")
	c.applyResolved(h, c.resolveAddrs(context.Background()))

	// No additional AddHosts or RemoveHosts.
	require.Equal(t, 2, h.addCount(), "no new AddHosts on DNS failure")
	require.Equal(t, 0, h.removeCount(), "no RemoveHosts on DNS failure — keep healthy host")

	// The preserved entry must have nextRefresh reset to ~now+minTTLFloor.
	m := c.hosts.Load()
	require.NotNil(t, m)
	entry := (*m)["provider_a"]
	require.Equal(t, ptrA, entry.ptrs[0], "HostPtr must be preserved after DNS failure")
	require.WithinDuration(t, time.Now().Add(minTTLFloor), entry.nextRefresh, 2*time.Second,
		"nextRefresh must be set to ~now+minTTLFloor after DNS failure")
}

// TestResolveAll_providerDeleted verifies that a provider removed from config is
// evicted via RemoveHosts on the next reconcile cycle.
func TestResolveAll_providerDeleted(t *testing.T) {
	loadPickConfig(t, "testdata/two_providers.yaml")

	h := &pickRecordingHandle{}
	c := newTestCluster(fixedResolve("1.2.3.4:443", 60*time.Second))
	c.applyResolved(h, c.resolveAddrs(context.Background()))
	require.Equal(t, 2, h.addCount(), "both providers added on first pass")

	// Switch config to only provider_a.
	t.Setenv(orangecfg.EnvVar, "testdata/one_provider.yaml")
	orangecfg.MustReload()

	c.applyResolved(h, c.resolveAddrs(context.Background()))

	// provider_b must have been removed; provider_a survives.
	require.Equal(t, 1, h.removeCount(), "provider_b must be evicted when deleted from config")
	m := c.hosts.Load()
	require.NotEmpty(t, (*m)["provider_a"].ptrs, "provider_a must survive config change")
	_, hasB := (*m)["provider_b"]
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
	loadPickConfig(t, "testdata/one_provider.yaml")

	h := &pickRecordingHandle{}
	c := newTestCluster(func(_ context.Context, _ string) ([]string, time.Duration, error) {
		return []string{"1.2.3.4:443", "5.6.7.8:443"}, 60 * time.Second, nil
	})

	c.applyResolved(h, c.resolveAddrs(context.Background()))

	require.Equal(t, 2, h.addCount(), "all IPs for provider_a must be registered")
	require.ElementsMatch(t, []string{"1.2.3.4:443", "5.6.7.8:443"}, h.allAddedAddrs())
	entry := (*c.hosts.Load())["provider_a"]
	require.Len(t, entry.ptrs, 2)
	require.Len(t, entry.addrs, 2)
}

// TestResolveAll_ttlFloor verifies that short TTLs are clamped to minTTLFloor.
func TestResolveAll_ttlFloor(t *testing.T) {
	loadPickConfig(t, "testdata/one_provider.yaml")

	h := &pickRecordingHandle{}
	// Return a pathologically short TTL (1s < minTTLFloor).
	c := newTestCluster(func(_ context.Context, _ string) ([]string, time.Duration, error) {
		return []string{"1.2.3.4:443"}, time.Second, nil
	})
	c.applyResolved(h, c.resolveAddrs(context.Background()))

	m := c.hosts.Load()
	require.NotNil(t, m)
	entry := (*m)["provider_a"]
	require.True(t, entry.nextRefresh.After(time.Now().Add(minTTLFloor-time.Second)),
		"nextRefresh must be at least minTTLFloor ahead even for short-TTL DNS responses")
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
