package reqlog

import (
	"encoding/json"
	"os"
	"sync"
)

// Exporter receives finalized request records. Implementations must not block
// the caller — Export is called on an Envoy worker thread.
type Exporter interface {
	Export(r *Record)
	Close()
}

// FilteredExporter applies an additional per-exporter FieldFilter on top of
// the global one, then delegates to inner. Use it when individual exporters
// need a different field projection than the global config provides (e.g.
// full bodies to a local file, stripped bodies to an HTTP backend).
type FilteredExporter struct {
	inner  Exporter
	filter *FieldFilter
}

// NewFilteredExporter wraps inner with an extra FieldFilter applied before
// each Export call. The filter is applied to a shallow copy of the record so
// the original is not mutated for other exporters in the fan-out.
func NewFilteredExporter(inner Exporter, f *FieldFilter) *FilteredExporter {
	return &FilteredExporter{inner: inner, filter: f}
}

func (fe *FilteredExporter) Export(r *Record) {
	cp := *r
	cp.RequestHeaders = append([][2]string(nil), r.RequestHeaders...)
	cp.ResponseHeaders = append([][2]string(nil), r.ResponseHeaders...)
	fe.filter.Apply(&cp)
	fe.inner.Export(&cp)
}

func (fe *FilteredExporter) Close() { fe.inner.Close() }

// multiExporter fans each Record out to all registered Exporters.
type multiExporter struct {
	mu        sync.RWMutex
	exporters []Exporter
}

func (m *multiExporter) add(e Exporter) {
	m.mu.Lock()
	m.exporters = append(m.exporters, e)
	m.mu.Unlock()
}

func (m *multiExporter) Export(r *Record) {
	m.mu.RLock()
	exporters := m.exporters
	m.mu.RUnlock()
	for _, e := range exporters {
		e.Export(r)
	}
}

func (m *multiExporter) Close() {
	m.mu.RLock()
	exporters := m.exporters
	m.mu.RUnlock()
	for _, e := range exporters {
		e.Close()
	}
}

var global = &multiExporter{}

// AddExporter registers an Exporter to receive all finalized records. Safe to
// call before Envoy starts serving. Typically called from an init() in the
// binary that wires up the concrete backend.
func AddExporter(e Exporter) {
	global.add(e)
}

// StdoutExporter writes one JSON line per record to os.Stdout. It is a
// reference implementation intended for development and debugging. Enable it
// by calling AddExporter(NewStdoutExporter()) from your init().
type StdoutExporter struct {
	enc *json.Encoder
}

// NewStdoutExporter creates a StdoutExporter writing JSON lines to stdout.
func NewStdoutExporter() *StdoutExporter {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return &StdoutExporter{enc: enc}
}

func (s *StdoutExporter) Export(r *Record) {
	_ = s.enc.Encode(r)
}

func (s *StdoutExporter) Close() {}
