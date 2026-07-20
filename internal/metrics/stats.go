// Package metrics provides tracking for Torana Edge cost savings.
// All counters are safe for concurrent use via sync/atomic; the per-plugin
// savings map is mutex-guarded.
package metrics

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/torana-edge/torana-edge/internal/economics"
)

// PluginSavings holds one plugin's cumulative compaction savings.
type PluginSavings struct {
	Compactions                int64   `json:"compactions"`
	BytesSaved                 int64   `json:"bytes_saved"`
	Applications               int64   `json:"applications"`
	Transformations            int64   `json:"transformations"`
	CacheReuses                int64   `json:"cache_reuses"`
	EstimatedTokensAvoided     int64   `json:"estimated_tokens_avoided"`
	EstimatedRewriteSpanTokens int64   `json:"estimated_rewrite_span_tokens"`
	EstimatedGrossUSD          float64 `json:"estimated_gross_usd,omitempty"`
	EstimatedNetUSD            float64 `json:"estimated_net_usd,omitempty"`
}

// Stats is an immutable snapshot of a StatsTracker.
type Stats struct {
	TotalRequests  int64 `json:"total_requests"`
	TotalBytesIn   int64 `json:"total_bytes_in"`
	TotalBytesOut  int64 `json:"total_bytes_out"`
	TotalTokensIn  int64 `json:"total_tokens_in"`
	TotalTokensOut int64 `json:"total_tokens_out"`
	// Prompt-cache accounting: read = input tokens served from the provider's
	// cache, write = tokens written to cache. Rates are provider/model-specific.
	TotalCacheReadTokens       int64                    `json:"total_cache_read_tokens"`
	TotalCacheWriteTokens      int64                    `json:"total_cache_write_tokens"`
	Compactions                int64                    `json:"compactions"`
	BytesSaved                 int64                    `json:"bytes_saved"`
	OffloadFailures            int64                    `json:"offload_failures"`
	OffloadInputTokens         int64                    `json:"offload_input_tokens"`
	OffloadOutputTokens        int64                    `json:"offload_output_tokens"`
	OffloadCacheReadTokens     int64                    `json:"offload_cache_read_tokens"`
	OffloadCacheWriteTokens    int64                    `json:"offload_cache_write_tokens"`
	CompactionApplications     int64                    `json:"compaction_applications"`
	CompactionTransformations  int64                    `json:"compaction_transformations"`
	CompactionCacheReuses      int64                    `json:"compaction_cache_reuses"`
	EstimatedTokensAvoided     int64                    `json:"estimated_input_tokens_avoided"`
	EstimatedRewriteSpanTokens int64                    `json:"estimated_cache_rewrite_tokens"`
	EstimatedGrossUSD          *float64                 `json:"estimated_gross_usd,omitempty"`
	EstimatedNetUSD            *float64                 `json:"estimated_net_usd,omitempty"`
	SavingsUnavailable         map[string]int64         `json:"savings_unavailable,omitempty"`
	PerPlugin                  map[string]PluginSavings `json:"per_plugin,omitempty"`
}

// StatsTracker records proxy request statistics and compaction savings —
// the numbers behind Torana's cost-saving value proposition.
type StatsTracker struct {
	totalRequests              int64
	totalBytesIn               int64
	totalBytesOut              int64
	totalTokensIn              int64
	totalTokensOut             int64
	totalCacheReadTokens       int64
	totalCacheWriteTokens      int64
	compactions                int64
	bytesSaved                 int64
	offloadFailures            int64
	offloadInputTokens         int64
	offloadOutputTokens        int64
	offloadCacheReadTokens     int64
	offloadCacheWriteTokens    int64
	compactionApplications     int64
	compactionTransformations  int64
	compactionCacheReuses      int64
	estimatedTokensAvoided     int64
	estimatedRewriteSpanTokens int64

	mu                  sync.Mutex
	perPlugin           map[string]*PluginSavings
	estimatedGrossUSD   float64
	estimatedNetUSD     float64
	estimatedGrossCount int64
	estimatedNetCount   int64
	savingsUnavailable  map[string]int64
}

// NewStatsTracker creates a zeroed StatsTracker.
func NewStatsTracker() *StatsTracker {
	return &StatsTracker{
		perPlugin:          map[string]*PluginSavings{},
		savingsUnavailable: map[string]int64{},
	}
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

// RecordCacheTokens accumulates provider-reported prompt-cache usage
// (read = cache hits, write = cache creation). Zero counts are a no-op.
func (s *StatsTracker) RecordCacheTokens(read, write int64) {
	if read > 0 {
		atomic.AddInt64(&s.totalCacheReadTokens, read)
	}
	if write > 0 {
		atomic.AddInt64(&s.totalCacheWriteTokens, write)
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
	atomic.AddInt64(&s.compactionApplications, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.perPlugin[plugin]
	if ps == nil {
		ps = &PluginSavings{}
		s.perPlugin[plugin] = ps
	}
	ps.Compactions++
	ps.Applications++
	if saved > 0 {
		ps.BytesSaved += saved
	}
}

// RecordCompactionReport records the richer batch ABI. Pricing arguments are
// optional; absent or incomplete prices preserve token/byte metrics and add an
// explicit reason instead of inventing a dollar estimate.
func (s *StatsTracker) RecordCompactionReport(plugin string, report economics.CompactionReport, targetPricing, offloadPricing *economics.ModelPricing) {
	report.Normalize()
	// Preserve the legacy counters as application-oriented compatibility
	// fields. A batch is one application regardless of candidate count.
	s.RecordCompaction(plugin, report.OriginalBytes, report.FinalBytes)
	atomic.AddInt64(&s.estimatedTokensAvoided, report.EstimatedTokensRemoved)
	if report.Source == "transformation" {
		atomic.AddInt64(&s.compactionTransformations, 1)
		atomic.AddInt64(&s.estimatedRewriteSpanTokens, report.EstimatedRewriteSpanTokens)
	} else if report.Source == "cache_reuse" {
		atomic.AddInt64(&s.compactionCacheReuses, 1)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.perPlugin[plugin]
	if ps != nil {
		ps.EstimatedTokensAvoided += report.EstimatedTokensRemoved
		if report.Source == "transformation" {
			ps.Transformations++
			ps.EstimatedRewriteSpanTokens += report.EstimatedRewriteSpanTokens
		} else if report.Source == "cache_reuse" {
			ps.CacheReuses++
		}
	}
	if report.Source == "legacy" {
		return
	}
	if targetPricing == nil {
		s.savingsUnavailable[economics.UnavailablePricing]++
		return
	}
	est := economics.EstimateApplicationSavings(report, *targetPricing, offloadPricing)
	if est.EstimatedGrossUSD != nil {
		s.estimatedGrossUSD += *est.EstimatedGrossUSD
		s.estimatedGrossCount++
		if ps != nil {
			ps.EstimatedGrossUSD += *est.EstimatedGrossUSD
		}
	}
	if est.EstimatedNetUSD != nil {
		s.estimatedNetUSD += *est.EstimatedNetUSD
		s.estimatedNetCount++
		if ps != nil {
			ps.EstimatedNetUSD += *est.EstimatedNetUSD
		}
	}
	if est.UnavailableReason != "" {
		s.savingsUnavailable[est.UnavailableReason]++
	}
}

// RecordOffloadFailure counts a failed cheap-model offload call.
func (s *StatsTracker) RecordOffloadFailure() {
	atomic.AddInt64(&s.offloadFailures, 1)
}

// RecordOffloadUsage records provider-reported usage for a successful
// compaction offload. InputTokens includes cache reads for OpenAI-compatible
// providers, matching Usage.InputIncludesCacheRead.
func (s *StatsTracker) RecordOffloadUsage(usage economics.Usage) {
	if !usage.Reported {
		return
	}
	atomic.AddInt64(&s.offloadInputTokens, usage.InputTokens)
	atomic.AddInt64(&s.offloadOutputTokens, usage.OutputTokens)
	atomic.AddInt64(&s.offloadCacheReadTokens, usage.CacheReadTokens)
	atomic.AddInt64(&s.offloadCacheWriteTokens, usage.CacheWriteTokens)
}

// Snapshot returns a copy of the current state.
func (s *StatsTracker) Snapshot() Stats {
	snap := Stats{
		TotalRequests:              atomic.LoadInt64(&s.totalRequests),
		TotalBytesIn:               atomic.LoadInt64(&s.totalBytesIn),
		TotalBytesOut:              atomic.LoadInt64(&s.totalBytesOut),
		TotalTokensIn:              atomic.LoadInt64(&s.totalTokensIn),
		TotalTokensOut:             atomic.LoadInt64(&s.totalTokensOut),
		TotalCacheReadTokens:       atomic.LoadInt64(&s.totalCacheReadTokens),
		TotalCacheWriteTokens:      atomic.LoadInt64(&s.totalCacheWriteTokens),
		Compactions:                atomic.LoadInt64(&s.compactions),
		BytesSaved:                 atomic.LoadInt64(&s.bytesSaved),
		OffloadFailures:            atomic.LoadInt64(&s.offloadFailures),
		OffloadInputTokens:         atomic.LoadInt64(&s.offloadInputTokens),
		OffloadOutputTokens:        atomic.LoadInt64(&s.offloadOutputTokens),
		OffloadCacheReadTokens:     atomic.LoadInt64(&s.offloadCacheReadTokens),
		OffloadCacheWriteTokens:    atomic.LoadInt64(&s.offloadCacheWriteTokens),
		CompactionApplications:     atomic.LoadInt64(&s.compactionApplications),
		CompactionTransformations:  atomic.LoadInt64(&s.compactionTransformations),
		CompactionCacheReuses:      atomic.LoadInt64(&s.compactionCacheReuses),
		EstimatedTokensAvoided:     atomic.LoadInt64(&s.estimatedTokensAvoided),
		EstimatedRewriteSpanTokens: atomic.LoadInt64(&s.estimatedRewriteSpanTokens),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.estimatedGrossCount > 0 {
		gross := s.estimatedGrossUSD
		snap.EstimatedGrossUSD = &gross
	}
	if s.estimatedNetCount > 0 {
		net := s.estimatedNetUSD
		snap.EstimatedNetUSD = &net
	}
	if len(s.savingsUnavailable) > 0 {
		snap.SavingsUnavailable = make(map[string]int64, len(s.savingsUnavailable))
		for reason, count := range s.savingsUnavailable {
			snap.SavingsUnavailable[reason] = count
		}
	}
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
