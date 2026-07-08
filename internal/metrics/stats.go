// Package metrics provides tracking for Torana Edge cost savings.
// All counters are safe for concurrent use via sync/atomic.
package metrics

import (
	"encoding/json"
	"sync/atomic"
)

// StatsTracker records bytes and tokens saved by the offload compactor.
type StatsTracker struct {
	TotalRequests   int64 `json:"total_requests"`
	TotalBytesIn    int64 `json:"total_bytes_in"`
	TotalBytesOut   int64 `json:"total_bytes_out"`
	TotalBytesSaved int64 `json:"total_bytes_saved"`
	Compactions     int64 `json:"compactions"`
	OffloadFailures int64 `json:"offload_failures"`
}

func (s *StatsTracker) RecordCompaction(bytesIn, bytesOut int64) {
	atomic.AddInt64(&s.TotalRequests, 1)
	atomic.AddInt64(&s.TotalBytesIn, bytesIn)
	atomic.AddInt64(&s.TotalBytesOut, bytesOut)
	atomic.AddInt64(&s.TotalBytesSaved, bytesIn-bytesOut)
	atomic.AddInt64(&s.Compactions, 1)
}

func (s *StatsTracker) RecordOffloadFailure() {
	atomic.AddInt64(&s.OffloadFailures, 1)
}

func (s *StatsTracker) Snapshot() StatsTracker {
	return StatsTracker{
		TotalRequests:   atomic.LoadInt64(&s.TotalRequests),
		TotalBytesIn:    atomic.LoadInt64(&s.TotalBytesIn),
		TotalBytesOut:   atomic.LoadInt64(&s.TotalBytesOut),
		TotalBytesSaved: atomic.LoadInt64(&s.TotalBytesSaved),
		Compactions:     atomic.LoadInt64(&s.Compactions),
		OffloadFailures: atomic.LoadInt64(&s.OffloadFailures),
	}
}

func (s *StatsTracker) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Snapshot())
}
