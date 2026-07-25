package metrics

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/torana-edge/torana-edge/internal/economics"
)

// collect installs a manual-reader meter and returns a collect function.
func collect(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	initInstruments(provider.Meter("test"))
	t.Cleanup(func() {
		meter = nil
		reqDuration, reqTotal, tokensTotal, pluginSaved = nil, nil, nil, nil
		compactionApplications, compactionEstimatedTokens = nil, nil
		compactionEstimatedUSD, compactionUnavailable = nil, nil
	})
	return func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
	}
}

func TestRecordCompactionEconomics(t *testing.T) {
	do := collect(t)
	read, write := 0.5, 1.0
	pricing := economics.ModelPricing{CacheReadUSDPerMTok: &read, CacheWriteUSDPerMTok: &write}
	RecordCompactionEconomics(context.Background(), "compactor", economics.CompactionReport{
		OriginalBytes: 40_000, FinalBytes: 4_000, EstimatedTokensRemoved: 9_000,
		EstimatedRewriteSpanTokens: 2_000, Source: "transformation",
	}, &pricing, nil)
	names := metricNames(do())
	for _, want := range []string{
		"torana_compaction_applications_total",
		"torana_compaction_estimated_tokens_total",
		"torana_compaction_estimated_usd_total",
	} {
		if !names[want] {
			t.Fatalf("missing economics metric %q: %v", want, names)
		}
	}
}

func metricNames(rm metricdata.ResourceMetrics) map[string]bool {
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	return names
}

// TestRecordProxyRequest: host request metrics are emitted with model/provider/
// status labels.
func TestRecordProxyRequest(t *testing.T) {
	do := collect(t)
	RecordProxyRequest(context.Background(), "gpt-x", "oai", 200, 12.5)
	RecordProxyRequest(context.Background(), "gpt-x", "oai", 503, 1.0)

	names := metricNames(do())
	if !names["torana_request_duration_ms"] || !names["torana_requests_total"] {
		t.Fatalf("missing host request metrics: %v", names)
	}
}

// TestStatsObservablesBridge: the StatsTracker's cumulative counters are
// exported to OTLP without any plugin. Savings bytes are NOT bridged — they
// export as the labeled sync counter (see TestRecordPluginSavings).
func TestStatsObservablesBridge(t *testing.T) {
	do := collect(t)
	st := NewStatsTracker()
	st.RecordCompaction("compactor", 1000, 200) // BytesSaved += 800
	st.RecordOffloadFailure()
	RegisterStatsObservables(st)

	names := metricNames(do())
	for _, want := range []string{"torana_compactions_total", "torana_offload_failures_total"} {
		if !names[want] {
			t.Fatalf("missing bridged stat %q: %v", want, names)
		}
	}
	if names["torana_bytes_saved_total"] {
		t.Fatal("bytes_saved must not be bridged as an observable (conflicts with the labeled sync counter)")
	}
}

// TestRecordPluginSavings: savings export as a sync counter labeled by plugin.
func TestRecordPluginSavings(t *testing.T) {
	do := collect(t)
	RecordPluginSavings(context.Background(), "compactor", 800)

	rm := do()
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "torana_bytes_saved_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) == 0 {
				t.Fatalf("unexpected data for %s", m.Name)
			}
			dp := sum.DataPoints[0]
			plugin, _ := dp.Attributes.Value("plugin")
			if dp.Value != 800 || plugin.AsString() != "compactor" {
				t.Fatalf("wrong datapoint: value=%d plugin=%q", dp.Value, plugin.AsString())
			}
			found = true
		}
	}
	if !found {
		t.Fatal("torana_bytes_saved_total not emitted")
	}
}

// TestRecordTokens: token usage exports labeled by model/provider/direction.
func TestRecordTokens(t *testing.T) {
	do := collect(t)
	RecordTokens(context.Background(), "gpt-x", "oai", 120, 45)

	rm := do()
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "torana_tokens_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data for %s", m.Name)
			}
			for _, dp := range sum.DataPoints {
				dir, _ := dp.Attributes.Value("direction")
				got[dir.AsString()] = dp.Value
			}
		}
	}
	if got["input"] != 120 || got["output"] != 45 {
		t.Fatalf("token datapoints wrong: %v", got)
	}
}

// TestEmitPluginMetricGauge: gauge metric type records the latest value.
func TestEmitPluginMetricGauge(t *testing.T) {
	do := collect(t)
	EmitPluginMetric(context.Background(), "x", "torana_plugin_queue_depth", 2, 7, nil)
	EmitPluginMetric(context.Background(), "x", "torana_plugin_queue_depth", 2, 3, nil)

	rm := do()
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "torana_plugin_queue_depth" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[float64])
			if !ok || len(g.DataPoints) == 0 {
				t.Fatalf("expected gauge data, got %T", m.Data)
			}
			if g.DataPoints[0].Value != 3 {
				t.Fatalf("gauge should hold latest value 3, got %v", g.DataPoints[0].Value)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("gauge metric not emitted")
	}
}

// TestEmitPluginMetricLabels: a plugin metric carries the plugin name plus its
// supplied labels.
func TestEmitPluginMetricLabels(t *testing.T) {
	do := collect(t)
	EmitPluginMetric(context.Background(), "otel", "torana_plugin_requests_total", 0, 1, map[string]string{"model": "gpt-x"})

	rm := do()
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "torana_plugin_requests_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[float64])
			if !ok || len(sum.DataPoints) == 0 {
				t.Fatalf("unexpected data for %s", m.Name)
			}
			attrs := sum.DataPoints[0].Attributes
			plugin, _ := attrs.Value("plugin")
			model, _ := attrs.Value("model")
			if plugin.AsString() != "otel" || model.AsString() != "gpt-x" {
				t.Fatalf("labels wrong: plugin=%q model=%q", plugin.AsString(), model.AsString())
			}
			found = true
		}
	}
	if !found {
		t.Fatal("torana_plugin_requests_total not emitted")
	}
}

// TestMetricsDisabledNoop: with no meter, the emit paths are safe no-ops.
func TestMetricsDisabledNoop(t *testing.T) {
	meter = nil
	RecordProxyRequest(context.Background(), "m", "p", 200, 1)
	EmitPluginMetric(context.Background(), "x", "y", 0, 1, nil)
	RegisterStatsObservables(NewStatsTracker())
}
