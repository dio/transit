# transit-bridge: multi-.so composition

**Status: design stub — not yet implemented**

---

## Problem

Envoy loads one dynamic module `.so` per filter entry. Users who want to ship
business logic separately from the core filter — different team, different
release cycle, different language — have no way to plug in without recompiling
the main `.so`.

Go's `plugin.Open` does not work here: each `-buildmode=c-shared` binary
carries its own Go runtime and its own copy of every imported package's
globals. A handler registered in `libuser.so`'s `up.registry` is invisible
to `libmain.so`'s `up.registry` — they are separate instances.

---

## Solution: C ABI callback bridge

```
Envoy
  └─ dlopen(libmain.so)            ← transit + compiled-in filters
       └─ dlopen(libuser.so)       ← user's .so, any language
            └─ transit_module_init(&register_table)
                 └─ register_table.register_filter(...)
                      └─ lands in libmain.so's Go registry
```

`libmain.so` opens `libuser.so` and calls a single well-known C symbol,
passing a pointer to a `transit_register_table_t` — a struct of function
pointers into `libmain.so`'s own `Register*` functions. The user's `.so` calls
those pointers; registrations land in the same Go registry as compiled-in
filters.

No `RTLD_GLOBAL` dependency. No shared Go runtime. The C struct is the
entire contract.

---

## C ABI

### Symbol the user's `.so` must export

```c
// Called once by libmain.so after dlopen. Table is valid for the process lifetime.
void transit_module_init(const transit_register_table_t* table);
```

### `transit_register_table_t`

The five entries mirror transit's five Go registration functions exactly.

```c
typedef void (*transit_filter_handler_fn)(transit_writer_t*, transit_request_t*);
typedef void (*transit_request_body_handler_fn)(transit_writer_t*, transit_body_chunk_t*);
typedef void (*transit_response_handler_fn)(transit_writer_t*, transit_response_chunk_t*);
// Flat on_log callback — see factory-simplification note below.
typedef void (*transit_access_logger_on_log_fn)(transit_access_logger_handle_t*, int32_t log_type);

typedef struct {
    uint32_t version; // forward-compatibility; current value: 1

    // Mirrors up.Register — request-phase only.
    void (*register_filter)(const char* name, transit_filter_handler_fn handler);

    // Mirrors up.RegisterWithResponse — request + response headers/body.
    void (*register_with_response)(const char* name,
        transit_filter_handler_fn req, transit_response_handler_fn resp);

    // Mirrors up.RegisterWithBody — streaming request body (one call per chunk).
    void (*register_with_body)(const char* name,
        transit_filter_handler_fn req, transit_request_body_handler_fn body,
        transit_response_handler_fn resp);

    // Mirrors up.RegisterWithMutableBody — buffered (full) request body.
    // SetRequestBody / SetResponseBody replace body content after handler returns.
    void (*register_with_mutable_body)(const char* name,
        transit_filter_handler_fn req, transit_request_body_handler_fn body,
        transit_response_handler_fn resp);

    // Mirrors up.RegisterAccessLogger.
    // config_bytes/config_len carry the raw YAML logger_config field.
    void (*register_access_logger)(const char* name,
        transit_access_logger_on_log_fn on_log,
        const uint8_t* config_bytes, size_t config_len);
} transit_register_table_t;
```

> **Access logger factory simplification.** Go exposes a 3-tier hierarchy
> (`AccessLoggerConfigFactory → AccessLoggerFactory → AccessLogger`) to support
> per-worker instances and metric definition at config time. Over C ABI this
> hierarchy adds complexity with little payoff for most users. The bridge wraps
> `on_log` in a trivial Go `AccessLoggerConfigFactory` that ignores the config
> handle and vends a single shared logger. Users needing per-worker state or
> metric definition must compile their logger in Go and call
> `RegisterAccessLogger` directly.

---

## `transit.h` accessor functions

Opaque handle types and accessors are declared in `transit.h`, distributed
alongside `libmain.so`. Returned `const char*` pointers are valid only for the
duration of the callback.

```c
// ── Writer (transit_writer_t) ────────────────────────────────────────────────

void transit_writer_send_local_response(transit_writer_t* w, uint32_t status,
    const uint8_t* body, size_t body_len,
    const char** header_names, const char** header_values, size_t header_count);

// Request header mutation (valid during request-phase callbacks).
void transit_writer_set_request_header(transit_writer_t* w, const char* name, const char* value);
void transit_writer_add_request_header(transit_writer_t* w, const char* name, const char* value);
void transit_writer_remove_request_header(transit_writer_t* w, const char* name);

// Response header mutation (valid during response-phase callbacks).
void transit_writer_set_response_header(transit_writer_t* w, const char* name, const char* value);
void transit_writer_add_response_header(transit_writer_t* w, const char* name, const char* value);
void transit_writer_remove_response_header(transit_writer_t* w, const char* name);

// Body replacement — only effective with register_with_mutable_body.
void transit_writer_set_request_body(transit_writer_t* w, const uint8_t* data, size_t len);
void transit_writer_set_response_body(transit_writer_t* w, const uint8_t* data, size_t len);

int  transit_writer_get_attribute_string(transit_writer_t* w, int32_t attr_id, const char** out, size_t* out_len);
int  transit_writer_get_attribute_number(transit_writer_t* w, int32_t attr_id, double* out);
int  transit_writer_get_attribute_bool(transit_writer_t* w, int32_t attr_id, int* out);
void transit_writer_log(transit_writer_t* w, int32_t level, const char* msg);

// ── BodyChunk (transit_body_chunk_t) ─────────────────────────────────────────

const uint8_t* transit_body_chunk_data(const transit_body_chunk_t* c, size_t* out_len);
int            transit_body_chunk_end_stream(const transit_body_chunk_t* c);
const char*    transit_body_chunk_content_encoding(const transit_body_chunk_t* c);
const char*    transit_body_chunk_content_type(const transit_body_chunk_t* c);

// ── ResponseChunk (transit_response_chunk_t) ─────────────────────────────────
// status_code is non-zero on the headers call; data is set on body calls.

int            transit_response_chunk_status_code(const transit_response_chunk_t* c);
const uint8_t* transit_response_chunk_data(const transit_response_chunk_t* c, size_t* out_len);
int            transit_response_chunk_end_stream(const transit_response_chunk_t* c);
const char*    transit_response_chunk_content_encoding(const transit_response_chunk_t* c);
const char*    transit_response_chunk_content_type(const transit_response_chunk_t* c);
size_t         transit_response_chunk_headers(const transit_response_chunk_t* c,
                   const char** names_out, const char** values_out, size_t cap);

// ── AccessLoggerHandle (transit_access_logger_handle_t) ──────────────────────

typedef struct {
    int64_t start_time_unix_ns, request_complete_duration_ns;
    int64_t first_upstream_tx_byte_sent_ns, last_upstream_tx_byte_sent_ns;
    int64_t first_upstream_rx_byte_received_ns, last_upstream_rx_byte_received_ns;
    int64_t first_downstream_tx_byte_sent_ns, last_downstream_tx_byte_sent_ns;
} transit_timing_info_t; // -1 means unavailable

typedef struct {
    uint64_t bytes_received, bytes_sent, wire_bytes_received, wire_bytes_sent;
} transit_bytes_info_t;

void     transit_al_get_timing_info(const transit_access_logger_handle_t* h, transit_timing_info_t* out);
void     transit_al_get_bytes_info(const transit_access_logger_handle_t* h, transit_bytes_info_t* out);
uint64_t transit_al_get_response_flags(const transit_access_logger_handle_t* h);
uint32_t transit_al_get_response_code(const transit_access_logger_handle_t* h);
// header_type: 0=request 1=request-trailer 2=response 3=response-trailer
int      transit_al_get_header(const transit_access_logger_handle_t* h,
             int32_t header_type, const char* key, const char** out, size_t* out_len);
int      transit_al_get_attribute_string(const transit_access_logger_handle_t* h,
             int32_t attr_id, const char** out, size_t* out_len);
int      transit_al_get_trace_id(const transit_access_logger_handle_t* h, const char** out, size_t* out_len);
int      transit_al_get_span_id(const transit_access_logger_handle_t* h, const char** out, size_t* out_len);
int      transit_al_is_trace_sampled(const transit_access_logger_handle_t* h);
uint32_t transit_al_get_worker_index(const transit_access_logger_handle_t* h);
void     transit_al_log(const transit_access_logger_handle_t* h, int32_t level, const char* msg);
```

---

## What transit needs to add (future work)

1. **`transit.h`** — the C header above, distributed alongside `libmain.so`.

2. **Exported accessor symbols** — Go functions with `//export` in
   `down/abi_impl/` implementing every declaration in `transit.h`.

3. **Module loader** — reads `TRANSIT_EXTRA_MODULES` at startup (or a field in
   the filter config), calls `dlopen` + `transit_module_init` for each path.

   ```
   TRANSIT_EXTRA_MODULES=/usr/lib/libuser.so:/opt/libcompany.so
   ```

4. **`RegisterTable` builder** — constructs `transit_register_table_t` with
   live function pointers before calling `transit_module_init`.

---

## User's `.so` in Go (compiled as c-shared)

```go
package main

import "C"
import "unsafe"

//export transit_module_init
func transit_module_init(table unsafe.Pointer) {
    t := (*C.transit_register_table_t)(table)
    name := C.CString("my-filter")
    defer C.free(unsafe.Pointer(name))
    C.call_register_filter(t, name, C.transit_filter_handler_fn(C.my_filter_handler))
}

//export my_filter_handler
func my_filter_handler(w, r unsafe.Pointer) {
    // Use transit.h accessors; e.g. C.transit_writer_set_request_header(w, ...)
}

func main() {} // required for c-shared
```

## User's `.so` in Rust

```rust
#[repr(C)]
pub struct RegisterTable {
    version: u32,
    register_filter: unsafe extern "C" fn(*const i8, FilterHandlerFn),
    // ... remaining fields
}
type FilterHandlerFn = unsafe extern "C" fn(*mut Writer, *mut Request);

#[no_mangle]
pub unsafe extern "C" fn transit_module_init(table: *const RegisterTable) {
    ((*table).register_filter)(b"my-filter\0".as_ptr() as _, my_filter_handler);
}

unsafe extern "C" fn my_filter_handler(w: *mut Writer, _r: *mut Request) {
    // Use transit.h accessors; e.g. transit_writer_log(w, LOG_INFO, b"hi\0".as_ptr() as _);
}
```

---

## Key design properties

- **Language-agnostic**: any language that can export C symbols works.
- **No `RTLD_GLOBAL`**: the register table is passed explicitly.
- **Same registry**: each `register_*` pointer calls directly into
  `libmain.so`'s `up.Register*` — the Go registry is shared by construction.
- **Stable ABI surface**: `transit_register_table_t` and `transit.h` are the
  only contract; internal Go types can change freely behind the accessors.
- **Release decoupling**: `libuser.so` ships independently as long as it
  targets the same table `version`. The field enables forward-compatibility
  checks at `transit_module_init` time.
- **Access logger trade-off**: the 3-tier Go factory hierarchy is flattened to
  a single `on_log` callback over C ABI. Users needing per-worker state or
  metric definition compile their logger in Go and call `RegisterAccessLogger`
  directly.
