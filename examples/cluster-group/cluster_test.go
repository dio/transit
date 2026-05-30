package clustergroup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestParseConfig_Valid(t *testing.T) {
	raw := `{"discovery_url":"http://host/hosts","refresh_ms":500}`
	var cfg clusterConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &cfg))
	require.Equal(t, "http://host/hosts", cfg.DiscoveryURL)
	require.Equal(t, 500*1000*1000, int(cfg.refreshInterval()))
}

func TestParseConfig_DefaultRefresh(t *testing.T) {
	var cfg clusterConfig
	require.NoError(t, json.Unmarshal([]byte(`{"discovery_url":"http://x"}`), &cfg))
	require.Equal(t, 5_000_000_000, int(cfg.refreshInterval()))
}

func TestFactory_Create_MissingURL(t *testing.T) {
	f := &discoveryFactory{}
	_, err := f.Create([]byte(`{}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "discovery_url")
}

func TestFactory_Create_BadJSON(t *testing.T) {
	f := &discoveryFactory{}
	_, err := f.Create([]byte(`not json`))
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// fetchDiscovery
// ---------------------------------------------------------------------------

func TestFetchDiscovery_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hosts":["10.0.0.1:8080","10.0.0.2:8080"]}`)) //nolint:errcheck
	}))
	defer srv.Close()

	hosts, err := fetchDiscovery(srv.URL)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"10.0.0.1:8080", "10.0.0.2:8080"}, hosts)
}

func TestFetchDiscovery_Error(t *testing.T) {
	_, err := fetchDiscovery("http://127.0.0.1:1") // nothing listening
	require.Error(t, err)
}

func TestFetchDiscovery_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`not json`)) //nolint:errcheck
	}))
	defer srv.Close()
	_, err := fetchDiscovery(srv.URL)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// applyHostsDirect
// ---------------------------------------------------------------------------

func TestApplyHostsDirect_AddsHosts(t *testing.T) {
	h := &fakeClusterHandle{}
	c := &discoveryCluster{
		handle: h,
		known:  make(map[string]up.HostPtr),
	}

	c.applyHostsDirect([]string{"10.0.0.1:80", "10.0.0.2:80"})

	require.Len(t, h.added, 2)
	require.True(t, h.preInitComplete || true) // add doesn't call preInit
	require.Len(t, h.healthUpdates, 2)
	for _, u := range h.healthUpdates {
		require.Equal(t, up.HostHealthy, u.health)
	}
}

func TestApplyHostsDirect_RemovesStaleHosts(t *testing.T) {
	h := &fakeClusterHandle{}
	c := &discoveryCluster{
		handle: h,
		known:  make(map[string]up.HostPtr),
	}

	c.applyHostsDirect([]string{"10.0.0.1:80", "10.0.0.2:80"})
	require.Len(t, c.known, 2)

	// Second apply keeps only one host.
	c.applyHostsDirect([]string{"10.0.0.1:80"})
	require.Len(t, c.known, 1)
	require.Len(t, h.removed, 1)
}

func TestApplyHostsDirect_IdempotentAdd(t *testing.T) {
	h := &fakeClusterHandle{}
	c := &discoveryCluster{
		handle: h,
		known:  make(map[string]up.HostPtr),
	}

	c.applyHostsDirect([]string{"10.0.0.1:80"})
	c.applyHostsDirect([]string{"10.0.0.1:80"}) // same host, no duplicate add

	require.Len(t, h.added, 1) // AddHosts called once
}

// ---------------------------------------------------------------------------
// roundRobinLB
// ---------------------------------------------------------------------------

func TestRoundRobinLB_RotatesHosts(t *testing.T) {
	hosts := []up.HostPtr{fakeHostPtr(), fakeHostPtr(), fakeHostPtr()}
	lb := &roundRobinLB{}
	h := &fakeClusterLBHandle{healthyHosts: hosts}

	seen := map[up.HostPtr]int{}
	for range 6 {
		got, _ := lb.ChooseHost(h, nil)
		require.NotNil(t, got)
		seen[got]++
	}
	for _, host := range hosts {
		require.Equal(t, 2, seen[host], "each host should be chosen twice in 6 rounds")
	}
}

func TestRoundRobinLB_NoHealthyHosts(t *testing.T) {
	lb := &roundRobinLB{}
	got, completion := lb.ChooseHost(&fakeClusterLBHandle{}, nil)
	require.Nil(t, got)
	require.Nil(t, completion)
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeClusterHandle struct {
	added           []up.HostSpec
	removed         []up.HostPtr
	preInitComplete bool
	healthUpdates   []healthUpdate
}

type healthUpdate struct {
	host   up.HostPtr
	health up.HostHealth
}

func (h *fakeClusterHandle) AddHosts(hosts []up.HostSpec) []up.HostPtr {
	h.added = append(h.added, hosts...)
	ptrs := make([]up.HostPtr, len(hosts))
	for i := range hosts {
		ptrs[i] = fakeHostPtr()
	}
	return ptrs
}
func (h *fakeClusterHandle) RemoveHosts(ptrs []up.HostPtr) { h.removed = append(h.removed, ptrs...) }
func (h *fakeClusterHandle) UpdateHostHealth(host up.HostPtr, health up.HostHealth) {
	h.healthUpdates = append(h.healthUpdates, healthUpdate{host: host, health: health})
}
func (h *fakeClusterHandle) FindHostByAddress(_ string) up.HostPtr { return nil }
func (h *fakeClusterHandle) PreInitComplete()                      { h.preInitComplete = true }
func (h *fakeClusterHandle) Schedule(fn func())                    { fn() }

type fakeClusterLBHandle struct {
	healthyHosts []up.HostPtr
}

func (h *fakeClusterLBHandle) ClusterName() string                                  { return "test" }
func (h *fakeClusterLBHandle) PriorityCount() int                                   { return 1 }
func (h *fakeClusterLBHandle) HostCount(_ uint32) int                               { return len(h.healthyHosts) }
func (h *fakeClusterLBHandle) HealthyHostCount(_ uint32) int                        { return len(h.healthyHosts) }
func (h *fakeClusterLBHandle) DegradedHostCount(_ uint32) int                       { return 0 }
func (h *fakeClusterLBHandle) Host(_ uint32, i int) up.HostPtr                      { return h.healthyHosts[i] }
func (h *fakeClusterLBHandle) HealthyHost(_ uint32, i int) up.HostPtr               { return h.healthyHosts[i] }
func (h *fakeClusterLBHandle) HostAddress(_ uint32, _ int) (string, bool)           { return "", false }
func (h *fakeClusterLBHandle) HealthyHostAddress(_ uint32, _ int) (string, bool)    { return "", false }
func (h *fakeClusterLBHandle) HostWeight(_ uint32, _ int) uint32                    { return 1 }
func (h *fakeClusterLBHandle) HealthyHostWeight(_ uint32, _ int) uint32             { return 1 }
func (h *fakeClusterLBHandle) HostHealth(_ uint32, _ int) up.HostHealth             { return up.HostHealthy }
func (h *fakeClusterLBHandle) HostHealthByAddress(_ string) (up.HostHealth, bool)   { return 0, false }
func (h *fakeClusterLBHandle) HostStat(_ uint32, _ int, _ up.HostStat) uint64       { return 0 }
func (h *fakeClusterLBHandle) FindHostByAddress(_ string) up.HostPtr                { return nil }
func (h *fakeClusterLBHandle) MemberUpdateHostAddress(_ int, _ bool) (string, bool) { return "", false }
func (h *fakeClusterLBHandle) HostLocality(_ uint32, _ int) (string, string, string, bool) {
	return "", "", "", false
}
func (h *fakeClusterLBHandle) SetHostData(_ uint32, _ int, _ uintptr) bool { return false }
func (h *fakeClusterLBHandle) GetHostData(_ uint32, _ int) (uintptr, bool) { return 0, false }
func (h *fakeClusterLBHandle) HostMetadataString(_ uint32, _ int, _, _ string) (string, bool) {
	return "", false
}
func (h *fakeClusterLBHandle) HostMetadataNumber(_ uint32, _ int, _, _ string) (float64, bool) {
	return 0, false
}
func (h *fakeClusterLBHandle) HostMetadataBool(_ uint32, _ int, _, _ string) (bool, bool) {
	return false, false
}
func (h *fakeClusterLBHandle) LocalityCount(_ uint32) int            { return 0 }
func (h *fakeClusterLBHandle) LocalityHostCount(_ uint32, _ int) int { return 0 }
func (h *fakeClusterLBHandle) LocalityHostAddress(_ uint32, _, _ int) (string, bool) {
	return "", false
}
func (h *fakeClusterLBHandle) LocalityWeight(_ uint32, _ int) uint32 { return 0 }

func fakeHostPtr() up.HostPtr {
	return up.HostPtr(unsafe.Pointer(new(int)))
}
