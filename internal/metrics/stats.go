// Package metrics provides tracking for Torana Edge cost savings.
// All counters are safe for concurrent use via sync/atomic.
package metrics

import (
	"encoding/json"
	"sync/atomic"
)

// StatsTracker records proxy request statistics.
type StatsTracker struct {
	TotalRequests int64 `json:"total_requests"`
	TotalBytesIn  int64 `json:"total_bytes_in"`
	TotalBytesOut int64 `json:"total_bytes_out"`
}

// NewStatsTracker creates a zeroed StatsTracker.
func NewStatsTracker() *StatsTracker { return &StatsTracker{} }

// RecordRequest records a single proxied request.
func (s *StatsTracker) RecordRequest(bytesIn, bytesOut int64) {
	atomic.AddInt64(&s.TotalRequests, 1)
	atomic.AddInt64(&s.TotalBytesIn, bytesIn)
	atomic.AddInt64(&s.TotalBytesOut, bytesOut)
}

// Snapshot returns a copy of the current state.
func (s *StatsTracker) Snapshot() StatsTracker {
	return StatsTracker{
		TotalRequests: atomic.LoadInt64(&s.TotalRequests),
		TotalBytesIn:  atomic.LoadInt64(&s.TotalBytesIn),
		TotalBytesOut: atomic.LoadInt64(&s.TotalBytesOut),
	}
}

// MarshalJSON implements json.Marshaler.
func (s *StatsTracker) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Snapshot())
}
