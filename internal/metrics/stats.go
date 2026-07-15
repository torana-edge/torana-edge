// Package metrics provides tracking for Torana Edge cost savings.
// All counters are safe for concurrent use via sync/atomic.
package metrics

import (
	"encoding/json"
	"sync/atomic"
)

// StatsTracker records proxy request statistics and compaction savings —
// the numbers behind Torana's cost-saving value proposition.
type StatsTracker struct {
	TotalRequests   int64 `json:"total_requests"`
	TotalBytesIn    int64 `json:"total_bytes_in"`
	TotalBytesOut   int64 `json:"total_bytes_out"`
	Compactions     int64 `json:"compactions"`
	BytesSaved      int64 `json:"bytes_saved"`
	OffloadFailures int64 `json:"offload_failures"`
}

// NewStatsTracker creates a zeroed StatsTracker.
func NewStatsTracker() *StatsTracker { return &StatsTracker{} }

// RecordRequest records a single proxied request.
func (s *StatsTracker) RecordRequest(bytesIn, bytesOut int64) {
	atomic.AddInt64(&s.TotalRequests, 1)
	atomic.AddInt64(&s.TotalBytesIn, bytesIn)
	atomic.AddInt64(&s.TotalBytesOut, bytesOut)
}

// RecordCompaction records one tool-result compaction and its savings.
func (s *StatsTracker) RecordCompaction(originalBytes, finalBytes int64) {
	atomic.AddInt64(&s.Compactions, 1)
	if saved := originalBytes - finalBytes; saved > 0 {
		atomic.AddInt64(&s.BytesSaved, saved)
	}
}

// RecordOffloadFailure counts a failed cheap-model offload call.
func (s *StatsTracker) RecordOffloadFailure() {
	atomic.AddInt64(&s.OffloadFailures, 1)
}

// Snapshot returns a copy of the current state.
func (s *StatsTracker) Snapshot() StatsTracker {
	return StatsTracker{
		TotalRequests:   atomic.LoadInt64(&s.TotalRequests),
		TotalBytesIn:    atomic.LoadInt64(&s.TotalBytesIn),
		TotalBytesOut:   atomic.LoadInt64(&s.TotalBytesOut),
		Compactions:     atomic.LoadInt64(&s.Compactions),
		BytesSaved:      atomic.LoadInt64(&s.BytesSaved),
		OffloadFailures: atomic.LoadInt64(&s.OffloadFailures),
	}
}

// MarshalJSON implements json.Marshaler.
func (s *StatsTracker) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Snapshot())
}
