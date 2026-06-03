# Bounded SNI-scoped upstream TLS session cache

## Answer: do we have the config surface today?

Partially.

Envoy already has this upstream TLS knob:

```proto
// Maximum number of session keys (Pre-Shared Keys for TLSv1.3+, Session IDs and Session Tickets
// for TLSv1.2 and older) to be stored for session resumption.
//
// Defaults to 1, setting this to 0 disables session resumption.
google.protobuf.UInt32Value max_session_keys = 4;
```

But this is a **single global cache bound per `ClientContextImpl`**. It does
not scope cached client TLS sessions by the effective upstream SNI. That is the
gap: one upstream TLS context can talk to multiple SNI names when
`auto_host_sni` or `serverNameOverride()` is used, but the session cache is not
partitioned by that name.

`UpstreamTlsContext` currently has `[#next-free-field: 8]`, so a compatible API
extension can add fields `8` and `9`.

## Problem

Today `ClientContextImpl` has one deque:

```cpp
std::deque<bssl::UniquePtr<SSL_SESSION>> session_keys_;
```

On a new upstream TLS connection, Envoy uses the most recent session key from
that deque, regardless of which SNI the new connection will use.

That is fine when a TLS context has a single static SNI. It is unsafe when a
single TLS context can connect to multiple upstream server names, for example:

```yaml
auto_host_sni: true
auto_sni_san_validation: true
```

In that setup, session resumption must not cross SNI boundaries.

## Proposed config API

Add optional SNI-scoped cache bounds to `UpstreamTlsContext`:

```proto
// Maximum number of distinct upstream SNI names for which client TLS sessions
// may be cached.
//
// This only affects SNI-scoped session caching. When unset, Envoy derives a
// conservative default from max_session_keys.
google.protobuf.UInt32Value max_session_sni_names = 8;

// Maximum number of client TLS sessions cached for one upstream SNI name.
//
// This only affects SNI-scoped session caching. When unset, Envoy uses a
// conservative default of 1.
google.protobuf.UInt32Value max_session_keys_per_sni = 9;
```

Suggested semantics:

- `max_session_keys: 0` still disables upstream client TLS session caching.
- Static-SNI contexts may keep the existing global cache behavior.
- Dynamic-SNI contexts use the SNI-scoped cache.
- `max_session_sni_names` bounds the number of SNI buckets.
- `max_session_keys_per_sni` bounds sessions inside one SNI bucket.
- Eviction is LRU by SNI bucket and MRU within each bucket.

Suggested defaults:

```text
max_session_sni_names = max(1, max_session_keys)
max_session_keys_per_sni = 1
```

This gives safe behavior close to the existing default (`max_session_keys: 1`)
without allowing unbounded growth when upstream hostnames are dynamic.

## Current patch shape

The current patch is intentionally narrower than the proposed upstream API:

- It hardcodes the number of cached SNI buckets to `10`.
- It keeps one cached client TLS session per SNI bucket.
- `max_session_keys: 0` still disables client TLS session caching.
- It does not add `.proto` fields.

The hardcoded constant is:

```cpp
static constexpr size_t MaxSniSessionCacheEntries = 10;
```

This is enough for the immediate validation and deployment shape, but it should
not be treated as the final upstream API. The upstreamable version should expose
the cache bounds in `UpstreamTlsContext`, as described above.

## Effective SNI

Centralize SNI computation so SNI selection and session-cache keying cannot
drift:

```cpp
std::string ClientContextImpl::effectiveSni(
    const Network::TransportSocketOptionsConstSharedPtr& options,
    Upstream::HostDescriptionConstSharedPtr host) const {
  // Per-request or per-attempt override wins.
  if (options && options->serverNameOverride().has_value()) {
    return options->serverNameOverride().value();
  }

  // auto_host_sni uses the selected upstream host's hostname.
  if (auto_host_sni_ && host != nullptr && !host->hostname().empty()) {
    return host->hostname();
  }

  // Static configured SNI.
  return server_name_indication_;
}
```

Use this value for both:

- `SSL_set_tlsext_host_name`.
- TLS session-cache lookup/store.

## Data structure

Replace the single dynamic-SNI session deque with SNI buckets:

```cpp
struct SniSessionBucket {
  // Most recently issued/used session at the front.
  std::deque<bssl::UniquePtr<SSL_SESSION>> sessions;

  // Iterator into sni_lru_. Keeping this avoids a linear search when touching
  // or evicting a bucket.
  std::list<std::string>::iterator lru_it;
};

// Existing mutex can continue to protect session cache state.
absl::Mutex session_keys_mu_;

// Front is most recently used SNI bucket. Back is eviction candidate.
std::list<std::string> sni_lru_ ABSL_GUARDED_BY(session_keys_mu_);

// SNI -> cached sessions for that SNI.
absl::flat_hash_map<std::string, SniSessionBucket> session_keys_by_sni_
    ABSL_GUARDED_BY(session_keys_mu_);
```

For static-SNI contexts, Envoy can keep the existing `session_keys_` path to
avoid changing behavior. For dynamic-SNI contexts, use `session_keys_by_sni_`.

## Detect dynamic SNI

```cpp
bool ClientContextImpl::usesDynamicSni(
    const Network::TransportSocketOptionsConstSharedPtr& options) const {
  // serverNameOverride can vary per request/attempt.
  if (options && options->serverNameOverride().has_value()) {
    return true;
  }

  // auto_host_sni can vary with selected upstream host.
  return auto_host_sni_;
}
```

The simple rule is conservative. It scopes sessions whenever the context could
connect to more than one SNI name.

## Lookup path

Called from `ClientContextImpl::newSsl` after setting SNI:

```cpp
void ClientContextImpl::setSessionForSni(SSL* ssl, absl::string_view sni) {
  if (max_session_keys_ == 0 || sni.empty()) {
    return;
  }

  absl::WriterMutexLock lock(session_keys_mu_);

  auto it = session_keys_by_sni_.find(sni);
  if (it == session_keys_by_sni_.end() || it->second.sessions.empty()) {
    return;
  }

  // Touch the SNI bucket: it is now the most recently used bucket.
  sni_lru_.splice(sni_lru_.begin(), sni_lru_, it->second.lru_it);
  it->second.lru_it = sni_lru_.begin();

  SSL_SESSION* session = it->second.sessions.front().get();
  SSL_set_session(ssl, session);

  // TLS 1.3 tickets may be single-use. If BoringSSL says this session should
  // be consumed once, remove it immediately after offering it.
  if (SSL_SESSION_should_be_single_use(session)) {
    it->second.sessions.pop_front();
    if (it->second.sessions.empty()) {
      sni_lru_.erase(it->second.lru_it);
      session_keys_by_sni_.erase(it);
    }
  }
}
```

## Store path

BoringSSL's new-session callback receives `SSL*`, so store under the actual SNI
used by the handshake:

```cpp
int ClientContextImpl::newSessionKey(SSL* ssl, SSL_SESSION* session) {
  if (max_session_keys_ == 0) {
    SSL_SESSION_free(session);
    return 1;
  }

  const char* raw_sni = SSL_get_servername(ssl, TLSEXT_NAMETYPE_host_name);
  if (raw_sni == nullptr || raw_sni[0] == '\0') {
    // Empty SNI is ambiguous for dynamic-SNI caching. Do not cache it in the
    // SNI-scoped cache.
    SSL_SESSION_free(session);
    return 1;
  }

  const std::string sni(raw_sni);

  absl::WriterMutexLock lock(session_keys_mu_);

  auto [it, inserted] = session_keys_by_sni_.try_emplace(sni);
  if (inserted) {
    sni_lru_.push_front(sni);
    it->second.lru_it = sni_lru_.begin();
  } else {
    sni_lru_.splice(sni_lru_.begin(), sni_lru_, it->second.lru_it);
    it->second.lru_it = sni_lru_.begin();
  }

  auto& bucket = it->second.sessions;
  bucket.push_front(bssl::UniquePtr<SSL_SESSION>(session));

  // Bound sessions within this SNI bucket.
  while (bucket.size() > max_session_keys_per_sni_) {
    bucket.pop_back();
  }

  // Bound number of SNI buckets. Evict the least recently used bucket.
  while (session_keys_by_sni_.size() > max_session_sni_names_) {
    const std::string evict = sni_lru_.back();
    sni_lru_.pop_back();
    session_keys_by_sni_.erase(evict);
  }

  return 1; // Tell BoringSSL Envoy took ownership of the session.
}
```

Callback wiring changes from:

```cpp
SSL_CTX_sess_set_new_cb(ctx, [](SSL* ssl, SSL_SESSION* session) -> int {
  auto* context = ...;
  return context->newSessionKey(session);
});
```

to:

```cpp
SSL_CTX_sess_set_new_cb(ctx, [](SSL* ssl, SSL_SESSION* session) -> int {
  auto* context = ...;
  return context->newSessionKey(ssl, session);
});
```

## `newSsl` integration

```cpp
absl::StatusOr<bssl::UniquePtr<SSL>>
ClientContextImpl::newSsl(const Network::TransportSocketOptionsConstSharedPtr& options,
                          Upstream::HostDescriptionConstSharedPtr host) {
  auto ssl_con_or_status = ContextImpl::newSsl(options, host);
  if (!ssl_con_or_status.ok()) {
    return ssl_con_or_status;
  }

  bssl::UniquePtr<SSL> ssl_con = std::move(ssl_con_or_status.value());

  const std::string sni = effectiveSni(options, host);
  if (!sni.empty()) {
    const int rc = SSL_set_tlsext_host_name(ssl_con.get(), sni.c_str());
    if (rc != 1) {
      return absl::InvalidArgumentError(
          absl::StrCat("Failed to create upstream TLS due to failure setting SNI: ",
                       Utility::getLastCryptoError().value_or("unknown")));
    }
  }

  if (usesDynamicSni(options)) {
    setSessionForSni(ssl_con.get(), sni);
  } else {
    setSessionFromLegacyCache(ssl_con.get());
  }

  return ssl_con;
}
```

`setSessionFromLegacyCache` is the current `session_keys_` logic factored out.

## Why not only disable sessions for dynamic SNI?

Disabling session reuse for dynamic SNI is correct and simple, but it gives up
performance for high-volume dynamic-SNI clusters. SNI-scoped caching preserves
the safety property while still allowing resumption for repeated connections to
the same server name.

## Test coverage

The current patch adds focused unit coverage for both themes.

TLS session-cache tests:

```text
//test/common/tls:ssl_socket_test
```

Run with:

```sh
envoy-mini-builder test run \
  --sha 0d6e3c60aa55e434f28e581df1d25fcb83404b68 \
  --patch file:///private/tmp/auto-host-sni-bounded-sni-session-cache.patch \
  --target //test/common/tls:ssl_socket_test \
  --filter '*ClientSessionCache*' \
  --platform darwin-arm64 \
  --no-clean
```

The `*ClientSessionCache*` filter is intentional. These are parameterized tests,
so `SslSocketTest.ClientSessionCache*` matches zero tests.

Covered cases:

- `ClientSessionCacheIsScopedBySni`
  - stores sessions under separate SNI buckets
  - verifies one SNI cannot overwrite another SNI bucket
- `ClientSessionCacheEvictsLeastRecentlyUsedSniAfterHardcodedBound`
  - fills the 10-entry cache
  - touches `sni-1.example.com` to make it hot
  - adds `sni-11.example.com`
  - verifies `sni-2.example.com` is evicted while `sni-1.example.com` remains

Validated result:

```text
4 tests passed
ClientSessionCacheIsScopedBySni / IPv4
ClientSessionCacheIsScopedBySni / IPv6
ClientSessionCacheEvictsLeastRecentlyUsedSniAfterHardcodedBound / IPv4
ClientSessionCacheEvictsLeastRecentlyUsedSniAfterHardcodedBound / IPv6
```

Dynamic-module hostname ABI tests:

```text
//test/extensions/clusters/dynamic_modules:cluster_test
```

Run with:

```sh
envoy-mini-builder test run \
  --sha 0d6e3c60aa55e434f28e581df1d25fcb83404b68 \
  --patch file:///private/tmp/auto-host-sni-bounded-sni-session-cache.patch \
  --target //test/extensions/clusters/dynamic_modules:cluster_test \
  --filter 'DynamicModuleClusterTest.*Hostname*' \
  --platform darwin-arm64 \
  --no-clean
```

Validated result:

```text
3 tests passed
DynamicModuleClusterTest.AbiCallbacksAddHostsWithHostnames
DynamicModuleClusterTest.AbiCallbacksAddHostsWithHostnamesNullArrayIsLegacy
DynamicModuleClusterTest.AbiCallbacksLegacyAddHostsPreservesSynthesizedHostname
```

## Patch theme

Use this name:

```text
upstream-tls-bounded-sni-session-cache
```

This reflects the desired final behavior:

- upstream TLS
- SNI-scoped
- bounded
- still a cache, not a blanket disable
