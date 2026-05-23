package abi_impl

/*
#include "abi.h"
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/dio/transit/down"
)

// =============================================================================
// Wrapper types
// =============================================================================

type lbConfigWrapper struct {
	configFactory down.LBPolicyConfigFactory
}

type lbWrapper struct {
	lb      down.LBPolicy
	lbEnvoy C.envoy_dynamic_module_type_lb_envoy_ptr
}

var (
	lbConfigManager = newManager[lbConfigWrapper]()
	lbManager       = newManager[lbWrapper]()
)

// =============================================================================
// dymLBHandle — implements down.LBHandle (stack-allocated per call)
// =============================================================================

type dymLBHandle struct {
	lbPtr C.envoy_dynamic_module_type_lb_envoy_ptr
}

var _ down.LBHandle = (*dymLBHandle)(nil)

func (h *dymLBHandle) ClusterName() string {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	C.envoy_dynamic_module_callback_lb_get_cluster_name(h.lbPtr, &buf)
	return envoyBufferToStringUnsafe(buf)
}

func (h *dymLBHandle) PriorityCount() int {
	return int(C.envoy_dynamic_module_callback_lb_get_priority_set_size(h.lbPtr))
}

func (h *dymLBHandle) HostCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_lb_get_hosts_count(h.lbPtr, C.uint32_t(priority)))
}

func (h *dymLBHandle) HealthyHostCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_lb_get_healthy_hosts_count(
		h.lbPtr, C.uint32_t(priority)))
}

func (h *dymLBHandle) DegradedHostCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_lb_get_degraded_hosts_count(
		h.lbPtr, C.uint32_t(priority)))
}

func (h *dymLBHandle) HostAddress(priority uint32, index int) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_lb_get_host_address(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymLBHandle) HealthyHostAddress(priority uint32, index int) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_lb_get_healthy_host_address(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymLBHandle) HostWeight(priority uint32, index int) uint32 {
	return uint32(C.envoy_dynamic_module_callback_lb_get_host_weight(
		h.lbPtr, C.uint32_t(priority), C.size_t(index)))
}

func (h *dymLBHandle) HealthyHostWeight(priority uint32, index int) uint32 {
	return uint32(C.envoy_dynamic_module_callback_lb_get_healthy_host_weight(
		h.lbPtr, C.uint32_t(priority), C.size_t(index)))
}

func (h *dymLBHandle) HostHealth(priority uint32, index int) down.HostHealth {
	return down.HostHealth(C.envoy_dynamic_module_callback_lb_get_host_health(
		h.lbPtr, C.uint32_t(priority), C.size_t(index)))
}

func (h *dymLBHandle) HostHealthByAddress(addr string) (down.HostHealth, bool) {
	var health C.envoy_dynamic_module_type_host_health
	ok := bool(C.envoy_dynamic_module_callback_lb_get_host_health_by_address(
		h.lbPtr, stringToModuleBuffer(addr), &health))
	runtime.KeepAlive(addr)
	return down.HostHealth(health), ok
}

func (h *dymLBHandle) HostStat(priority uint32, index int, stat down.HostStat) uint64 {
	return uint64(C.envoy_dynamic_module_callback_lb_get_host_stat(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		C.envoy_dynamic_module_type_host_stat(stat)))
}

func (h *dymLBHandle) MemberUpdateHostAddress(index int, isAdded bool) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_lb_get_member_update_host_address(
		h.lbPtr, C.size_t(index), C.bool(isAdded), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymLBHandle) HostLocality(priority uint32, index int) (string, string, string, bool) {
	var region, zone, subZone C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_lb_get_host_locality(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &region, &zone, &subZone))
	if !ok {
		return "", "", "", false
	}
	return envoyBufferToStringUnsafe(region),
		envoyBufferToStringUnsafe(zone),
		envoyBufferToStringUnsafe(subZone),
		true
}

func (h *dymLBHandle) SetHostData(priority uint32, index int, data uintptr) bool {
	return bool(C.envoy_dynamic_module_callback_lb_set_host_data(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), C.uintptr_t(data)))
}

func (h *dymLBHandle) GetHostData(priority uint32, index int) (uintptr, bool) {
	var data C.uintptr_t
	ok := bool(C.envoy_dynamic_module_callback_lb_get_host_data(
		h.lbPtr, C.uint32_t(priority), C.size_t(index), &data))
	return uintptr(data), ok
}

func (h *dymLBHandle) HostMetadataString(priority uint32, index int, filterName, key string) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_lb_get_host_metadata_string(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		stringToModuleBuffer(filterName), stringToModuleBuffer(key), &buf))
	runtime.KeepAlive(filterName)
	runtime.KeepAlive(key)
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymLBHandle) HostMetadataNumber(priority uint32, index int, filterName, key string) (float64, bool) {
	var out C.double
	ok := bool(C.envoy_dynamic_module_callback_lb_get_host_metadata_number(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		stringToModuleBuffer(filterName), stringToModuleBuffer(key), &out))
	runtime.KeepAlive(filterName)
	runtime.KeepAlive(key)
	return float64(out), ok
}

func (h *dymLBHandle) HostMetadataBool(priority uint32, index int, filterName, key string) (bool, bool) {
	var out C.bool
	ok := bool(C.envoy_dynamic_module_callback_lb_get_host_metadata_bool(
		h.lbPtr, C.uint32_t(priority), C.size_t(index),
		stringToModuleBuffer(filterName), stringToModuleBuffer(key), &out))
	runtime.KeepAlive(filterName)
	runtime.KeepAlive(key)
	return bool(out), ok
}

func (h *dymLBHandle) LocalityCount(priority uint32) int {
	return int(C.envoy_dynamic_module_callback_lb_get_locality_count(
		h.lbPtr, C.uint32_t(priority)))
}

func (h *dymLBHandle) LocalityHostCount(priority uint32, localityIndex int) int {
	return int(C.envoy_dynamic_module_callback_lb_get_locality_host_count(
		h.lbPtr, C.uint32_t(priority), C.size_t(localityIndex)))
}

func (h *dymLBHandle) LocalityHostAddress(priority uint32, localityIndex, hostIndex int) (string, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_lb_get_locality_host_address(
		h.lbPtr, C.uint32_t(priority), C.size_t(localityIndex), C.size_t(hostIndex), &buf))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(buf), true
}

func (h *dymLBHandle) LocalityWeight(priority uint32, localityIndex int) uint32 {
	return uint32(C.envoy_dynamic_module_callback_lb_get_locality_weight(
		h.lbPtr, C.uint32_t(priority), C.size_t(localityIndex)))
}

// =============================================================================
// dymLBContext — implements down.LBContext (stack-allocated per call)
// =============================================================================

type dymLBContext struct {
	ctxPtr C.envoy_dynamic_module_type_lb_context_envoy_ptr
	lbPtr  C.envoy_dynamic_module_type_lb_envoy_ptr
}

var _ down.LBContext = (*dymLBContext)(nil)

func (c *dymLBContext) GetAllHeaders() [][2]string {
	n := int(C.envoy_dynamic_module_callback_lb_context_get_downstream_headers_size(c.ctxPtr))
	if n == 0 {
		return nil
	}
	raw := make([]C.envoy_dynamic_module_type_envoy_http_header, n)
	ok := bool(C.envoy_dynamic_module_callback_lb_context_get_downstream_headers(c.ctxPtr, &raw[0]))
	if !ok {
		return nil
	}
	out := make([][2]string, n)
	for i, h := range raw {
		out[i] = envoyHTTPHeaderToPair(h)
	}
	return out
}

func (c *dymLBContext) GetOverrideHost() (string, bool) {
	var addrBuf C.envoy_dynamic_module_type_envoy_buffer
	var strict C.bool
	ok := bool(C.envoy_dynamic_module_callback_lb_context_get_override_host(
		c.ctxPtr, &addrBuf, &strict))
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(addrBuf), bool(strict)
}

func (c *dymLBContext) GetHeader(name string) (string, bool) {
	var val C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_lb_context_get_downstream_header(
		c.ctxPtr, stringToModuleBuffer(name), &val, 0, nil))
	runtime.KeepAlive(name)
	if !ok {
		return "", false
	}
	return envoyBufferToStringUnsafe(val), true
}

func (c *dymLBContext) ComputeHashKey() (uint64, bool) {
	var h C.uint64_t
	ok := bool(C.envoy_dynamic_module_callback_lb_context_compute_hash_key(c.ctxPtr, &h))
	return uint64(h), ok
}

func (c *dymLBContext) GetHostSelectionRetryCount() uint32 {
	return uint32(C.envoy_dynamic_module_callback_lb_context_get_host_selection_retry_count(
		c.ctxPtr))
}

func (c *dymLBContext) ShouldSelectAnotherHost(lb down.LBHandle, priority uint32, index int) bool {
	h, ok := lb.(*dymLBHandle)
	if !ok {
		return false
	}
	return bool(C.envoy_dynamic_module_callback_lb_context_should_select_another_host(
		h.lbPtr, c.ctxPtr, C.uint32_t(priority), C.size_t(index)))
}

// =============================================================================
// ABI export functions
// =============================================================================

//export envoy_dynamic_module_on_lb_config_new
func envoy_dynamic_module_on_lb_config_new(
	_ C.envoy_dynamic_module_type_lb_config_envoy_ptr,
	name C.envoy_dynamic_module_type_envoy_buffer,
	config C.envoy_dynamic_module_type_envoy_buffer,
) C.envoy_dynamic_module_type_lb_config_module_ptr {
	nameStr := envoyBufferToStringUnsafe(name)
	factory := down.GetLBPolicyFactory(nameStr)
	if factory == nil {
		fmt.Fprintf(os.Stderr, "[transit] lb policy %q: no factory registered\n", nameStr)
		return nil
	}
	configFactory, err := factory.Create(envoyBufferToBytesUnsafe(config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[transit] lb policy %q: Create failed: %v\n", nameStr, err)
		return nil
	}
	w := &lbConfigWrapper{configFactory: configFactory}
	return C.envoy_dynamic_module_type_lb_config_module_ptr(lbConfigManager.record(w))
}

//export envoy_dynamic_module_on_lb_config_destroy
func envoy_dynamic_module_on_lb_config_destroy(
	configModulePtr C.envoy_dynamic_module_type_lb_config_module_ptr,
) {
	ptr := unsafe.Pointer(configModulePtr)
	w := lbConfigManager.unwrap(ptr)
	if w == nil {
		return
	}
	w.configFactory.Close()
	lbConfigManager.remove(ptr)
}

//export envoy_dynamic_module_on_lb_new
func envoy_dynamic_module_on_lb_new(
	configModulePtr C.envoy_dynamic_module_type_lb_config_module_ptr,
	lbEnvoyPtr C.envoy_dynamic_module_type_lb_envoy_ptr,
) C.envoy_dynamic_module_type_lb_module_ptr {
	cw := lbConfigManager.unwrap(unsafe.Pointer(configModulePtr))
	if cw == nil {
		return nil
	}
	lb := cw.configFactory.NewLBPolicy()
	if lb == nil {
		return nil
	}
	w := &lbWrapper{lb: lb, lbEnvoy: lbEnvoyPtr}
	return C.envoy_dynamic_module_type_lb_module_ptr(lbManager.record(w))
}

//export envoy_dynamic_module_on_lb_destroy
func envoy_dynamic_module_on_lb_destroy(
	lbModulePtr C.envoy_dynamic_module_type_lb_module_ptr,
) {
	ptr := unsafe.Pointer(lbModulePtr)
	w := lbManager.unwrap(ptr)
	if w == nil {
		return
	}
	w.lb.Close()
	lbManager.remove(ptr)
}

//export envoy_dynamic_module_on_lb_choose_host
func envoy_dynamic_module_on_lb_choose_host(
	lbEnvoyPtr C.envoy_dynamic_module_type_lb_envoy_ptr,
	lbModulePtr C.envoy_dynamic_module_type_lb_module_ptr,
	contextEnvoyPtr C.envoy_dynamic_module_type_lb_context_envoy_ptr,
	resultPriority *C.uint32_t,
	resultIndex *C.uint32_t,
) C.bool {
	w := lbManager.unwrap(unsafe.Pointer(lbModulePtr))
	if w == nil {
		return false
	}
	lbHandle := dymLBHandle{lbPtr: lbEnvoyPtr}
	ctx := dymLBContext{ctxPtr: contextEnvoyPtr, lbPtr: lbEnvoyPtr}

	var priority, index uint32
	ok := w.lb.ChooseHost(&lbHandle, &ctx, &priority, &index)
	if ok {
		*resultPriority = C.uint32_t(priority)
		*resultIndex = C.uint32_t(index)
	}
	return C.bool(ok)
}

//export envoy_dynamic_module_on_lb_on_host_membership_update
func envoy_dynamic_module_on_lb_on_host_membership_update(
	lbEnvoyPtr C.envoy_dynamic_module_type_lb_envoy_ptr,
	lbModulePtr C.envoy_dynamic_module_type_lb_module_ptr,
	numHostsAdded C.size_t,
	numHostsRemoved C.size_t,
) {
	w := lbManager.unwrap(unsafe.Pointer(lbModulePtr))
	if w == nil {
		return
	}
	lbHandle := dymLBHandle{lbPtr: lbEnvoyPtr}
	w.lb.OnHostMembershipUpdate(&lbHandle, int(numHostsAdded), int(numHostsRemoved))
}
