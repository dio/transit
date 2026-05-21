// Package down bridges the official Envoy SDK registration and ABI layer.
// It also defines access logger types — absent from the official SDK — that
// down/abi_impl implements via CGO //export symbols.
//
// Callers never import this package directly; transit/up re-exports everything.
package down

import (
	"strconv"
	"strings"
	"sync"
	"unsafe"

	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// RegisterHttpFilter wires one named HTTP filter factory into the official SDK
// registry. Called by up.Register; must be called from an init() function.
func RegisterHttpFilter(name string, factory shared.HttpFilterConfigFactory) {
	sdk.RegisterHttpFilterConfigFactories(map[string]shared.HttpFilterConfigFactory{
		name: factory,
	})
}

// =============================================================================
// Access logger types (official SDK has no access logger API)
// =============================================================================

// TimingInfo holds finalized stream timing from Envoy StreamInfo.
// All durations are in nanoseconds; -1 means the timing is unavailable.
type TimingInfo struct {
	StartTimeUnixNs               int64
	RequestCompleteDurationNs     int64
	FirstUpstreamTxByteSentNs     int64
	LastUpstreamTxByteSentNs      int64
	FirstUpstreamRxByteReceivedNs int64
	LastUpstreamRxByteReceivedNs  int64
	FirstDownstreamTxByteSentNs   int64
	LastDownstreamTxByteSentNs    int64
}

// BytesInfo holds finalized byte counts from Envoy StreamInfo.
type BytesInfo struct {
	BytesReceived     uint64
	BytesSent         uint64
	WireBytesReceived uint64
	WireBytesSent     uint64
}

// AccessLogType identifies the type of access log event.
type AccessLogType int32

const (
	AccessLogTypeNotSet                                  AccessLogType = 0
	AccessLogTypeTcpUpstreamConnected                    AccessLogType = 1
	AccessLogTypeTcpPeriodic                             AccessLogType = 2
	AccessLogTypeTcpConnectionEnd                        AccessLogType = 3
	AccessLogTypeDownstreamStart                         AccessLogType = 4
	AccessLogTypeDownstreamPeriodic                      AccessLogType = 5
	AccessLogTypeDownstreamEnd                           AccessLogType = 6
	AccessLogTypeUpstreamPoolReady                       AccessLogType = 7
	AccessLogTypeUpstreamPeriodic                        AccessLogType = 8
	AccessLogTypeUpstreamEnd                             AccessLogType = 9
	AccessLogTypeDownstreamTunnelSuccessfullyEstablished AccessLogType = 10
	AccessLogTypeUdpTunnelUpstreamConnected              AccessLogType = 11
	AccessLogTypeUdpPeriodic                             AccessLogType = 12
	AccessLogTypeUdpSessionEnd                           AccessLogType = 13
)

// HttpHeaderType selects which header map to read in AccessLoggerHandle.GetHeader.
type HttpHeaderType int32

const (
	HttpHeaderTypeRequest         HttpHeaderType = 0
	HttpHeaderTypeRequestTrailer  HttpHeaderType = 1
	HttpHeaderTypeResponse        HttpHeaderType = 2
	HttpHeaderTypeResponseTrailer HttpHeaderType = 3
)

// AccessLoggerHandle provides access to finalized stream state during OnLog.
// Valid only for the duration of the OnLog callback; do not retain it.
type AccessLoggerHandle interface {
	// GetTimingInfo returns finalized stream timing. -1 means unavailable.
	GetTimingInfo() TimingInfo

	// GetBytesInfo returns finalized byte counts.
	GetBytesInfo() BytesInfo

	// GetResponseFlags returns Envoy response flags as a bitmask.
	// Pass to ResponseFlagsString for the human-readable representation.
	GetResponseFlags() uint64

	// GetResponseCode returns the finalized HTTP response code.
	GetResponseCode() uint32

	// GetAttributeString returns a finalized string stream attribute.
	GetAttributeString(id shared.AttributeID) (shared.UnsafeEnvoyBuffer, bool)

	// GetAttributeInt returns a finalized integer stream attribute.
	GetAttributeInt(id shared.AttributeID) (int64, bool)

	// GetAttributeBool returns a finalized bool stream attribute.
	GetAttributeBool(id shared.AttributeID) (bool, bool)

	// GetHeader retrieves a header value from the specified header map.
	GetHeader(headerType HttpHeaderType, key string) (shared.UnsafeEnvoyBuffer, bool)

	// GetWorkerIndex returns the worker index for this access log event.
	GetWorkerIndex() uint32

	// GetTraceID returns the trace ID from the active span, if tracing is enabled.
	GetTraceID() (shared.UnsafeEnvoyBuffer, bool)

	// GetSpanID returns the span ID from the active span, if tracing is enabled.
	GetSpanID() (shared.UnsafeEnvoyBuffer, bool)

	// IsTraceSampled reports whether the request was sampled for tracing.
	IsTraceSampled() bool

	// GetLocalReplyBody returns the body Envoy sent in a local reply, if any.
	GetLocalReplyBody() (shared.UnsafeEnvoyBuffer, bool)

	// GetUpstreamPoolReadyDurationNs returns nanoseconds spent waiting for an
	// upstream connection from the pool. -1 if unavailable.
	GetUpstreamPoolReadyDurationNs() int64

	// GetUpstreamRequestAttemptCount returns the number of upstream attempts
	// (> 1 means retries occurred).
	GetUpstreamRequestAttemptCount() uint32

	// Log emits a message via Envoy's logging system.
	Log(level shared.LogLevel, format string, args ...any)
}

// AccessLoggerConfigHandle is passed to AccessLoggerConfigFactory.Create on the
// main thread. Use it to define Envoy metrics during initialization.
type AccessLoggerConfigHandle interface {
	// Log emits a message via Envoy's logging system.
	Log(level shared.LogLevel, format string, args ...any)

	// DefineCounter registers a counter metric; returns its ID for later use.
	DefineCounter(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult)

	// DefineGauge registers a gauge metric.
	DefineGauge(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult)

	// DefineHistogram registers a histogram metric.
	DefineHistogram(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult)
}

// AccessLogger is the per-worker-thread logger instance.
type AccessLogger interface {
	// OnLog is called for each access log event.
	// handle is valid only for the duration of this call; do not retain it.
	OnLog(handle AccessLoggerHandle, logType AccessLogType)

	// OnDestroy is called when this logger instance is being destroyed.
	OnDestroy()
}

// AccessLoggerFactory creates AccessLogger instances, one per worker thread.
// Implementations must be safe for concurrent use.
type AccessLoggerFactory interface {
	// NewLogger creates a logger instance for one worker thread.
	NewLogger() AccessLogger

	// OnDestroy is called when the factory is being destroyed.
	OnDestroy()
}

// AccessLoggerConfigFactory is created once on the main thread.
// It parses the logger config and vends AccessLoggerFactory instances.
type AccessLoggerConfigFactory interface {
	// Create is called on the main thread when the access logger config is loaded.
	// config is the raw bytes from the Envoy YAML logger_config field.
	Create(handle AccessLoggerConfigHandle, config []byte) (AccessLoggerFactory, error)
}

// EmptyAccessLogger is a no-op base; embed it to skip unused methods.
type EmptyAccessLogger struct{}

func (e *EmptyAccessLogger) OnLog(_ AccessLoggerHandle, _ AccessLogType) {}
func (e *EmptyAccessLogger) OnDestroy()                                  {}

// =============================================================================
// Access logger registry
// =============================================================================

var (
	accessLoggerMu       sync.RWMutex
	accessLoggerRegistry = map[string]AccessLoggerConfigFactory{}
)

// RegisterAccessLoggerConfigFactory adds a named access logger factory.
// Called by up.RegisterAccessLogger; must be called from an init() function.
func RegisterAccessLoggerConfigFactory(name string, f AccessLoggerConfigFactory) {
	accessLoggerMu.Lock()
	defer accessLoggerMu.Unlock()
	if _, ok := accessLoggerRegistry[name]; ok {
		panic("down: access logger already registered: " + name)
	}
	accessLoggerRegistry[name] = f
}

// GetAccessLoggerConfigFactory returns the factory for the given name,
// or nil if not found. Called by down/abi_impl.
func GetAccessLoggerConfigFactory(name string) AccessLoggerConfigFactory {
	accessLoggerMu.RLock()
	defer accessLoggerMu.RUnlock()
	return accessLoggerRegistry[name]
}

// =============================================================================
// ResponseFlagsString
// =============================================================================

// ResponseFlagsString converts the GetResponseFlags() bitmask to Envoy's
// human-readable flag string (e.g. "UF,UT"), matching %RESPONSE_FLAGS%.
// Returns "" when mask is 0.
func ResponseFlagsString(mask uint64) string {
	if mask == 0 {
		return ""
	}
	var out []string
	for i, name := range responseFlagNames {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, name)
		}
	}
	for i := len(responseFlagNames); i < 64; i++ {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, "0x"+strconv.FormatUint(1<<uint(i), 16))
		}
	}
	return strings.Join(out, ",")
}

// responseFlagNames maps CoreResponseFlag bit positions to their short strings,
// matching Envoy's %RESPONSE_FLAGS% access log format.
// =============================================================================
// Cluster Extension + LB Policy — shared types
// =============================================================================

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
	Address string // "ip:port", e.g. "10.0.0.1:8080"
	Weight  uint32 // load-balancing weight 1–128; 0 is treated as 1
}

// =============================================================================
// Cluster Extension
// =============================================================================

// ClusterLBCompletion is the async handle returned by ClusterLB.ChooseHost.
// Call Complete exactly once to deliver the result, unless CancelHostSelection
// is called first.
type ClusterLBCompletion struct {
	completeFn func(host HostPtr, errDetail string)
	cancelFn   func()
}

// Complete delivers the async host selection result. host == 0 signals failure.
// Must not be called after CancelHostSelection.
func (c *ClusterLBCompletion) Complete(host HostPtr, errDetail string) {
	c.completeFn(host, errDetail)
}

// SetCompleteFn injects the C callback closure. Called by down/abi_impl only.
func (c *ClusterLBCompletion) SetCompleteFn(fn func(HostPtr, string)) { c.completeFn = fn }

// SetCancelFn injects the cancel closure. Called by down/abi_impl only.
func (c *ClusterLBCompletion) SetCancelFn(fn func()) { c.cancelFn = fn }

// Cancel is called by abi_impl before CancelHostSelection to guard Complete
// against being called after the async handle has been removed.
func (c *ClusterLBCompletion) Cancel() { c.cancelFn() }

// ClusterLBContext provides per-request information inside ClusterLB.ChooseHost.
type ClusterLBContext interface {
	// GetFilterState returns a string filter state value written by an earlier
	// HTTP filter via Writer.SetFilterState.
	GetFilterState(key string) (string, bool)

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
}

// ClusterLBHandle gives a ClusterLB access to its cluster's host set.
// Valid for the duration of the ChooseHost call (and host-membership callbacks).
type ClusterLBHandle interface {
	// HealthyHostCount returns the number of healthy hosts at the given priority.
	HealthyHostCount(priority uint32) int

	// HealthyHost returns the host pointer at index within healthy hosts.
	HealthyHost(priority uint32, index int) HostPtr

	// HealthyHostAddress returns the address string of the healthy host at index.
	HealthyHostAddress(priority uint32, index int) (string, bool)

	// HealthyHostWeight returns the LB weight of the healthy host at index.
	HealthyHostWeight(priority uint32, index int) uint32

	// HostHealth returns the health of the host at index within all hosts.
	HostHealth(priority uint32, index int) HostHealth

	// HostStat returns a live Envoy counter for the healthy host at index.
	HostStat(priority uint32, index int, stat HostStat) uint64

	// FindHostByAddress performs an O(1) address→host pointer lookup across
	// all priorities.
	FindHostByAddress(addr string) HostPtr

	// MemberUpdateHostAddress returns the address of the added (isAdded=true)
	// or removed (isAdded=false) host at index during OnHostMembershipUpdate.
	MemberUpdateHostAddress(index int, isAdded bool) (string, bool)
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

	// ServerInitialized is called after all clusters have initialised and
	// before Envoy workers start. Start background discovery here.
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

// =============================================================================
// LB Policy
// =============================================================================

// LBContext provides per-request information inside LBPolicy.ChooseHost.
// Note: filter-state and downstream-SNI are unavailable in LB Policy context;
// use the Cluster Extension (ClusterLBContext) if those are needed.
type LBContext interface {
	GetOverrideHost() (addr string, strict bool)
	GetHeader(name string) (string, bool)
	ComputeHashKey() (uint64, bool)
	GetHostSelectionRetryCount() uint32
	ShouldSelectAnotherHost(lb LBHandle, priority uint32, index int) bool
}

// LBHandle gives an LBPolicy access to the cluster's host set.
type LBHandle interface {
	HealthyHostCount(priority uint32) int
	HealthyHostAddress(priority uint32, index int) (string, bool)
	HealthyHostWeight(priority uint32, index int) uint32
	HostHealth(priority uint32, index int) HostHealth
	HostStat(priority uint32, index int, stat HostStat) uint64
	MemberUpdateHostAddress(index int, isAdded bool) (string, bool)
}

// LBPolicy is the per-worker-thread load balancer for the LB Policy extension.
type LBPolicy interface {
	// ChooseHost selects a host. Write result priority and index into the
	// output params and return true, or return false for no host (→ 503).
	ChooseHost(lb LBHandle, ctx LBContext, priority *uint32, index *uint32) bool

	// OnHostMembershipUpdate is called when the host set changes.
	OnHostMembershipUpdate(lb LBHandle, numAdded, numRemoved int)

	// Close is called when this per-worker instance is destroyed.
	Close()
}

// LBPolicyConfigFactory is created once per LB policy config (main thread).
type LBPolicyConfigFactory interface {
	NewLBPolicy() LBPolicy
	Close()
}

// LBPolicyFactory parses config and produces LBPolicyConfigFactory instances.
type LBPolicyFactory interface {
	Create(config []byte) (LBPolicyConfigFactory, error)
}

// EmptyLBPolicy embeds into LBPolicy implementations to no-op optional methods.
type EmptyLBPolicy struct{}

func (EmptyLBPolicy) OnHostMembershipUpdate(_ LBHandle, _, _ int) {}
func (EmptyLBPolicy) Close()                                      {}

// =============================================================================
// LB Policy registry
// =============================================================================

var (
	lbPolicyMu       sync.RWMutex
	lbPolicyRegistry = map[string]LBPolicyFactory{}
)

// RegisterLBPolicy registers a named LBPolicyFactory.
// Must be called from an init() function. Panics on duplicate names.
func RegisterLBPolicy(name string, f LBPolicyFactory) {
	lbPolicyMu.Lock()
	defer lbPolicyMu.Unlock()
	if _, ok := lbPolicyRegistry[name]; ok {
		panic("down: LB policy already registered: " + name)
	}
	lbPolicyRegistry[name] = f
}

// GetLBPolicyFactory returns the factory for the given name, or nil.
func GetLBPolicyFactory(name string) LBPolicyFactory {
	lbPolicyMu.RLock()
	defer lbPolicyMu.RUnlock()
	return lbPolicyRegistry[name]
}

// =============================================================================
// Response flags
// =============================================================================

var responseFlagNames = [...]string{
	"LH",    // 0  FailedLocalHealthCheck
	"UH",    // 1  NoHealthyUpstream
	"UT",    // 2  UpstreamRequestTimeout
	"LR",    // 3  LocalReset
	"UR",    // 4  UpstreamRemoteReset
	"UF",    // 5  UpstreamConnectionFailure
	"UC",    // 6  UpstreamConnectionTermination
	"UO",    // 7  UpstreamOverflow
	"NR",    // 8  NoRouteFound
	"DI",    // 9  DelayInjected
	"FI",    // 10 FaultInjected
	"RL",    // 11 RateLimited
	"UAEX",  // 12 UnauthorizedExternalService
	"RLSE",  // 13 RateLimitServiceError
	"DC",    // 14 DownstreamConnectionTermination
	"URX",   // 15 UpstreamRetryLimitExceeded
	"SI",    // 16 StreamIdleTimeout
	"IH",    // 17 InvalidEnvoyRequestHeaders
	"DPE",   // 18 DownstreamProtocolError
	"UMSDR", // 19 UpstreamMaxStreamDurationReached
	"RFCF",  // 20 ResponseFromCacheFilter
	"NFCF",  // 21 NoFilterConfigFound
	"DT",    // 22 DurationTimeout
	"UPE",   // 23 UpstreamProtocolError
	"NC",    // 24 NoClusterFound
	"OM",    // 25 OverloadManager
}
