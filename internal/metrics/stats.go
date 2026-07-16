// Package metrics provides tracking for Torana Edge cost savings.
// All counters are safe for concurrent use via sync/atomic; the per-plugin
// savings map is mutex-guarded.
package metrics

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// PluginSavings holds one plugin's cumulative compaction savings.
type PluginSavings struct {
	Compactions int64 `json:"compactions"`
	BytesSaved  int64 `json:"bytes_saved"`
}

// Stats is an immutable snapshot of a StatsTracker.
type Stats struct {
	TotalRequests   int64                    `json:"total_requests"`
	TotalBytesIn    int64                    `json:"total_bytes_in"`
	TotalBytesOut   int64                    `json:"total_bytes_out"`
	TotalTokensIn   int64                    `json:"total_tokens_in"`
	TotalTokensOut  int64                    `json:"total_tokens_out"`
	Compactions     int64                    `json:"compactions"`
	BytesSaved      int64                    `json:"bytes_saved"`
	OffloadFailures int64                    `json:"offload_failures"`
	PerPlugin       map[string]PluginSavings `json:"per_plugin,omitempty"`
}

// StatsTracker records proxy request statistics and compaction savings —
// the numbers behind Torana's cost-saving value proposition.
type StatsTracker struct {
	totalRequests   int64
	totalBytesIn    int64
	totalBytesOut   int64
	totalTokensIn   int64
	totalTokensOut  int64
	compactions     int64
	bytesSaved      int64
	offloadFailures int64

	mu        sync.Mutex
	perPlugin map[string]*PluginSavings
}

// NewStatsTracker creates a zeroed StatsTracker.
func NewStatsTracker() *StatsTracker {
	return &StatsTracker{perPlugin: map[string]*PluginSavings{}}
}

// RecordRequest records a single proxied request.
func (s *StatsTracker) RecordRequest(bytesIn, bytesOut int64) {
	atomic.AddInt64(&s.totalRequests, 1)
	atomic.AddInt64(&s.totalBytesIn, bytesIn)
	atomic.AddInt64(&s.totalBytesOut, bytesOut)
}

// RecordTokens accumulates provider-reported token usage. Zero counts
// (provider didn't report) are a no-op.
func (s *StatsTracker) RecordTokens(in, out int64) {
	if in > 0 {
		atomic.AddInt64(&s.totalTokensIn, in)
	}
	if out > 0 {
		atomic.AddInt64(&s.totalTokensOut, out)
	}
}

// RecordCompaction records one tool-result compaction and its savings,
// attributed to the plugin that reported it.
func (s *StatsTracker) RecordCompaction(plugin string, originalBytes, finalBytes int64) {
	atomic.AddInt64(&s.compactions, 1)
	saved := originalBytes - finalBytes
	if saved > 0 {
		atomic.AddInt64(&s.bytesSaved, saved)
	}
	if plugin == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.perPlugin[plugin]
	if ps == nil {
		ps = &PluginSavings{}
		s.perPlugin[plugin] = ps
	}
	ps.Compactions++
	if saved > 0 {
		ps.BytesSaved += saved
	}
}

// RecordOffloadFailure counts a failed cheap-model offload call.
func (s *StatsTracker) RecordOffloadFailure() {
	atomic.AddInt64(&s.offloadFailures, 1)
}

// Snapshot returns a copy of the current state.
func (s *StatsTracker) Snapshot() Stats {
	snap := Stats{
		TotalRequests:   atomic.LoadInt64(&s.totalRequests),
		TotalBytesIn:    atomic.LoadInt64(&s.totalBytesIn),
		TotalBytesOut:   atomic.LoadInt64(&s.totalBytesOut),
		TotalTokensIn:   atomic.LoadInt64(&s.totalTokensIn),
		TotalTokensOut:  atomic.LoadInt64(&s.totalTokensOut),
		Compactions:     atomic.LoadInt64(&s.compactions),
		BytesSaved:      atomic.LoadInt64(&s.bytesSaved),
		OffloadFailures: atomic.LoadInt64(&s.offloadFailures),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.perPlugin) > 0 {
		snap.PerPlugin = make(map[string]PluginSavings, len(s.perPlugin))
		for name, ps := range s.perPlugin {
			snap.PerPlugin[name] = *ps
		}
	}
	return snap
}

// MarshalJSON implements json.Marshaler.
func (s *StatsTracker) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Snapshot())
}
