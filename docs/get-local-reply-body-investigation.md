# `GetLocalReplyBody` returns empty: open investigation

> **Status:** unresolved. Field is plumbed through the SDK for ABI parity
> but observed empty on every path reachable from in-tree e2e. Predates
> the `WithOnStreamFinalized` work — the previous bespoke access logger
> in `examples/request-ui/accesslogger.go` saw the same empty result and
> just shipped it silently. This doc captures what was tried and what to
> investigate next when we read the Envoy source.

## Symptom

`AccessLoggerHandle.GetLocalReplyBody()` returns `ok=false` (zero-length
`String` view) for every code path the e2e suite can drive. The same
call, made from the same access logger instance, returns finalized
fields (timings, response code, response flags, byte counts) correctly —
so the handle is live and the access logger is firing at
`AccessLogTypeDownstreamEnd`. Only the local-reply body slot is empty.

## Paths tried

All three were exercised against the Envoy build pinned by
`integration/down/envoy` (the dynamic-modules fork used by transit2 e2e).
Each was wired through the existing `stream-finalized-e2e` filter so
`OnStreamFinalized` fires and we observed `FinalizedInfo.LocalReplyBody`
on the resulting record.

### 1. Filter `SendLocalResponse` with inline body

The `e2e-stream-finalized-guard` filter on `stream-finalized-local-e2e`
returns 401 via `SendLocalResponse` with a non-empty body string when
`x-api-key` is missing. The 401 reaches the client with the body
present in the response. Access logger fires. `GetLocalReplyBody()`
returns empty.

Reference: `e2e/filters/stream_finalized.go` (guard filter) and
`e2e/stream_finalized_test.go::TestLocalReply_firesWithResponseCode`.

### 2. Route `direct_response` with `response_body.inline_string`

A route variant with `direct_response` (status 200, inline body) was
added during the initial investigation. Envoy serves the body to the
client. Access logger fires. `GetLocalReplyBody()` returns empty.

The test (`TestDirectResponse_firesWithLocalReplyBody`) was removed
after the negative result; the listener config that drove it is no
longer in `envoy.tmpl.yaml`. Restoring it is mechanical if a future
Envoy build changes behaviour.

### 3. Envoy-synthesised 503 on upstream connection failure

`stream-finalized-dead-e2e` routes to a cluster whose only endpoint is
a closed TCP socket. Envoy responds with a 503 and synthesises the
"upstream connect error" body itself. Access logger fires with
`ResponseFlags` containing `UF` (UpstreamConnectionFailure).
`GetLocalReplyBody()` returns empty.

Reference: `e2e/stream_finalized_test.go::TestUpstreamFailure_firesWithResponseFlags`.

## What this *might* mean (hypotheses to verify against Envoy source)

These are guesses. The investigation is the point of this doc.

1. **The slot is populated only for a specific Envoy "local reply"
   internal path** — perhaps only when `LocalReply::Impl::rewrite` runs,
   which `SendLocalResponse` from a dynamic-module filter and a route's
   `direct_response` may both bypass. In that case the body is sent on
   the wire via a different code path (DirectResponseEntry, filter
   chain short-circuit) that never touches the `StreamInfo`
   local-reply-body slot the dynamic-module ABI reads from.

2. **The ABI binding reads a slot that's wired but never set** by the
   dynamic-modules fork. I.e. the C++ side has the getter, returns a
   pointer to an `absl::optional<std::string>` or similar that nothing
   in this build assigns to. Would explain "always empty, never an
   error."

3. **Lifetime / ordering issue.** The body buffer is set during the
   local reply but freed before `AccessLogTypeDownstreamEnd`. Possible
   but unlikely given other `StreamInfo`-derived fields (response code,
   flags) survive to the same callback.

## Concrete next steps for an Envoy-source pass

- Find the dynamic-modules ABI implementation of `getLocalReplyBody`
  (likely in `source/extensions/dynamic_modules/abi_impl.cc` or
  similar). Identify the `StreamInfo` field or `LocalReply` member it
  reads.
- Grep for assignments to that field. Cross-reference with:
  - `Http::Utility::sendLocalReply` (the path
    `Filter::sendLocalReply` ends up in).
  - `Router::DirectResponseEntry` (the `direct_response` path).
  - `Router::Filter::onUpstreamConnectionFailure` / the UF synthesised
    body path.
- If the field is assigned on some paths but not the three above, the
  fix is upstream in the dynamic-modules fork: assign on the missing
  paths.
- If the field is assigned but read at the wrong lifecycle point, fix
  the ABI getter or the access-log timing.

## SDK posture in the meantime

- `FinalizedInfo.LocalReplyBody` stays — removing it would force every
  consumer to rewrite if Envoy starts populating it. Keep the surface,
  document the gap.
- `e2e/filters/stream_finalized.go` keeps the `HasLocalReplyBody`
  counter (currently always zero) so a future Envoy build flips it to
  non-zero with no test wiring change.
- `e2e/stream_finalized_test.go::TestLocalReply_firesWithResponseCode`
  carries the long-form comment pointing here.

## Related code

- SDK plumb: `up/stream_finalized.go` (`finalizedLogger.OnLog`).
- SDK surface: `up/up.go` (`FinalizedInfo.LocalReplyBody`).
- Predecessor (now deleted) consumer that had the same silent gap:
  `examples/request-ui/accesslogger.go` at commit `f2ff1c1^`, line 155.
- e2e listeners that drove the three paths: `e2e/testdata/envoy.tmpl.yaml`
  (`stream-finalized-e2e`, `stream-finalized-dead-e2e`,
  `stream-finalized-local-e2e`).
