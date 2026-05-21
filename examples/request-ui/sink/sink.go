// Package sink provides a request log store with an SSE broadcaster and a
// JSON HTTP API for the request-ui example.
//
// Two storage backends are supported, selected by the REQUI_MODE env var:
//
//	REQUI_MODE=postgres (default)
//	  Persists records to Postgres. Requires REQUI_DSN. History survives
//	  restarts and reconnects. Supports iLIKE full-text search.
//
//	REQUI_MODE=memory
//	  In-process ring buffer. Zero external dependencies -- no Docker, no
//	  Postgres. History is lost on process exit. Ring capacity defaults to
//	  2000 records; set REQUI_MEM_CAP to override.
//
// Architecture (both modes):
//
//	Filter (Envoy worker thread)
//	  -> records channel (buffered, 4096)
//	  -> writer goroutine (flush every 100ms or 100 records)
//	     -> store.insert(batch)    -- pg: COPY; mem: ring append
//	     -> broadcaster (fan-out to connected SSE clients)
//	  -> HTTP server (REQUI_ADDR, default 0.0.0.0:6062)
//	       GET /api/requests              last 500 records, newest first
//	       GET /api/requests?since=ID     records with id > ID
//	       GET /api/requests?q=TEXT       substring search
//	       GET /api/requests?errors=1     only error records
//	       GET /api/stream                SSE: new records in real time
//	       GET /                          embedded UI
package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record is the per-request data written by the filter and stored by the sink.
type Record struct {
	ID        int64  `json:"id"`
	Timestamp string `json:"timestamp"`
	HasError  bool   `json:"has_error"`

	RequestID      string `json:"request_id"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	Host           string `json:"host"`
	TraceID        string `json:"trace_id"`
	SpanID         string `json:"span_id"`
	RequestHeaders string `json:"request_headers"`
	RequestBody    string `json:"request_body"`

	UpstreamStatus  string `json:"upstream_status"`
	UpstreamAddress string `json:"upstream_address"`
	ResponseHeaders string `json:"response_headers"`
	ResponseBody    string `json:"response_body"`

	ErrorDetails        string `json:"error_details"`
	ResponseFlags       string `json:"response_flags"`
	ResponseCodeDetails string `json:"response_code_details"`
	UpstreamFailure     string `json:"upstream_failure"`

	DurationMs        float64 `json:"duration_ms"`
	RequestSizeBytes  float64 `json:"request_size_bytes"`
	ResponseSizeBytes float64 `json:"response_size_bytes"`
	ResponseCode      float64 `json:"response_code"`

	// Finalized fields from access logger: timing
	FirstUpstreamTxByteSentNs    int64   `json:"first_upstream_tx_byte_sent_ns"`
	LastUpstreamRxByteReceivedNs int64   `json:"last_upstream_rx_byte_received_ns"`
	UpstreamCxPoolReadyMs        float64 `json:"upstream_cx_pool_ready_ms"`

	// Finalized fields from access logger: upstream
	UpstreamLocalAddress    string `json:"upstream_local_address"`
	UpstreamRequestAttempts uint32 `json:"upstream_request_attempts"`
	RequestProtocol         string `json:"request_protocol"`

	// Finalized fields from access logger: wire bytes
	WireBytesReceived uint64 `json:"wire_bytes_received"`
	WireBytesSent     uint64 `json:"wire_bytes_sent"`

	// Finalized fields from access logger: tracing
	TraceIDFinal string `json:"trace_id_final"`
	SpanIDFinal  string `json:"span_id_final"`
	TraceSampled bool   `json:"trace_sampled"`

	// Finalized fields from access logger: local reply
	LocalReplyBody string `json:"local_reply_body"`
}

// store is the internal storage interface. Both backends implement it.
type store interface {
	insert(batch []*Record) ([]*Record, error)
	query(q queryParams) ([]*Record, error)
}

type queryParams struct {
	search     string
	since      int64
	errorsOnly bool
	limit      int
	ascending  bool
}

// Sink is the central store and broadcaster.
type Sink struct {
	ch    chan *Record
	st    store
	bcast *broadcaster
	once  sync.Once
}

// New creates a Sink. Call Start() to connect/initialise and begin serving.
func New() *Sink {
	return &Sink{
		ch:    make(chan *Record, 4096),
		bcast: newBroadcaster(),
	}
}

// Send enqueues a record. Non-blocking: drops silently when the channel is
// full so it never stalls an Envoy worker thread.
func (s *Sink) Send(r *Record) {
	select {
	case s.ch <- r:
	default:
	}
}

// Start initialises the chosen storage backend, starts the writer goroutine
// and the HTTP server.
//
// Environment variables:
//
//	REQUI_MODE    "postgres" (default) or "memory"
//	REQUI_DSN     Postgres DSN (postgres mode only)
//	REQUI_ADDR    HTTP listen address (default 0.0.0.0:6062)
//	REQUI_MEM_CAP ring capacity for memory mode (default 2000)
func (s *Sink) Start() {
	s.once.Do(func() {
		mode := os.Getenv("REQUI_MODE")
		if mode == "" {
			mode = "postgres"
		}
		addr := os.Getenv("REQUI_ADDR")
		if addr == "" {
			addr = "0.0.0.0:6062"
		}

		switch mode {
		case "memory":
			cap := 2000
			if v, _ := strconv.Atoi(os.Getenv("REQUI_MEM_CAP")); v > 0 {
				cap = v
			}
			s.st = newMemStore(cap)
			fmt.Fprintf(os.Stderr, "[request-ui] mode=memory cap=%d\n", cap)

		default: // postgres
			dsn := os.Getenv("REQUI_DSN")
			if dsn == "" {
				dsn = "postgres://requi:requi@localhost:5432/requi?sslmode=disable"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			pool, err := pgxpool.New(ctx, dsn)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[request-ui] pgx connect: %v\n", err)
				return
			}
			if err := migrate(ctx, pool); err != nil {
				fmt.Fprintf(os.Stderr, "[request-ui] migrate: %v\n", err)
				return
			}
			s.st = &pgStore{pool: pool}
			fmt.Fprintf(os.Stderr, "[request-ui] mode=postgres\n")
		}

		go s.writer()

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[request-ui] listen %s: %v\n", addr, err)
			return
		}

		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/requests", s.handleRequests)
		mux.HandleFunc("GET /api/stream", s.handleStream)
		mux.HandleFunc("/", handleUI)

		srv := &http.Server{Handler: mux}
		go srv.Serve(ln) //nolint:errcheck
		fmt.Fprintf(os.Stderr, "[request-ui] UI on http://%s/\n", ln.Addr())
	})
}

func (s *Sink) writer() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	batch := make([]*Record, 0, 100)

	flush := func() {
		if len(batch) == 0 || s.st == nil {
			return
		}
		inserted, err := s.st.insert(batch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[request-ui] insert: %v\n", err)
		}
		for _, r := range inserted {
			s.bcast.send(r)
		}
		batch = batch[:0]
	}

	for {
		select {
		case r := <-s.ch:
			batch = append(batch, r)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *Sink) handleRequests(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]*Record{}) //nolint:errcheck
		return
	}

	q := r.URL.Query()
	limit := 500
	if l := q.Get("limit"); l != "" {
		if n, _ := strconv.Atoi(l); n > 0 && n <= 5000 {
			limit = n
		}
	}

	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	p := queryParams{
		search:     q.Get("q"),
		since:      since,
		errorsOnly: q.Get("errors") == "1",
		limit:      limit,
		ascending:  since > 0,
	}

	records, err := s.st.query(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(records) //nolint:errcheck
}

func (s *Sink) handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Flush headers immediately so clients don't wait for the first event.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.bcast.subscribe()
	defer s.bcast.unsubscribe(ch)

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case rec, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(rec)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-keepalive.C:
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ── Postgres store ───────────────────────────────────────────────────────────

type pgStore struct{ pool *pgxpool.Pool }

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id                    BIGSERIAL PRIMARY KEY,
	ts                    TIMESTAMPTZ NOT NULL DEFAULT now(),
	request_id            TEXT,
	method                TEXT,
	path                  TEXT,
	host                  TEXT,
	trace_id              TEXT,
	span_id               TEXT,
	request_headers       TEXT,
	request_body          TEXT,
	upstream_status       TEXT,
	upstream_address      TEXT,
	response_headers      TEXT,
	response_body         TEXT,
	error_details         TEXT,
	response_flags        TEXT,
	response_code_details TEXT,
	upstream_failure      TEXT,
	duration_ms           DOUBLE PRECISION,
	request_size_bytes    DOUBLE PRECISION,
	response_size_bytes   DOUBLE PRECISION,
	response_code         DOUBLE PRECISION,
	has_error             BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS requests_ts    ON requests (ts DESC);
CREATE INDEX IF NOT EXISTS requests_error ON requests (has_error) WHERE has_error;
`

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schema)
	return err
}

func (pg *pgStore) insert(batch []*Record) ([]*Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows := make([][]any, len(batch))
	for i, r := range batch {
		rows[i] = []any{
			r.RequestID, r.Method, r.Path, r.Host, r.TraceID, r.SpanID,
			r.RequestHeaders, r.RequestBody,
			r.UpstreamStatus, r.UpstreamAddress,
			r.ResponseHeaders, r.ResponseBody,
			r.ErrorDetails, r.ResponseFlags, r.ResponseCodeDetails, r.UpstreamFailure,
			r.DurationMs, r.RequestSizeBytes, r.ResponseSizeBytes, r.ResponseCode,
			r.HasError,
		}
	}

	n, err := pg.pool.CopyFrom(ctx,
		pgx.Identifier{"requests"},
		[]string{
			"request_id", "method", "path", "host", "trace_id", "span_id",
			"request_headers", "request_body",
			"upstream_status", "upstream_address",
			"response_headers", "response_body",
			"error_details", "response_flags", "response_code_details", "upstream_failure",
			"duration_ms", "request_size_bytes", "response_size_bytes", "response_code",
			"has_error",
		},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}

	qrows, err := pg.pool.Query(ctx,
		`SELECT `+selectCols+` FROM requests ORDER BY id DESC LIMIT $1`, n)
	if err != nil {
		return nil, nil // insert succeeded; broadcast failure is non-fatal
	}
	defer qrows.Close()

	out := make([]*Record, 0, int(n))
	for qrows.Next() {
		r, err := scanRecord(qrows)
		if err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (pg *pgStore) query(p queryParams) ([]*Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var args []any
	var where string

	switch {
	case p.search != "":
		pat := "%" + p.search + "%"
		where = `WHERE (method ILIKE $1 OR path ILIKE $1 OR response_flags ILIKE $1
			OR error_details ILIKE $1 OR response_code_details ILIKE $1
			OR upstream_failure ILIKE $1 OR request_id ILIKE $1)`
		args = []any{pat, p.limit}
	case p.since > 0:
		where = `WHERE id > $1`
		args = []any{p.since, p.limit}
	case p.errorsOnly:
		where = `WHERE has_error = true`
		args = []any{p.limit}
	default:
		args = []any{p.limit}
	}

	order := "DESC"
	if p.ascending {
		order = "ASC"
	}
	limitIdx := len(args)
	sql := fmt.Sprintf(`SELECT %s FROM requests %s ORDER BY id %s LIMIT $%d`,
		selectCols, where, order, limitIdx)

	rows, err := pg.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Record, 0, 64)
	for rows.Next() {
		r, err := scanRecord(rows)
		if err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

const selectCols = `id, to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	request_id, method, path, host, trace_id, span_id,
	request_headers, request_body, upstream_status, upstream_address,
	response_headers, response_body, error_details, response_flags,
	response_code_details, upstream_failure,
	duration_ms, request_size_bytes, response_size_bytes, response_code, has_error`

func scanRecord(rows interface{ Scan(dest ...any) error }) (*Record, error) {
	r := &Record{}
	return r, rows.Scan(
		&r.ID, &r.Timestamp,
		&r.RequestID, &r.Method, &r.Path, &r.Host, &r.TraceID, &r.SpanID,
		&r.RequestHeaders, &r.RequestBody,
		&r.UpstreamStatus, &r.UpstreamAddress,
		&r.ResponseHeaders, &r.ResponseBody,
		&r.ErrorDetails, &r.ResponseFlags, &r.ResponseCodeDetails, &r.UpstreamFailure,
		&r.DurationMs, &r.RequestSizeBytes, &r.ResponseSizeBytes, &r.ResponseCode,
		&r.HasError,
	)
}

// ── In-memory store ──────────────────────────────────────────────────────────

type memStore struct {
	mu   sync.RWMutex
	ring []*Record
	cap  int
	head int
	size int
	seq  int64
}

func newMemStore(capacity int) *memStore {
	if capacity <= 0 {
		capacity = 2000
	}
	return &memStore{
		ring: make([]*Record, capacity),
		cap:  capacity,
	}
}

func (m *memStore) insert(batch []*Record) ([]*Record, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	out := make([]*Record, len(batch))

	m.mu.Lock()
	for i, r := range batch {
		m.seq++
		r.ID = m.seq
		if r.Timestamp == "" {
			r.Timestamp = now
		}
		m.ring[m.head] = r
		m.head = (m.head + 1) % m.cap
		if m.size < m.cap {
			m.size++
		}
		out[i] = r
	}
	m.mu.Unlock()
	return out, nil
}

func (m *memStore) query(p queryParams) ([]*Record, error) {
	m.mu.RLock()
	snap := make([]*Record, 0, m.size)
	for i := 0; i < m.size; i++ {
		idx := ((m.head - 1 - i) + m.cap) % m.cap
		if m.ring[idx] != nil {
			snap = append(snap, m.ring[idx])
		}
	}
	m.mu.RUnlock()

	filtered := make([]*Record, 0, len(snap))
	for _, r := range snap {
		if p.since > 0 && r.ID <= p.since {
			continue
		}
		if p.errorsOnly && !r.HasError {
			continue
		}
		if p.search != "" && !recordMatchesSearch(r, p.search) {
			continue
		}
		filtered = append(filtered, r)
	}

	if p.ascending {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}

	if len(filtered) > p.limit {
		filtered = filtered[:p.limit]
	}
	return filtered, nil
}

func recordMatchesSearch(r *Record, q string) bool {
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(r.Method), q) ||
		strings.Contains(strings.ToLower(r.Path), q) ||
		strings.Contains(strings.ToLower(r.RequestID), q) ||
		strings.Contains(strings.ToLower(r.ResponseFlags), q) ||
		strings.Contains(strings.ToLower(r.ErrorDetails), q) ||
		strings.Contains(strings.ToLower(r.ResponseCodeDetails), q) ||
		strings.Contains(strings.ToLower(r.UpstreamFailure), q)
}

// ── broadcaster ──────────────────────────────────────────────────────────────

type broadcaster struct {
	mu   sync.Mutex
	subs map[chan *Record]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[chan *Record]struct{})}
}

func (b *broadcaster) subscribe() chan *Record {
	ch := make(chan *Record, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan *Record) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

func (b *broadcaster) send(r *Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- r:
		default:
		}
	}
}
