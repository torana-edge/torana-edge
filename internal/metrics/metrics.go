// Package metrics provides counters for Torana Edge observability.
// All counters are safe for concurrent use via sync/atomic.
package metrics

import (
	"encoding/json"
	"sync/atomic"
)

// Metrics holds Torana's operational counters.
type Metrics struct {
	Requests        atomic.Int64 `json:"requests"`
	IntentsExtracted atomic.Int64 `json:"intents_extracted"`
	ResultsCompacted atomic.Int64 `json:"results_compacted"`
	BytesSaved       atomic.Int64 `json:"bytes_saved"`
	Errors           atomic.Int64 `json:"errors"`
}

// New creates a zeroed Metrics instance.
func New() *Metrics { return &Metrics{} }

// RecordRequest increments the request counter.
func (m *Metrics) RecordRequest() { m.Requests.Add(1) }

// RecordIntent increments the intent extraction counter.
func (m *Metrics) RecordIntent() { m.IntentsExtracted.Add(1) }

// RecordCompaction adds saved bytes to the compaction counter.
func (m *Metrics) RecordCompaction(bytesSaved int64) {
	m.ResultsCompacted.Add(1)
	m.BytesSaved.Add(bytesSaved)
}

// RecordError increments the error counter.
func (m *Metrics) RecordError() { m.Errors.Add(1) }

// Snapshot returns a copy of all counters as a plain struct (safe for JSON).
func (m *Metrics) Snapshot() *Metrics {
	snap := &Metrics{}
	snap.Requests.Store(m.Requests.Load())
	snap.IntentsExtracted.Store(m.IntentsExtracted.Load())
	snap.ResultsCompacted.Store(m.ResultsCompacted.Load())
	snap.BytesSaved.Store(m.BytesSaved.Load())
	snap.Errors.Store(m.Errors.Load())
	return snap
}

// MarshalJSON implements json.Marshaler for clean output.
func (m *Metrics) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int64{
		"requests":         m.Requests.Load(),
		"intents_extracted": m.IntentsExtracted.Load(),
		"results_compacted": m.ResultsCompacted.Load(),
		"bytes_saved":      m.BytesSaved.Load(),
		"errors":           m.Errors.Load(),
	})
}
