# e2e/sinks/accessloggersink

An in-process HTTP sink that receives JSON access log entries posted by e2e
filters and makes them available to tests.

```go
sinkURL := accessloggersink.StartSink() // "http://127.0.0.1:<random-port>"

// poll until entries arrive (or timeout), then read them
entries := accessloggersink.Drain(5 * time.Second)

// correlated entries posted by e2e-correlator-logger
corr := accessloggersink.DrainCorrelated(5 * time.Second)

// reset between test cases
accessloggersink.Reset()
accessloggersink.ResetCorrelated()
```

## Endpoints

| Path | Posted by | Entry type |
|---|---|---|
| `POST /log` | `e2e-logger` | `Entry` — timing, byte counts, response code, flags |
| `POST /correlate` | `e2e-correlator-logger` | `CorrelatedEntry` — joins HTTP filter phase data with access log phase data |

## Entry types

**`Entry`** — one record from the `e2e-logger` access logger:

```go
type Entry struct {
    LogType      int    // AccessLogType value
    DurationMs   int64
    BytesSent    uint64
    ResponseCode uint32
    CodeDetails  string
    Flags        string // ResponseFlagsString output
}
```

**`CorrelatedEntry`** — posted by `e2e-correlator-logger` after combining
the HTTP filter phase (response status code, request ID) with the access log
phase (duration, byte counts, flags). Used by `CorrelatorSuite` to assert
`status_filter == response_code`.
