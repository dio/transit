package down

import (
	"sync"
	"unsafe"
)

// HostPtr is an opaque handle to an Envoy upstream host. Obtained from
// ClusterHandle.AddHosts or ClusterLBHandle.FindHostByAddress.
// Valid for the host's lifetime in the cluster.
type HostPtr unsafe.Pointer

// HostHealth represents the health status of a host.
type HostHealth int32

const (
	HostUnhealthy HostHealth = 0
	HostDegraded  HostHealth = 1
	HostHealthy   HostHealth = 2
)

// HostStat identifies a per-host Envoy stat.
type HostStat int32

const (
	HostStatCxConnectFail HostStat = 0
	HostStatCxTotal       HostStat = 1
	HostStatRqError       HostStat = 2
	HostStatRqSuccess     HostStat = 3
	HostStatRqTimeout     HostStat = 4
	HostStatRqTotal       HostStat = 5
	HostStatCxActive      HostStat = 6
	HostStatRqActive      HostStat = 7
)

// HostSpec describes a host to be added to a cluster.
type HostSpec struct {
	Address  string            // "ip:port", e.g. "10.0.0.1:8080"
	Hostname string            // logical FQDN stored on HostImpl; read by auto_host_sni and auto_sni_san_validation at connect time. Empty falls back to the synthesised "cluster_name+address" form.
	Weight   uint32            // load-balancing weight 1–128; 0 is treated as 1
	Metadata map[string]string // per-host endpoint metadata under the envoy.transport_socket_match namespace; used for transport_socket_matches selection
}

// =============================================================================
// Cluster Extension
// =============================================================================

// ClusterLBCompletion is the async handle returned by ClusterLB.ChooseHost.
// Complete and Cancel are idempotent; only the first terminal action wins.
type ClusterLBCompletion struct {
	mu         sync.Mutex
	done       bool
	completeFn func(host HostPtr, errDetail string)
	cancelFn   func()
	finishFn   func()
}

// Complete delivers the async host selection result. host == nil signals failure.
// It returns true if this call completed the selection and false if completion
// had already been cancelled or completed.
func (c *ClusterLBCompletion) Complete(host HostPtr, errDetail string) bool {
	if c == nil {
		return false
	}
	completeFn, finishFn, ok := c.finish()
	if !ok {
		return false
	}
	if completeFn != nil {
		completeFn(host, errDetail)
	}
	if finishFn != nil {
		finishFn()
	}
	return true
}

// SetCompleteFn injects the C callback closure. Called by down/abi_impl only.
func (c *ClusterLBCompletion) SetCompleteFn(fn func(HostPtr, string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completeFn = fn
}

// SetCancelFn injects the cancel closure. Called by down/abi_impl only.
func (c *ClusterLBCompletion) SetCancelFn(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelFn = fn
}

// SetFinishFn injects cleanup for the async handle. Called by down/abi_impl only.
func (c *ClusterLBCompletion) SetFinishFn(fn func()) {
	if c == nil {
		return
	}
	c.mu.Lock()
	done := c.done
	if !done {
		c.finishFn = fn
	}
	c.mu.Unlock()
	if done && fn != nil {
		fn()
	}
}

// Cancel is called by abi_impl before CancelHostSelection to guard Complete
// against being called after the async handle has been removed.
func (c *ClusterLBCompletion) Cancel() bool {
	if c == nil {
		return false
	}
	cancelFn, finishFn, ok := c.cancel()
	if !ok {
		return false
	}
	if cancelFn != nil {
		cancelFn()
	}
	if finishFn != nil {
		finishFn()
	}
	return true
}

func (c *ClusterLBCompletion) finish() (func(HostPtr, string), func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return nil, nil, false
	}
	c.done = true
	return c.completeFn, c.finishFn, true
}

func (c *ClusterLBCompletion) cancel() (func(), func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return nil, nil, false
	}
	c.done = true
	return c.cancelFn, c.finishFn, true
}

// ClusterLBContext provides per-request information inside ClusterLB.ChooseHost.
type ClusterLBContext interface {
	// GetAllHeaders returns all downstream request headers.
	GetAllHeaders() [][2]string

	// GetFilterState returns a string filter state value written by an earlier
	// HTTP filter via Writer.SetFilterState.
	GetFilterState(key string) (string, bool)

	// GetFilterStateTyped returns the serialized value of a typed filter-state
	// object. Prefer GetFilterState for raw string state written by Writer.
	GetFilterStateTyped(key string) (string, bool)

	// GetOverrideHost returns the host address and strict flag set by an HTTP
	// filter via Writer.SetUpstreamOverrideHost.
	GetOverrideHost() (addr string, strict bool)

	// GetHeader returns the first value of a downstream request header.
	GetHeader(name string) (string, bool)

	// GetDownstreamSNI returns the TLS SNI from the downstream connection.
	GetDownstreamSNI() (string, bool)

	// ComputeHashKey computes a consistent-hash key from the request context.
	ComputeHashKey() (uint64, bool)

	// GetHostSelectionRetryCount returns the number of host-selection retries
	// configured for this request.
	GetHostSelectionRetryCount() uint32

	// ShouldSelectAnotherHost reports whether Envoy wants the LB to skip the
	// candidate at (priority, index) — typically because it already failed.
	ShouldSelectAnotherHost(lb ClusterLBHandle, priority uint32, index int) bool

	// NewCompletion creates an async handle pre-loaded with the Envoy pointers
	// needed to call Complete later. Use for async ChooseHost paths.
	NewCompletion() *ClusterLBCompletion

	// GetStreamObject returns the typed value stored under key for this stream
	// by an HTTP filter via Writer.SetStreamObject (Primitive A). Returns
	// (nil, false) if the key was never set or the bag does not exist for this
	// stream. The nonce is read from filter state under the reserved key
	// "up.stream_object_id".
	GetStreamObject(key string) (any, bool)
}

// ClusterLBHandle gives a ClusterLB access to its cluster's host set.
// Valid for the duration of the ChooseHost call (and host-membership callbacks).
type ClusterLBHandle interface {
	// ClusterName returns the owning cluster's name.
	ClusterName() string

	// PriorityCount returns the number of priority levels.
	PriorityCount() int

	// HostCount returns the number of all hosts at the given priority.
	HostCount(priority uint32) int

	// HealthyHostCount returns the number of healthy hosts at the given priority.
	HealthyHostCount(priority uint32) int

	// DegradedHostCount returns the number of degraded hosts at the given priority.
	DegradedHostCount(priority uint32) int

	// Host returns the host pointer at index within all hosts.
	Host(priority uint32, index int) HostPtr

	// HealthyHost returns the host pointer at index within healthy hosts.
	HealthyHost(priority uint32, index int) HostPtr

	// HostAddress returns the address string of the host at index within all hosts.
	HostAddress(priority uint32, index int) (string, bool)

	// HealthyHostAddress returns the address string of the healthy host at index.
	HealthyHostAddress(priority uint32, index int) (string, bool)

	// HostWeight returns the LB weight of the host at index within all hosts.
	HostWeight(priority uint32, index int) uint32

	// HealthyHostWeight returns the LB weight of the healthy host at index.
	HealthyHostWeight(priority uint32, index int) uint32

	// HostHealth returns the health of the host at index within all hosts.
	HostHealth(priority uint32, index int) HostHealth

	// HostHealthByAddress performs an O(1) address lookup and returns host health.
	HostHealthByAddress(addr string) (HostHealth, bool)

	// HostStat returns a live Envoy counter for the host at index within all hosts.
	HostStat(priority uint32, index int, stat HostStat) uint64

	// FindHostByAddress performs an O(1) address→host pointer lookup across
	// all priorities.
	FindHostByAddress(addr string) HostPtr

	// MemberUpdateHostAddress returns the address of the added (isAdded=true)
	// or removed (isAdded=false) host at index during OnHostMembershipUpdate.
	MemberUpdateHostAddress(index int, isAdded bool) (string, bool)

	// HostLocality returns region, zone, and sub-zone metadata for a host.
	HostLocality(priority uint32, index int) (region, zone, subZone string, ok bool)

	// SetHostData stores per-worker opaque data on a host.
	SetHostData(priority uint32, index int, data uintptr) bool

	// GetHostData retrieves per-worker opaque data stored on a host.
	GetHostData(priority uint32, index int) (uintptr, bool)

	HostMetadataString(priority uint32, index int, filterName, key string) (string, bool)
	HostMetadataNumber(priority uint32, index int, filterName, key string) (float64, bool)
	HostMetadataBool(priority uint32, index int, filterName, key string) (bool, bool)

	LocalityCount(priority uint32) int
	LocalityHostCount(priority uint32, localityIndex int) int
	LocalityHostAddress(priority uint32, localityIndex, hostIndex int) (string, bool)
	LocalityWeight(priority uint32, localityIndex int) uint32
}

// ClusterLB is the per-worker-thread load balancer created by ClusterFactory.
// One instance exists per Envoy worker; methods are called concurrently.
type ClusterLB interface {
	// ChooseHost selects an upstream host for a request.
	// Return (host, nil) for sync success, (nil, nil) for sync failure (→ 503),
	// or (nil, completion) to suspend the request for async resolution.
	ChooseHost(lb ClusterLBHandle, ctx ClusterLBContext) (HostPtr, *ClusterLBCompletion)

	// CancelHostSelection is called when the stream is torn down before an
	// async ChooseHost completes. Must not call completion.Complete after this.
	CancelHostSelection(completion *ClusterLBCompletion)

	// OnHostMembershipUpdate is called on each worker when hosts are added or
	// removed. Use MemberUpdateHostAddress inside this call to get addresses.
	// Optional: embed EmptyClusterLB to no-op it.
	OnHostMembershipUpdate(lb ClusterLBHandle, numAdded, numRemoved int)

	// Close is called when the per-worker LB is being destroyed.
	Close()
}

// ClusterHandle gives a Cluster access to Envoy cluster operations.
// All methods except Schedule must be called on the main thread.
type ClusterHandle interface {
	// AddHosts adds hosts to priority 0. Returns one HostPtr per spec;
	// retain them for RemoveHosts / UpdateHostHealth.
	AddHosts(hosts []HostSpec) []HostPtr

	// RemoveHosts removes hosts previously added via AddHosts.
	RemoveHosts(hosts []HostPtr)

	// UpdateHostHealth updates the EDS health status of a host.
	UpdateHostHealth(host HostPtr, health HostHealth)

	// FindHostByAddress performs an O(1) address→host pointer lookup.
	FindHostByAddress(addr string) HostPtr

	// PreInitComplete signals that initial host discovery is done.
	// Must be called during or shortly after Cluster.Init.
	PreInitComplete()

	// Schedule dispatches fn on the main thread. Thread-safe; safe to call
	// from any goroutine. fn is called via on_cluster_scheduled.
	Schedule(fn func())
}

// Cluster is the main cluster instance created once per cluster definition.
type Cluster interface {
	// Init is called when cluster initialization begins. Call
	// ClusterHandle.PreInitComplete when the initial host set is ready.
	Init(h ClusterHandle)

	// NewClusterLB creates a per-worker LB instance.
	NewClusterLB() ClusterLB

	// ServerInitialized is called on the Envoy main thread after all clusters
	// have initialized and before workers start accepting traffic. It is safe
	// to call blocking setup operations here (e.g. a cold-start RPC Fetch)
	// because no requests are in flight yet. Use up.ClusterGroup.Start to
	// launch background goroutines scoped to the cluster's lifetime.
	ServerInitialized(h ClusterHandle)

	// DrainStarted is called when Envoy begins draining.
	DrainStarted(h ClusterHandle)

	// Shutdown is called on process exit. Must call done() when finished.
	Shutdown(h ClusterHandle, done func())

	// Close is called when the cluster is being destroyed.
	Close()
}

// ClusterConfigFactory is created once per cluster config block (main thread).
type ClusterConfigFactory interface {
	// NewCluster creates the cluster instance. h is valid for the cluster's
	// lifetime.
	NewCluster(h ClusterHandle) Cluster

	// Close is called when the cluster config is destroyed.
	Close()
}

// ClusterFactory parses config bytes and produces ClusterConfigFactory instances.
// Register via RegisterCluster in an init() function.
type ClusterFactory interface {
	// Create is called on the main thread when the cluster config is loaded.
	Create(config []byte) (ClusterConfigFactory, error)
}

// EmptyClusterLB embeds into ClusterLB implementations to no-op optional methods.
type EmptyClusterLB struct{}

func (EmptyClusterLB) OnHostMembershipUpdate(_ ClusterLBHandle, _, _ int) {}
func (EmptyClusterLB) CancelHostSelection(_ *ClusterLBCompletion)         {}
func (EmptyClusterLB) Close()                                             {}

// =============================================================================
// Cluster registry
// =============================================================================

var (
	clusterMu       sync.RWMutex
	clusterRegistry = map[string]ClusterFactory{}
)

// RegisterCluster registers a named ClusterFactory.
// Must be called from an init() function. Panics on duplicate names.
func RegisterCluster(name string, f ClusterFactory) {
	clusterMu.Lock()
	defer clusterMu.Unlock()
	if _, ok := clusterRegistry[name]; ok {
		panic("down: cluster already registered: " + name)
	}
	clusterRegistry[name] = f
}

// GetClusterFactory returns the factory for the given name, or nil if not found.
func GetClusterFactory(name string) ClusterFactory {
	clusterMu.RLock()
	defer clusterMu.RUnlock()
	return clusterRegistry[name]
}
