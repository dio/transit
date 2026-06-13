package abi_impl

/*
#include "abi.h"

static inline void transit_call_event_cb(envoy_dynamic_module_type_event_cb cb, void* ctx) {
    cb(ctx);
}
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	"github.com/dio/transit/down"
)

// =============================================================================
// Wrapper types
// =============================================================================

type clusterConfigWrapper struct {
	configFactory down.ClusterConfigFactory
	configEnvoy   C.envoy_dynamic_module_type_cluster_config_envoy_ptr
}

type clusterWrapper struct {
	cluster down.Cluster
	handle  *clusterHandleImpl
}

type clusterLBWrapper struct {
	lb      down.ClusterLB
	lbEnvoy C.envoy_dynamic_module_type_cluster_lb_envoy_ptr
}

type asyncHandleWrapper struct {
	completion *down.ClusterLBCompletion
}

var (
	clusterConfigManager = newManager[clusterConfigWrapper]()
	clusterManager       = newManager[clusterWrapper]()
	clusterLBManager     = newManager[clusterLBWrapper]()
	clusterAsyncManager  = newManager[asyncHandleWrapper]()
)

// =============================================================================
// dymClusterMetricsHandle — implements down.ClusterMetricsHandle
// =============================================================================

type dymClusterMetricsHandle struct {
	configPtr C.envoy_dynamic_module_type_cluster_config_envoy_ptr
}

var _ down.ClusterMetricsHandle = (*dymClusterMetricsHandle)(nil)

// stringsToLabelBuffers converts []string to a slice of module buffers and
// returns a pointer to the first element (nil for empty slices).
func stringsToLabelBuffers(ss []string) ([]C.envoy_dynamic_module_type_module_buffer, *C.envoy_dynamic_module_type_module_buffer) {
	if len(ss) == 0 {
		return nil, nil
	}
	bufs := make([]C.envoy_dynamic_module_type_module_buffer, len(ss))
	for i, s := range ss {
		bufs[i] = stringToModuleBuffer(s)
	}
	return bufs, &bufs[0]
}

func (h *dymClusterMetricsHandle) DefineCounter(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult) {
	labelBufs, labelPtr := stringsToLabelBuffers(tagKeys)
	var id C.size_t
	result := C.envoy_dynamic_module_callback_cluster_config_define_counter(
		h.configPtr, stringToModuleBuffer(name),
		labelPtr, C.size_t(len(tagKeys)),
		&id,
	)
	runtime.KeepAlive(name)
	runtime.KeepAlive(tagKeys)
	runtime.KeepAlive(labelBufs)
	return shared.MetricID(id), shared.MetricsResult(result)
}

func (h *dymClusterMetricsHandle) DefineGauge(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult) {
	labelBufs, labelPtr := stringsToLabelBuffers(tagKeys)
	var id C.size_t
	result := C.envoy_dynamic_module_callback_cluster_config_define_gauge(
		h.configPtr, stringToModuleBuffer(name),
		labelPtr, C.size_t(len(tagKeys)),
		&id,
	)
	runtime.KeepAlive(name)
	runtime.KeepAlive(tagKeys)
	runtime.KeepAlive(labelBufs)
	return shared.MetricID(id), shared.MetricsResult(result)
}

func (h *dymClusterMetricsHandle) DefineHistogram(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult) {
	labelBufs, labelPtr := stringsToLabelBuffers(tagKeys)
	var id C.size_t
	result := C.envoy_dynamic_module_callback_cluster_config_define_histogram(
		h.configPtr, stringToModuleBuffer(name),
		labelPtr, C.size_t(len(tagKeys)),
		&id,
	)
	runtime.KeepAlive(name)
	runtime.KeepAlive(tagKeys)
	runtime.KeepAlive(labelBufs)
	return shared.MetricID(id), shared.MetricsResult(result)
}

func (h *dymClusterMetricsHandle) IncrementCounterValue(id shared.MetricID, value uint64, labelValues ...string) shared.MetricsResult {
	labelBufs, labelPtr := stringsToLabelBuffers(labelValues)
	result := C.envoy_dynamic_module_callback_cluster_config_increment_counter(
		h.configPtr, C.size_t(id),
		labelPtr, C.size_t(len(labelValues)),
		C.uint64_t(value),
	)
	runtime.KeepAlive(labelValues)
	runtime.KeepAlive(labelBufs)
	return shared.MetricsResult(result)
}

func (h *dymClusterMetricsHandle) SetGaugeValue(id shared.MetricID, value uint64, labelValues ...string) shared.MetricsResult {
	labelBufs, labelPtr := stringsToLabelBuffers(labelValues)
	result := C.envoy_dynamic_module_callback_cluster_config_set_gauge(
		h.configPtr, C.size_t(id),
		labelPtr, C.size_t(len(labelValues)),
		C.uint64_t(value),
	)
	runtime.KeepAlive(labelValues)
	runtime.KeepAlive(labelBufs)
	return shared.MetricsResult(result)
}

func (h *dymClusterMetricsHandle) IncrementGaugeValue(id shared.MetricID, value uint64, labelValues ...string) shared.MetricsResult {
	labelBufs, labelPtr := stringsToLabelBuffers(labelValues)
	result := C.envoy_dynamic_module_callback_cluster_config_increment_gauge(
		h.configPtr, C.size_t(id),
		labelPtr, C.size_t(len(labelValues)),
		C.uint64_t(value),
	)
	runtime.KeepAlive(labelValues)
	runtime.KeepAlive(labelBufs)
	return shared.MetricsResult(result)
}

func (h *dymClusterMetricsHandle) DecrementGaugeValue(id shared.MetricID, value uint64, labelValues ...string) shared.MetricsResult {
	labelBufs, labelPtr := stringsToLabelBuffers(labelValues)
	result := C.envoy_dynamic_module_callback_cluster_config_decrement_gauge(
		h.configPtr, C.size_t(id),
		labelPtr, C.size_t(len(labelValues)),
		C.uint64_t(value),
	)
	runtime.KeepAlive(labelValues)
	runtime.KeepAlive(labelBufs)
	return shared.MetricsResult(result)
}

func (h *dymClusterMetricsHandle) RecordHistogramValue(id shared.MetricID, value uint64, labelValues ...string) shared.MetricsResult {
	labelBufs, labelPtr := stringsToLabelBuffers(labelValues)
	result := C.envoy_dynamic_module_callback_cluster_config_record_histogram_value(
		h.configPtr, C.size_t(id),
		labelPtr, C.size_t(len(labelValues)),
		C.uint64_t(value),
	)
	runtime.KeepAlive(labelValues)
	runtime.KeepAlive(labelBufs)
	return shared.MetricsResult(result)
}

// =============================================================================
// clusterHandleImpl — implements down.ClusterHandle
// =============================================================================

type clusterHandleImpl struct {
	envoyPtr     C.envoy_dynamic_module_type_cluster_envoy_ptr
	schedulerPtr C.envoy_dynamic_module_type_cluster_scheduler_module_ptr
	mu           sync.Mutex
	nextID       uint64
	pending      map[uint64]func()
}

var _ down.ClusterHandle = (*clusterHandleImpl)(nil)

func newClusterHandle(envoyPtr C.envoy_dynamic_module_type_cluster_envoy_ptr) *clusterHandleImpl {
	h := &clusterHandleImpl{
		envoyPtr: envoyPtr,
		pending:  make(map[uint64]func()),
	}
	return h
}

func (h *clusterHandleImpl) destroy() {
	if h.schedulerPtr != nil {
		C.envoy_dynamic_module_callback_cluster_scheduler_delete(h.schedulerPtr)
		h.schedulerPtr = nil
	}
}

func (h *clusterHandleImpl) AddHosts(specs []down.HostSpec) []down.HostPtr {
	n := len(specs)
	if n == 0 {
		return nil
	}
	addrs := make([]C.envoy_dynamic_module_type_module_buffer, n)
	weights := make([]C.uint32_t, n)
	empty := make([]C.envoy_dynamic_module_type_module_buffer, n) // locality zeros
	results := make([]C.envoy_dynamic_module_type_cluster_host_envoy_ptr, n)

	hostnames := make([]C.envoy_dynamic_module_type_module_buffer, n)
	rawAddrs := make([]string, n)
	rawHostnames := make([]string, n)
	maxPairs := 0
	for i, s := range specs {
		rawAddrs[i] = s.Address
		addrs[i] = stringToModuleBuffer(rawAddrs[i])
		rawHostnames[i] = s.Hostname
		hostnames[i] = stringToModuleBuffer(rawHostnames[i])
		w := s.Weight
		if w == 0 {
			w = 1
		}
		weights[i] = C.uint32_t(w)
		if len(s.Metadata) > maxPairs {
			maxPairs = len(s.Metadata)
		}
	}

	// Build flat (filter_name, key, value) triples for transport_socket_matches.
	// All hosts share the same metadata_pairs_per_host count; hosts with fewer
	// entries are padded with zero-length buffers (already zero from make).
	var (
		metaBufs []C.envoy_dynamic_module_type_module_buffer
		rawMeta  []string
		metaPtr  *C.envoy_dynamic_module_type_module_buffer
	)
	if maxPairs > 0 {
		total := n * maxPairs * 3
		metaBufs = make([]C.envoy_dynamic_module_type_module_buffer, total)
		rawMeta = make([]string, total)
		for i, s := range specs {
			j := 0
			for k, v := range s.Metadata {
				base := (i*maxPairs + j) * 3
				rawMeta[base] = "envoy.transport_socket_match"
				rawMeta[base+1] = k
				rawMeta[base+2] = v
				metaBufs[base] = stringToModuleBuffer(rawMeta[base])
				metaBufs[base+1] = stringToModuleBuffer(rawMeta[base+1])
				metaBufs[base+2] = stringToModuleBuffer(rawMeta[base+2])
				j++
			}
		}
		metaPtr = &metaBufs[0]
	}

	C.envoy_dynamic_module_callback_cluster_add_hosts_with_hostnames(
		h.envoyPtr, 0,
		&addrs[0], &hostnames[0], &weights[0],
		&empty[0], &empty[0], &empty[0],
		metaPtr, C.size_t(maxPairs),
		C.size_t(n),
		&results[0],
	)
	runtime.KeepAlive(rawAddrs)
	runtime.KeepAlive(rawHostnames)
	runtime.KeepAlive(rawMeta)

	out := make([]down.HostPtr, n)
	for i, r := range results {
		out[i] = down.HostPtr(unsafe.Pointer(r))
	}
	return out
}

func (h *clusterHandleImpl) RemoveHosts(hosts []down.HostPtr) {
	if len(hosts) == 0 {
		return
	}
	ptrs := make([]C.envoy_dynamic_module_type_cluster_host_envoy_ptr, len(hosts))
	for i, hp := range hosts {
		ptrs[i] = C.envoy_dynamic_module_type_cluster_host_envoy_ptr(unsafe.Pointer(hp))
	}
	C.envoy_dynamic_module_callback_cluster_remove_hosts(h.envoyPtr, &ptrs[0], C.size_t(len(ptrs)))
}

func (h *clusterHandleImpl) UpdateHostHealth(host down.HostPtr, health down.HostHealth) {
	C.envoy_dynamic_module_callback_cluster_update_host_health(
		h.envoyPtr,
		C.envoy_dynamic_module_type_cluster_host_envoy_ptr(unsafe.Pointer(host)),
		C.envoy_dynamic_module_type_host_health(health),
	)
}

func (h *clusterHandleImpl) FindHostByAddress(addr string) down.HostPtr {
	buf := stringToModuleBuffer(addr)
	ptr := C.envoy_dynamic_module_callback_cluster_find_host_by_address(h.envoyPtr, buf)
	runtime.KeepAlive(addr)
	return down.HostPtr(unsafe.Pointer(ptr))
}

func (h *clusterHandleImpl) PreInitComplete() {
	C.envoy_dynamic_module_callback_cluster_pre_init_complete(h.envoyPtr)
}

func (h *clusterHandleImpl) Schedule(fn func()) {
	h.mu.Lock()
	if h.schedulerPtr == nil {
		h.schedulerPtr = C.envoy_dynamic_module_callback_cluster_scheduler_new(h.envoyPtr)
	}
	id := h.nextID
	h.nextID++
	h.pending[id] = fn
	scheduler := h.schedulerPtr
	h.mu.Unlock()
	C.envoy_dynamic_module_callback_cluster_scheduler_commit(scheduler, C.uint64_t(id))
}

func (h *clusterHandleImpl) runPending(id uint64) {
	h.mu.Lock()
	fn := h.pending[id]
	delete(h.pending, id)
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// =============================================================================
// dymClusterLBHandle — implements down.ClusterLBHandle (stack-allocated per call)
// =============================================================================

type dymClusterLBHandle struct {
	lbPtr C.envoy_dynamic_module_type_cluster_lb_envoy_ptr
}

var _ down.ClusterLBHandle = (*dymClusterLBHandle)(nil)

func (h *dymClusterLBHandle) ClusterName() string {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	C.envoy_dynamic_module_callback_cluster_lb_get_cluster_name(h.lbPtr, &buf)
	return envoyBufferToStringUnsafe(buf)
}

func (h *dymClusterLBHandle) PriorityCount() int {
	return int(C.envoy_dynamic_module_callback_cluster_lb_get_priority_set_size(h.lbPtr))
}

func (h *dymClusterLBHandle) HostCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_cluster_lb_get_hosts_count(
		h.lbPtr, C.uint32_t(priority)))
}

func (h *dymClusterLBHandle) HealthyHostCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_cluster_lb_get_healthy_host_count(
		h.lbPtr, C.uint32_t(priority)))
}

func (h *dymClusterLBHandle) DegradedHostCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_cluster_lb_get_degraded_hosts_count(
		h.lbPtr, C.uint32_t(priority)))
}

func (h *dymClusterLBHandle) Host(priority uint32, index int) down.HostPtr {
	ptr := C.envoy_dynamic_module_callback_cluster_lb_get_host(
		h.lbPtr, C.uint32_t(priority), C.size_t(index))
	return down.HostPtr(unsafe.Pointer(ptr))
}

func (h *dymClusterLBHandle) HealthyHost(priority uint32, index int) down.HostPtr {
	ptr := C.envoy_dynamic_module_callback_cluster_lb_get_healthy_host(
		h.lbPtr, C.uint32_t(priority), C.size_t(index))
	return down.HostPtr(unsafe.Pointer(ptr))
}

func (h *dymClusterLBHandle) HostAddress(priority uint32, index int) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_host_address(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymClusterLBHandle) HealthyHostAddress(priority uint32, index int) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_healthy_host_address(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymClusterLBHandle) HostWeight(priority uint32, index int) uint32 {
	return uint32(C.envoy_dynamic_module_callback_cluster_lb_get_host_weight(
		h.lbPtr, C.uint32_t(priority), C.size_t(index)))
}

func (h *dymClusterLBHandle) HealthyHostWeight(priority uint32, index int) uint32 {
	return uint32(C.envoy_dynamic_module_callback_cluster_lb_get_healthy_host_weight(
		h.lbPtr, C.uint32_t(priority), C.size_t(index)))
}

func (h *dymClusterLBHandle) HostHealth(priority uint32, index int) down.HostHealth {
	return down.HostHealth(C.envoy_dynamic_module_callback_cluster_lb_get_host_health(
		h.lbPtr, C.uint32_t(priority), C.size_t(index)))
}

func (h *dymClusterLBHandle) HostHealthByAddress(addr string) (down.HostHealth, bool) {
	var health C.envoy_dynamic_module_type_host_health
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_host_health_by_address(
		h.lbPtr, stringToModuleBuffer(addr), &health))
	runtime.KeepAlive(addr)
	return down.HostHealth(health), ok
}

func (h *dymClusterLBHandle) HostStat(priority uint32, index int, stat down.HostStat) uint64 {
	return uint64(C.envoy_dynamic_module_callback_cluster_lb_get_host_stat(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		C.envoy_dynamic_module_type_host_stat(stat)))
}

func (h *dymClusterLBHandle) FindHostByAddress(addr string) down.HostPtr {
	buf := stringToModuleBuffer(addr)
	ptr := C.envoy_dynamic_module_callback_cluster_lb_find_host_by_address(h.lbPtr, buf)
	runtime.KeepAlive(addr)
	return down.HostPtr(unsafe.Pointer(ptr))
}

func (h *dymClusterLBHandle) MemberUpdateHostAddress(index int, isAdded bool) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_member_update_host_address(
		h.lbPtr, C.size_t(index), C.bool(isAdded), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymClusterLBHandle) HostLocality(priority uint32, index int) (string, string, string, bool) {
	var region, zone, subZone C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_host_locality(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &region, &zone, &subZone))
	if !ok {
		return "", "", "", false
	}
	return envoyBufferToStringUnsafe(region),
		envoyBufferToStringUnsafe(zone),
		envoyBufferToStringUnsafe(subZone),
		true
}

func (h *dymClusterLBHandle) SetHostData(priority uint32, index int, data uintptr) bool {
	return bool(C.envoy_dynamic_module_callback_cluster_lb_set_host_data(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), C.uintptr_t(data)))
}

func (h *dymClusterLBHandle) GetHostData(priority uint32, index int) (uintptr, bool) {
	var data C.uintptr_t
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_host_data(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &data))
	return uintptr(data), ok
}

func (h *dymClusterLBHandle) HostMetadataString(priority uint32, index int, filterName, key string) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_host_metadata_string(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		stringToModuleBuffer(filterName), stringToModuleBuffer(key), &buf))
	runtime.KeepAlive(filterName)
	runtime.KeepAlive(key)
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymClusterLBHandle) HostMetadataNumber(priority uint32, index int, filterName, key string) (float64, bool) {
	var out C.double
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_host_metadata_number(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		stringToModuleBuffer(filterName), stringToModuleBuffer(key), &out))
	runtime.KeepAlive(filterName)
	runtime.KeepAlive(key)
	return float64(out), ok
}

func (h *dymClusterLBHandle) HostMetadataBool(priority uint32, index int, filterName, key string) (bool, bool) {
	var out C.bool
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_host_metadata_bool(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		stringToModuleBuffer(filterName), stringToModuleBuffer(key), &out))
	runtime.KeepAlive(filterName)
	runtime.KeepAlive(key)
	return bool(out), ok
}

func (h *dymClusterLBHandle) LocalityCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_cluster_lb_get_locality_count(
		h.lbPtr, C.uint32_t(priority)))
}

func (h *dymClusterLBHandle) LocalityHostCount(priority uint32, localityIndex int) int {
	return int(C.envoy_dynamic_module_callback_cluster_lb_get_locality_host_count(
		h.lbPtr, C.uint32_t(priority), C.size_t(localityIndex)))
}

func (h *dymClusterLBHandle) LocalityHostAddress(priority uint32, localityIndex, hostIndex int) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_get_locality_host_address(
		h.lbPtr, C.uint32_t(priority), C.size_t(localityIndex), C.size_t(hostIndex), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymClusterLBHandle) LocalityWeight(priority uint32, localityIndex int) uint32 {
	return uint32(C.envoy_dynamic_module_callback_cluster_lb_get_locality_weight(
		h.lbPtr, C.uint32_t(priority), C.size_t(localityIndex)))
}

// =============================================================================
// dymClusterLBContext — implements down.ClusterLBContext (stack-allocated per call)
// =============================================================================

type dymClusterLBContext struct {
	ctxPtr C.envoy_dynamic_module_type_cluster_lb_context_envoy_ptr
	lbPtr  C.envoy_dynamic_module_type_cluster_lb_envoy_ptr
}

var _ down.ClusterLBContext = (*dymClusterLBContext)(nil)

func (c *dymClusterLBContext) GetAllHeaders() [][2]string {
	n := int(C.envoy_dynamic_module_callback_cluster_lb_context_get_downstream_headers_size(c.ctxPtr))
	if n == 0 {
		return nil
	}
	raw := make([]C.envoy_dynamic_module_type_envoy_http_header, n)
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_get_downstream_headers(c.ctxPtr, &raw[0]))
	if !ok {
		return nil
	}
	out := make([][2]string, n)
	for i, h := range raw {
		out[i] = envoyHTTPHeaderToPair(h)
	}
	return out
}

func (c *dymClusterLBContext) GetFilterState(key string) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_get_filter_state_bytes(
		c.ctxPtr, stringToModuleBuffer(key), &buf))
	runtime.KeepAlive(key)
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (c *dymClusterLBContext) GetFilterStateTyped(key string) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_get_filter_state_typed(
		c.ctxPtr, stringToModuleBuffer(key), &buf))
	runtime.KeepAlive(key)
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (c *dymClusterLBContext) GetOverrideHost() (string, bool) {
	var addrBuf C.envoy_dynamic_module_type_envoy_buffer
	var strict C.bool
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_get_override_host(
		c.ctxPtr, &addrBuf, &strict))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(addrBuf), bool(strict)
}

func (c *dymClusterLBContext) GetHeader(name string) (string, bool) {
	var val C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_get_downstream_header(
		c.ctxPtr, stringToModuleBuffer(name), &val, 0, nil))
	runtime.KeepAlive(name)
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(val), true
}

func (c *dymClusterLBContext) GetDownstreamSNI() (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_get_downstream_connection_sni(
		c.ctxPtr, &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (c *dymClusterLBContext) ComputeHashKey() (uint64, bool) {
	var h C.uint64_t
	ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_compute_hash_key(c.ctxPtr, &h))
	return uint64(h), ok
}

func (c *dymClusterLBContext) GetHostSelectionRetryCount() uint32 {
	return uint32(C.envoy_dynamic_module_callback_cluster_lb_context_get_host_selection_retry_count(
		c.ctxPtr))
}

func (c *dymClusterLBContext) ShouldSelectAnotherHost(lb down.ClusterLBHandle, priority uint32, index int) bool {
	h, ok := lb.(*dymClusterLBHandle)
	if !ok {
		return false
	}
	return bool(C.envoy_dynamic_module_callback_cluster_lb_context_should_select_another_host(
		h.lbPtr, c.ctxPtr, C.uint32_t(priority), C.size_t(index)))
}

func (c *dymClusterLBContext) GetStreamObject(key string) (any, bool) {
	// Read the per-stream nonce from filter state under the reserved key.
	nonce, ok := c.GetFilterState(down.StreamObjectIDKey)
	if !ok || nonce == "" {
		return nil, false
	}
	bag, ok := down.LookupStreamObjectBag(nonce)
	if !ok {
		return nil, false
	}
	return bag.Get(key)
}

func (c *dymClusterLBContext) NewCompletion() *down.ClusterLBCompletion {
	lbPtr := c.lbPtr
	ctxPtr := c.ctxPtr
	comp := &down.ClusterLBCompletion{}
	comp.SetCompleteFn(func(host down.HostPtr, errDetail string) {
		hostPtr := C.envoy_dynamic_module_type_cluster_host_envoy_ptr(
			unsafe.Pointer(host))
		detail := errDetail
		buf := stringToModuleBuffer(detail)
		C.envoy_dynamic_module_callback_cluster_lb_async_host_selection_complete(
			lbPtr, ctxPtr, hostPtr, buf)
		runtime.KeepAlive(detail)
	})
	return comp
}

// =============================================================================
// ABI export functions
// =============================================================================

//export envoy_dynamic_module_on_cluster_config_new
func envoy_dynamic_module_on_cluster_config_new(
	configEnvoyPtr C.envoy_dynamic_module_type_cluster_config_envoy_ptr,
	name C.envoy_dynamic_module_type_envoy_buffer,
	config C.envoy_dynamic_module_type_envoy_buffer,
) C.envoy_dynamic_module_type_cluster_config_module_ptr {
	nameStr := envoyBufferToStringUnsafe(name)
	factory := down.GetClusterFactory(nameStr)
	if factory == nil {
		fmt.Fprintf(os.Stderr, "[transit] cluster %q: no factory registered\n", nameStr)
		return nil
	}
	cfgBytes := envoyBufferToBytesUnsafe(config)
	metrics := &dymClusterMetricsHandle{configPtr: configEnvoyPtr}
	var configFactory down.ClusterConfigFactory
	var err error
	if f, ok := factory.(down.ClusterFactoryWithMetrics); ok {
		configFactory, err = f.CreateWithMetrics(metrics, cfgBytes)
	} else {
		configFactory, err = factory.Create(cfgBytes)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[transit] cluster %q: Create failed: %v\n", nameStr, err)
		return nil
	}
	w := &clusterConfigWrapper{configFactory: configFactory, configEnvoy: configEnvoyPtr}
	return C.envoy_dynamic_module_type_cluster_config_module_ptr(clusterConfigManager.record(w))
}

//export envoy_dynamic_module_on_cluster_config_destroy
func envoy_dynamic_module_on_cluster_config_destroy(
	configModulePtr C.envoy_dynamic_module_type_cluster_config_module_ptr,
) {
	ptr := unsafe.Pointer(configModulePtr)
	w := clusterConfigManager.unwrap(ptr)
	if w == nil {
		return
	}
	w.configFactory.Close()
	clusterConfigManager.remove(ptr)
}

//export envoy_dynamic_module_on_cluster_new
func envoy_dynamic_module_on_cluster_new(
	configModulePtr C.envoy_dynamic_module_type_cluster_config_module_ptr,
	clusterEnvoyPtr C.envoy_dynamic_module_type_cluster_envoy_ptr,
) C.envoy_dynamic_module_type_cluster_module_ptr {
	cw := clusterConfigManager.unwrap(unsafe.Pointer(configModulePtr))
	if cw == nil {
		return nil
	}
	h := newClusterHandle(clusterEnvoyPtr)
	cluster := cw.configFactory.NewCluster(h)
	if cluster == nil {
		h.destroy()
		return nil
	}
	w := &clusterWrapper{cluster: cluster, handle: h}
	return C.envoy_dynamic_module_type_cluster_module_ptr(clusterManager.record(w))
}

//export envoy_dynamic_module_on_cluster_init
func envoy_dynamic_module_on_cluster_init(
	_ C.envoy_dynamic_module_type_cluster_envoy_ptr,
	clusterModulePtr C.envoy_dynamic_module_type_cluster_module_ptr,
) {
	w := clusterManager.unwrap(unsafe.Pointer(clusterModulePtr))
	if w == nil {
		return
	}
	w.cluster.Init(w.handle)
}

//export envoy_dynamic_module_on_cluster_destroy
func envoy_dynamic_module_on_cluster_destroy(
	clusterModulePtr C.envoy_dynamic_module_type_cluster_module_ptr,
) {
	ptr := unsafe.Pointer(clusterModulePtr)
	w := clusterManager.unwrap(ptr)
	if w == nil {
		return
	}
	w.cluster.Close()
	w.handle.destroy()
	clusterManager.remove(ptr)
}

//export envoy_dynamic_module_on_cluster_server_initialized
func envoy_dynamic_module_on_cluster_server_initialized(
	_ C.envoy_dynamic_module_type_cluster_envoy_ptr,
	clusterModulePtr C.envoy_dynamic_module_type_cluster_module_ptr,
) {
	w := clusterManager.unwrap(unsafe.Pointer(clusterModulePtr))
	if w == nil {
		return
	}
	w.cluster.ServerInitialized(w.handle)
}

//export envoy_dynamic_module_on_cluster_drain_started
func envoy_dynamic_module_on_cluster_drain_started(
	_ C.envoy_dynamic_module_type_cluster_envoy_ptr,
	clusterModulePtr C.envoy_dynamic_module_type_cluster_module_ptr,
) {
	w := clusterManager.unwrap(unsafe.Pointer(clusterModulePtr))
	if w == nil {
		return
	}
	w.cluster.DrainStarted(w.handle)
}

//export envoy_dynamic_module_on_cluster_shutdown
func envoy_dynamic_module_on_cluster_shutdown(
	_ C.envoy_dynamic_module_type_cluster_envoy_ptr,
	clusterModulePtr C.envoy_dynamic_module_type_cluster_module_ptr,
	completionCallback C.envoy_dynamic_module_type_event_cb,
	completionContext unsafe.Pointer,
) {
	w := clusterManager.unwrap(unsafe.Pointer(clusterModulePtr))
	if w == nil {
		C.transit_call_event_cb(completionCallback, completionContext)
		return
	}
	cb := completionCallback
	ctx := completionContext
	w.cluster.Shutdown(w.handle, func() {
		C.transit_call_event_cb(cb, ctx)
	})
}

//export envoy_dynamic_module_on_cluster_scheduled
func envoy_dynamic_module_on_cluster_scheduled(
	_ C.envoy_dynamic_module_type_cluster_envoy_ptr,
	clusterModulePtr C.envoy_dynamic_module_type_cluster_module_ptr,
	eventID C.uint64_t,
) {
	w := clusterManager.unwrap(unsafe.Pointer(clusterModulePtr))
	if w == nil {
		return
	}
	w.handle.runPending(uint64(eventID))
}

//export envoy_dynamic_module_on_cluster_lb_new
func envoy_dynamic_module_on_cluster_lb_new(
	clusterModulePtr C.envoy_dynamic_module_type_cluster_module_ptr,
	lbEnvoyPtr C.envoy_dynamic_module_type_cluster_lb_envoy_ptr,
) C.envoy_dynamic_module_type_cluster_lb_module_ptr {
	cw := clusterManager.unwrap(unsafe.Pointer(clusterModulePtr))
	if cw == nil {
		return nil
	}
	lb := cw.cluster.NewClusterLB()
	if lb == nil {
		return nil
	}
	w := &clusterLBWrapper{lb: lb, lbEnvoy: lbEnvoyPtr}
	return C.envoy_dynamic_module_type_cluster_lb_module_ptr(clusterLBManager.record(w))
}

//export envoy_dynamic_module_on_cluster_lb_destroy
func envoy_dynamic_module_on_cluster_lb_destroy(
	lbModulePtr C.envoy_dynamic_module_type_cluster_lb_module_ptr,
) {
	ptr := unsafe.Pointer(lbModulePtr)
	w := clusterLBManager.unwrap(ptr)
	if w == nil {
		return
	}
	w.lb.Close()
	clusterLBManager.remove(ptr)
}

//export envoy_dynamic_module_on_cluster_lb_choose_host
func envoy_dynamic_module_on_cluster_lb_choose_host(
	lbModulePtr C.envoy_dynamic_module_type_cluster_lb_module_ptr,
	contextEnvoyPtr C.envoy_dynamic_module_type_cluster_lb_context_envoy_ptr,
	hostOut *C.envoy_dynamic_module_type_cluster_host_envoy_ptr,
	asyncHandleOut *C.envoy_dynamic_module_type_cluster_lb_async_handle_module_ptr,
) {
	*hostOut = nil
	*asyncHandleOut = nil

	w := clusterLBManager.unwrap(unsafe.Pointer(lbModulePtr))
	if w == nil {
		return
	}
	lbHandle := dymClusterLBHandle{lbPtr: w.lbEnvoy}
	ctx := dymClusterLBContext{ctxPtr: contextEnvoyPtr, lbPtr: w.lbEnvoy}

	host, completion := w.lb.ChooseHost(&lbHandle, &ctx)
	if completion != nil {
		aw := &asyncHandleWrapper{completion: completion}
		ptr := clusterAsyncManager.record(aw)
		completion.SetFinishFn(func() {
			clusterAsyncManager.remove(ptr)
		})
		*asyncHandleOut = C.envoy_dynamic_module_type_cluster_lb_async_handle_module_ptr(ptr)
		return
	}
	if host != nil {
		*hostOut = C.envoy_dynamic_module_type_cluster_host_envoy_ptr(
			unsafe.Pointer(host))
	}
}

//export envoy_dynamic_module_on_cluster_lb_cancel_host_selection
func envoy_dynamic_module_on_cluster_lb_cancel_host_selection(
	lbModulePtr C.envoy_dynamic_module_type_cluster_lb_module_ptr,
	asyncHandlePtr C.envoy_dynamic_module_type_cluster_lb_async_handle_module_ptr,
) {
	lbW := clusterLBManager.unwrap(unsafe.Pointer(lbModulePtr))
	if lbW == nil {
		return
	}
	ptr := unsafe.Pointer(asyncHandlePtr)
	aw := clusterAsyncManager.unwrap(ptr)
	if aw == nil {
		return
	}
	if aw.completion.Cancel() {
		lbW.lb.CancelHostSelection(aw.completion)
	}
}

//export envoy_dynamic_module_on_cluster_lb_on_host_membership_update
func envoy_dynamic_module_on_cluster_lb_on_host_membership_update(
	lbEnvoyPtr C.envoy_dynamic_module_type_cluster_lb_envoy_ptr,
	lbModulePtr C.envoy_dynamic_module_type_cluster_lb_module_ptr,
	numHostsAdded C.size_t,
	numHostsRemoved C.size_t,
) {
	w := clusterLBManager.unwrap(unsafe.Pointer(lbModulePtr))
	if w == nil {
		return
	}
	lbHandle := dymClusterLBHandle{lbPtr: lbEnvoyPtr}
	w.lb.OnHostMembershipUpdate(&lbHandle, int(numHostsAdded), int(numHostsRemoved))
}

// =============================================================================
// Stubs for optional ABI exports Envoy may call
// =============================================================================

//export envoy_dynamic_module_on_cluster_http_callout_done
func envoy_dynamic_module_on_cluster_http_callout_done(
	clusterEnvoyPtr C.envoy_dynamic_module_type_cluster_envoy_ptr,
	clusterModPtr C.envoy_dynamic_module_type_cluster_module_ptr,
	calloutID C.uint64_t,
	calloutResult C.envoy_dynamic_module_type_http_callout_result,
	respHeaders *C.envoy_dynamic_module_type_envoy_http_header,
	respHeadersSize C.size_t,
	bodyChunks *C.envoy_dynamic_module_type_envoy_buffer,
	bodyChunksSize C.size_t,
) {
	// Required symbol; transit does not use cluster HTTP callouts.
	// Envoy returns CannotCreateRequest if the module calls
	// envoy_dynamic_module_callback_cluster_http_callout without this hook.
}
