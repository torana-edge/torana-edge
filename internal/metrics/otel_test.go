package metrics

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collect installs a manual-reader meter and returns a collect function.
func collect(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	initInstruments(provider.Meter("test"))
	t.Cleanup(func() { meter = nil; reqDuration = nil; reqTotal = nil })
	return func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
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
// exported to OTLP without any plugin.
func TestStatsObservablesBridge(t *testing.T) {
	do := collect(t)
	st := NewStatsTracker()
	st.RecordCompaction(1000, 200) // BytesSaved += 800
	st.RecordOffloadFailure()
	RegisterStatsObservables(st)

	names := metricNames(do())
	for _, want := range []string{"torana_bytes_saved_total", "torana_compactions_total", "torana_offload_failures_total"} {
		if !names[want] {
			t.Fatalf("missing bridged stat %q: %v", want, names)
		}
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
