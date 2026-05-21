// Package accessloggersink is an in-process HTTP server that receives JSON
// access log entries from e2e filters and makes them available to tests.
package accessloggersink

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Entry is one access log record as posted by the e2e-logger filter.
type Entry struct {
	LogType      int    `json:"log_type"`
	DurationMs   int64  `json:"duration_ms"`
	BytesSent    uint64 `json:"bytes_sent"`
	ResponseCode uint32 `json:"response_code"`
	CodeDetails  string `json:"code_details"`
	Flags        string `json:"flags"`
}

// CorrelatedEntry is posted by the e2e-correlator-logger filter and contains
// fields from both the HTTP filter phase and the finalized access log phase.
type CorrelatedEntry struct {
	RequestID    string `json:"request_id"`
	StatusFilter int    `json:"status_filter"` // response status seen by the HTTP filter
	ResponseCode uint32 `json:"response_code"` // finalized code from the access logger
	DurationMs   int64  `json:"duration_ms"`
	BytesSent    uint64 `json:"bytes_sent"`
	Flags        string `json:"flags"`
}

var (
	mu      sync.Mutex
	entries []Entry

	corrMu      sync.Mutex
	corrEntries []CorrelatedEntry
)

// StartSink starts the HTTP sink server on a random port and returns its URL.
func StartSink() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("accessloggersink: listen: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e Entry
		if json.Unmarshal(body, &e) == nil {
			mu.Lock()
			entries = append(entries, e)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/correlate", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e CorrelatedEntry
		if json.Unmarshal(body, &e) == nil {
			corrMu.Lock()
			corrEntries = append(corrEntries, e)
			corrMu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return "http://" + l.Addr().String()
}

// Drain waits up to timeout for at least one entry and returns all collected so far.
func Drain(timeout time.Duration) []Entry {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(entries)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, len(entries))
	copy(out, entries)
	return out
}

// Reset clears all collected entries.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	entries = entries[:0]
}

// DrainCorrelated waits up to timeout for at least one correlated entry and returns all collected.
func DrainCorrelated(timeout time.Duration) []CorrelatedEntry {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		corrMu.Lock()
		n := len(corrEntries)
		corrMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	corrMu.Lock()
	defer corrMu.Unlock()
	out := make([]CorrelatedEntry, len(corrEntries))
	copy(out, corrEntries)
	return out
}

// ResetCorrelated clears all collected correlated entries.
func ResetCorrelated() {
	corrMu.Lock()
	defer corrMu.Unlock()
	corrEntries = corrEntries[:0]
}
