# lb-policy-retry-aware

This example demonstrates an LB Policy that is aware of Envoy's retry mechanism. On retry attempts, the policy skips hosts that Envoy has already tried and selects a different host, improving the chances of success.

## What It Shows

- LB Policy extension point
- Detecting retry attempts in request context
- Skipping previously-tried hosts via `ShouldSelectAnotherHost()`
- Fallback behavior when all hosts have been tried

## Scenario

Your upstream service is flaky. Envoy retries requests when they fail. Without retry awareness, the retry might go to the same host that just failed. This policy detects retries and routes them to different hosts.

```
Request to host[0] → timeout/error
  └─ Envoy initiates retry
  └─ Policy sees ShouldSelectAnotherHost(host[0]) = true
  └─ Skips host[0]
  └─ Selects host[1]
  └─ Retry succeeds
```

## Build and Test

```bash
# Build
make -C examples/lb-policy-retry-aware build

# Unit tests
make -C examples/lb-policy-retry-aware test

# End-to-end tests (requires Envoy binary)
make -C examples/lb-policy-retry-aware e2e
```

## How It Works

### Selection Algorithm

```go
func (p *retryAwarePolicy) ChooseHost(lb up.LBHandle, ctx up.LBContext, priority *uint32, index *uint32) bool {
    n := lb.HealthyHostCount(0)
    if n == 0 {
        return false
    }
    *priority = 0
    for i := 0; i < n; i++ {
        *index = uint32(i)
        if !ctx.ShouldSelectAnotherHost(lb, 0, i) {
            return true
        }
    }
    // all tried — fall back to 0
    *index = 0
    return true
}
```

1. Get healthy host count
2. Iterate through all healthy hosts
3. For each host, check `ShouldSelectAnotherHost()`:
   - Returns `true` if Envoy already tried this host
   - Returns `false` if it's safe to try (first attempt or not yet tried)
4. Select the first host where `ShouldSelectAnotherHost()` returns `false`
5. If all hosts have been tried, fall back to index 0 (avoid hard 503)

### Context API

The `up.LBContext` provides:

```go
// ShouldSelectAnotherHost returns true if Envoy has already attempted
// a request to this host and is considering a retry to a different host.
ShouldSelectAnotherHost(lb up.LBHandle, priority uint32, index uint32) bool
```

This allows the policy to query whether a specific host has been tried on the current request.

### Flow

1. **Initial attempt**: Policy selects host[0] (ShouldSelectAnotherHost returns false for all)
2. **Host fails**: Envoy detects error/timeout
3. **Retry**: Envoy calls policy again
4. **Policy sees retry**: ShouldSelectAnotherHost(host[0]) returns true
5. **Policy skips host[0]**: Iterates to host[1] where ShouldSelectAnotherHost returns false
6. **Select host[1]**: Retry routed to different host

## Key Files

- `lb_policy_retry_aware.go` — Policy implementation with retry detection
- `lb_policy_retry_aware_test.go` — Unit tests
- `cmd/main.go` — Shared library entry point
- `e2e/e2e_test.go` — End-to-end tests with real Envoy
- `Makefile` — Build and test targets

## Fallback Behavior

When all healthy hosts have been tried:
- Policy returns index 0 instead of failing
- Rationale: Better to retry the same host than fail with 503
- Envoy's retry logic will respect max retries and eventually fail if needed

## Envoy Retry Config

To use this policy effectively, enable Envoy retries:

```yaml
route:
  cluster: backend
  retry_policy:
    retry_on: "5xx,reset,connect-failure,retriable-4xx"
    num_retries: 3
    backoff:
      base_interval: 25ms
      max_interval: 250ms
```

## Test Coverage

**Unit tests** cover:
- Config parsing (empty config is valid)
- Policy creation

**E2E tests** verify:
- Initial request goes to first healthy host
- Retry attempts go to different hosts
- All-tried fallback returns index 0
- Multiple retries exercise multiple hosts
- Unhealthy hosts are skipped throughout
