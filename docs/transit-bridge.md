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
to `libe2e.so`'s `up.registry` — they are separate instances.

---

## Solution: C ABI callback bridge

```
Envoy
  └─ dlopen(libe2e.so)            ← transit + compiled-in filters
       └─ dlopen(libuser.so)      ← user's .so, any language
            └─ transit_module_init(&register_table)
                 └─ register_table.register_filter(...)
                      └─ lands in libe2e.so's Go registry
```

`libe2e.so` opens `libuser.so` and calls a single well-known C symbol,
passing a pointer to a `RegisterTable` — a struct of function pointers into
`libe2e.so`'s own `Register*` functions. The user's `.so` calls those
pointers; registrations land in the same Go registry as compiled-in filters.

No `RTLD_GLOBAL` dependency. No shared Go runtime. The C struct is the
entire contract.

---

## C ABI

### Symbol the user's `.so` must export

```c
// Called once by libe2e.so after dlopen. The table is valid for the lifetime
// of the process. Must be safe to call before any Envoy filter traffic.
void transit_module_init(const transit_register_table_t* table);
```

### `transit_register_table_t`

```c
typedef void (*transit_filter_handler_fn)(
    transit_writer_t* w, transit_request_t* r);

typedef void (*transit_request_body_handler_fn)(
    transit_writer_t* w, transit_body_chunk_t* chunk);

typedef void (*transit_response_handler_fn)(
    transit_writer_t* w, transit_response_chunk_t* chunk);

typedef void (*transit_access_logger_on_log_fn)(
    transit_access_logger_handle_t* h, int32_t log_type);

typedef struct {
    // Register an HTTP filter (request-side only).
    void (*register_filter)(
        const char* name,
        transit_filter_handler_fn handler);

    // Register an HTTP filter with response body callbacks.
    void (*register_with_body)(
        const char* name,
        transit_filter_handler_fn req_handler,
        transit_request_body_handler_fn req_body_handler,
        transit_response_handler_fn resp_handler,
        int buffered);   // 0 = streaming, 1 = buffered

    // Register an access logger.
    void (*register_access_logger)(
        const char* name,
        transit_access_logger_on_log_fn on_log);
} transit_register_table_t;
```

The `transit_writer_t`, `transit_request_t`, `transit_body_chunk_t`, etc.
are C-visible opaque handles backed by Go pointers. A companion C header
(`transit.h`) exports accessor functions (`transit_writer_get_attribute`,
`transit_chunk_get_data`, etc.) so the user's `.so` can read and write
request/response state without knowing Go internals.

---

## What transit needs to add (future work)

1. **`transit.h`** — C header with all opaque handle types and accessor
   function declarations. Distributed alongside `libe2e.so`.

2. **Exported accessor symbols** — Go functions with `//export` that
   implement the accessors declared in `transit.h`. These are the bridge
   between C callbacks and Go's `Writer`, `BodyChunk`, etc.

3. **Module loader** — reads `TRANSIT_EXTRA_MODULES` env var (or a field in
   the filter config) at startup, calls `dlopen` + `transit_module_init` for
   each listed path.

   ```
   TRANSIT_EXTRA_MODULES=/usr/lib/libuser.so:/opt/libcompany.so
   ```

4. **`RegisterTable` builder** — constructs the `transit_register_table_t`
   with live function pointers before calling `transit_module_init`.

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
    C.call_register_filter(t, name, C.transit_filter_handler_fn(
        C.my_filter_handler,
    ))
}

//export my_filter_handler
func my_filter_handler(w, r unsafe.Pointer) {
    // use transit.h accessor fns to read/write via w and r
}

func main() {} // required for c-shared
```

## User's `.so` in Rust

```rust
#[no_mangle]
pub extern "C" fn transit_module_init(table: *const RegisterTable) {
    unsafe {
        ((*table).register_filter)(
            b"my-filter\0".as_ptr() as _,
            my_filter_handler,
        );
    }
}

extern "C" fn my_filter_handler(w: *mut Writer, r: *mut Request) {
    // use transit.h accessor fns
}
```

---

## Key design properties

- **Language-agnostic**: any language that can export C symbols works.
- **No `RTLD_GLOBAL`**: the register table is passed explicitly; no global
  symbol visibility needed.
- **Same registry**: `register_table.register_filter` is a function pointer
  into `libe2e.so`'s own `up.Register` — the Go registry is shared by
  construction, not by accident.
- **Stable ABI surface**: `transit_register_table_t` and `transit.h` are the
  only contract. Internal Go types can change freely.
- **Release decoupling**: `libuser.so` ships independently as long as it
  targets the same `transit_register_table_t` version. A `uint32_t version`
  field at the top of the struct allows forward compatibility checks.
