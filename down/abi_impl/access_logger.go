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

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	"github.com/dio/transit/down"
)

// accessLoggerConfigWrapper holds the per-config factory and the Envoy pointer.
// Created on the main thread in envoy_dynamic_module_on_access_logger_config_new.
type accessLoggerConfigWrapper struct {
	factory   down.AccessLoggerFactory
	configPtr C.envoy_dynamic_module_type_access_logger_config_envoy_ptr
}

// accessLoggerWrapper holds one per-worker AccessLogger instance.
type accessLoggerWrapper struct {
	logger down.AccessLogger
}

var (
	accessLoggerConfigManager = newManager[accessLoggerConfigWrapper]()
	accessLoggerManager       = newManager[accessLoggerWrapper]()
)

// =============================================================================
// dymAccessLoggerConfigHandle — implements down.AccessLoggerConfigHandle
// =============================================================================

type dymAccessLoggerConfigHandle struct {
	configPtr C.envoy_dynamic_module_type_access_logger_config_envoy_ptr
}

func (h *dymAccessLoggerConfigHandle) Log(level shared.LogLevel, format string, args ...any) {
	hostLog(level, format, args)
}

func (h *dymAccessLoggerConfigHandle) DefineCounter(name string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	var id C.size_t
	result := C.envoy_dynamic_module_callback_access_logger_config_define_counter(
		h.configPtr, stringToModuleBuffer(name), &id,
	)
	runtime.KeepAlive(name)
	return shared.MetricID(id), shared.MetricsResult(result)
}

func (h *dymAccessLoggerConfigHandle) DefineGauge(name string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	var id C.size_t
	result := C.envoy_dynamic_module_callback_access_logger_config_define_gauge(
		h.configPtr, stringToModuleBuffer(name), &id,
	)
	runtime.KeepAlive(name)
	return shared.MetricID(id), shared.MetricsResult(result)
}

func (h *dymAccessLoggerConfigHandle) DefineHistogram(name string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	var id C.size_t
	result := C.envoy_dynamic_module_callback_access_logger_config_define_histogram(
		h.configPtr, stringToModuleBuffer(name), &id,
	)
	runtime.KeepAlive(name)
	return shared.MetricID(id), shared.MetricsResult(result)
}

// =============================================================================
// dymAccessLoggerHandle — implements down.AccessLoggerHandle
// =============================================================================

// dymAccessLoggerHandle wraps the per-log-event Envoy pointer.
// Stack-allocated per OnLog call; must not escape the callback.
type dymAccessLoggerHandle struct {
	envoyPtr C.envoy_dynamic_module_type_access_logger_envoy_ptr
}

func (h *dymAccessLoggerHandle) Log(level shared.LogLevel, format string, args ...any) {
	hostLog(level, format, args)
}

func (h *dymAccessLoggerHandle) GetTimingInfo() down.TimingInfo {
	var out C.envoy_dynamic_module_type_timing_info
	C.envoy_dynamic_module_callback_access_logger_get_timing_info(h.envoyPtr, &out)
	return down.TimingInfo{
		StartTimeUnixNs:               int64(out.start_time_unix_ns),
		RequestCompleteDurationNs:     int64(out.request_complete_duration_ns),
		FirstUpstreamTxByteSentNs:     int64(out.first_upstream_tx_byte_sent_ns),
		LastUpstreamTxByteSentNs:      int64(out.last_upstream_tx_byte_sent_ns),
		FirstUpstreamRxByteReceivedNs: int64(out.first_upstream_rx_byte_received_ns),
		LastUpstreamRxByteReceivedNs:  int64(out.last_upstream_rx_byte_received_ns),
		FirstDownstreamTxByteSentNs:   int64(out.first_downstream_tx_byte_sent_ns),
		LastDownstreamTxByteSentNs:    int64(out.last_downstream_tx_byte_sent_ns),
	}
}

func (h *dymAccessLoggerHandle) GetBytesInfo() down.BytesInfo {
	var out C.envoy_dynamic_module_type_bytes_info
	C.envoy_dynamic_module_callback_access_logger_get_bytes_info(h.envoyPtr, &out)
	return down.BytesInfo{
		BytesReceived:     uint64(out.bytes_received),
		BytesSent:         uint64(out.bytes_sent),
		WireBytesReceived: uint64(out.wire_bytes_received),
		WireBytesSent:     uint64(out.wire_bytes_sent),
	}
}

func (h *dymAccessLoggerHandle) GetResponseFlags() uint64 {
	return uint64(C.envoy_dynamic_module_callback_access_logger_get_response_flags(h.envoyPtr))
}

func (h *dymAccessLoggerHandle) GetResponseCode() uint32 {
	return uint32(C.envoy_dynamic_module_callback_access_logger_get_response_code(h.envoyPtr))
}

func (h *dymAccessLoggerHandle) GetAttributeString(id shared.AttributeID) (shared.UnsafeEnvoyBuffer, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_access_logger_get_attribute_string(
		h.envoyPtr,
		C.envoy_dynamic_module_type_attribute_id(id),
		&buf,
	))
	if !ok || buf.ptr == nil {
		return shared.UnsafeEnvoyBuffer{}, false
	}
	return envoyBufferToUnsafeEnvoyBuffer(buf), true
}

func (h *dymAccessLoggerHandle) GetAttributeInt(id shared.AttributeID) (int64, bool) {
	var val C.uint64_t
	ok := bool(C.envoy_dynamic_module_callback_access_logger_get_attribute_int(
		h.envoyPtr,
		C.envoy_dynamic_module_type_attribute_id(id),
		&val,
	))
	if !ok {
		return 0, false
	}
	return int64(val), true
}

func (h *dymAccessLoggerHandle) GetAttributeBool(id shared.AttributeID) (bool, bool) {
	var val C.bool
	ok := bool(C.envoy_dynamic_module_callback_access_logger_get_attribute_bool(
		h.envoyPtr,
		C.envoy_dynamic_module_type_attribute_id(id),
		&val,
	))
	if !ok {
		return false, false
	}
	return bool(val), true
}

func (h *dymAccessLoggerHandle) GetHeader(headerType down.HttpHeaderType, key string) (shared.UnsafeEnvoyBuffer, bool) {
	keyBuf := stringToModuleBuffer(key)
	var val C.envoy_dynamic_module_type_envoy_buffer
	ok := bool(C.envoy_dynamic_module_callback_access_logger_get_header_value(
		h.envoyPtr,
		C.envoy_dynamic_module_type_http_header_type(headerType),
		keyBuf,
		&val,
		0,
		nil,
	))
	runtime.KeepAlive(key)
	if !ok {
		return shared.UnsafeEnvoyBuffer{}, false
	}
	return envoyBufferToUnsafeEnvoyBuffer(val), true
}

func (h *dymAccessLoggerHandle) GetWorkerIndex() uint32 {
	return uint32(C.envoy_dynamic_module_callback_access_logger_get_worker_index(h.envoyPtr))
}

func (h *dymAccessLoggerHandle) GetTraceID() (shared.UnsafeEnvoyBuffer, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	if !bool(C.envoy_dynamic_module_callback_access_logger_get_trace_id(h.envoyPtr, &buf)) {
		return shared.UnsafeEnvoyBuffer{}, false
	}
	return envoyBufferToUnsafeEnvoyBuffer(buf), true
}

func (h *dymAccessLoggerHandle) GetSpanID() (shared.UnsafeEnvoyBuffer, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	if !bool(C.envoy_dynamic_module_callback_access_logger_get_span_id(h.envoyPtr, &buf)) {
		return shared.UnsafeEnvoyBuffer{}, false
	}
	return envoyBufferToUnsafeEnvoyBuffer(buf), true
}

func (h *dymAccessLoggerHandle) IsTraceSampled() bool {
	return bool(C.envoy_dynamic_module_callback_access_logger_is_trace_sampled(h.envoyPtr))
}

func (h *dymAccessLoggerHandle) GetLocalReplyBody() (shared.UnsafeEnvoyBuffer, bool) {
	var buf C.envoy_dynamic_module_type_envoy_buffer
	if !bool(C.envoy_dynamic_module_callback_access_logger_get_local_reply_body(h.envoyPtr, &buf)) {
		return shared.UnsafeEnvoyBuffer{}, false
	}
	return envoyBufferToUnsafeEnvoyBuffer(buf), true
}

func (h *dymAccessLoggerHandle) GetUpstreamPoolReadyDurationNs() int64 {
	return int64(C.envoy_dynamic_module_callback_access_logger_get_upstream_pool_ready_duration_ns(h.envoyPtr))
}

func (h *dymAccessLoggerHandle) GetUpstreamRequestAttemptCount() uint32 {
	var val C.uint64_t
	ok := C.envoy_dynamic_module_callback_access_logger_get_attribute_int(
		h.envoyPtr,
		C.envoy_dynamic_module_type_attribute_id(shared.AttributeIDUpstreamRequestAttemptCount),
		&val,
	)
	if !bool(ok) || val == 0 {
		return 1
	}
	return uint32(val)
}

// =============================================================================
// ABI export functions: called by Envoy
// =============================================================================

//export envoy_dynamic_module_on_access_logger_config_new
func envoy_dynamic_module_on_access_logger_config_new(
	configEnvoyPtr C.envoy_dynamic_module_type_access_logger_config_envoy_ptr,
	name C.envoy_dynamic_module_type_envoy_buffer,
	config C.envoy_dynamic_module_type_envoy_buffer,
) C.envoy_dynamic_module_type_access_logger_config_module_ptr {
	nameStr := envoyBufferToStringUnsafe(name)
	configBytes := envoyBufferToBytesUnsafe(config)

	configFactory := down.GetAccessLoggerConfigFactory(nameStr)
	if configFactory == nil {
		fmt.Fprintf(os.Stderr, "[transit] access logger %q: no factory registered\n", nameStr)
		return nil
	}

	handle := &dymAccessLoggerConfigHandle{configPtr: configEnvoyPtr}
	factory, err := configFactory.Create(handle, configBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[transit] access logger %q: Create failed: %v\n", nameStr, err)
		return nil
	}

	wrapper := &accessLoggerConfigWrapper{
		factory:   factory,
		configPtr: configEnvoyPtr,
	}
	return C.envoy_dynamic_module_type_access_logger_config_module_ptr(
		accessLoggerConfigManager.record(wrapper))
}

//export envoy_dynamic_module_on_access_logger_config_destroy
func envoy_dynamic_module_on_access_logger_config_destroy(
	configModulePtr C.envoy_dynamic_module_type_access_logger_config_module_ptr,
) {
	ptr := unsafe.Pointer(configModulePtr)
	wrapper := accessLoggerConfigManager.unwrap(ptr)
	if wrapper == nil {
		return
	}
	wrapper.factory.OnDestroy()
	accessLoggerConfigManager.remove(ptr)
}

//export envoy_dynamic_module_on_access_logger_new
func envoy_dynamic_module_on_access_logger_new(
	configModulePtr C.envoy_dynamic_module_type_access_logger_config_module_ptr,
	_ C.envoy_dynamic_module_type_access_logger_envoy_ptr,
) C.envoy_dynamic_module_type_access_logger_module_ptr {
	wrapper := accessLoggerConfigManager.unwrap(unsafe.Pointer(configModulePtr))
	if wrapper == nil {
		return nil
	}
	logger := wrapper.factory.NewLogger()
	if logger == nil {
		return nil
	}
	w := &accessLoggerWrapper{logger: logger}
	return C.envoy_dynamic_module_type_access_logger_module_ptr(
		accessLoggerManager.record(w))
}

//export envoy_dynamic_module_on_access_logger_log
func envoy_dynamic_module_on_access_logger_log(
	loggerEnvoyPtr C.envoy_dynamic_module_type_access_logger_envoy_ptr,
	loggerModulePtr C.envoy_dynamic_module_type_access_logger_module_ptr,
	logType C.envoy_dynamic_module_type_access_log_type,
) {
	wrapper := accessLoggerManager.unwrap(unsafe.Pointer(loggerModulePtr))
	if wrapper == nil {
		return
	}
	// Stack-allocate: handle is valid for this call only and must not escape.
	handle := dymAccessLoggerHandle{envoyPtr: loggerEnvoyPtr}
	wrapper.logger.OnLog(&handle, down.AccessLogType(logType))
}

//export envoy_dynamic_module_on_access_logger_destroy
func envoy_dynamic_module_on_access_logger_destroy(
	loggerModulePtr C.envoy_dynamic_module_type_access_logger_module_ptr,
) {
	ptr := unsafe.Pointer(loggerModulePtr)
	wrapper := accessLoggerManager.unwrap(ptr)
	if wrapper == nil {
		return
	}
	wrapper.logger.OnDestroy()
	accessLoggerManager.remove(ptr)
}

//export envoy_dynamic_module_on_access_logger_flush
func envoy_dynamic_module_on_access_logger_flush(
	loggerModulePtr C.envoy_dynamic_module_type_access_logger_module_ptr,
) {
	wrapper := accessLoggerManager.unwrap(unsafe.Pointer(loggerModulePtr))
	if wrapper == nil {
		return
	}
	type flusher interface{ Flush() }
	if f, ok := wrapper.logger.(flusher); ok {
		f.Flush()
	}
}
