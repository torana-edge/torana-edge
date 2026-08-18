package metrics

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
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
		pluginMetricRejected = nil
		pluginMetrics = newPluginMetricRegistry()
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

// TestRecordProxyRequest: host request metrics are emitted with bounded model
// family/provider/status labels.
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
	st.RecordAuditWriteFailure()
	RegisterStatsObservables(st)

	names := metricNames(do())
	for _, want := range []string{"torana_compactions_total", "torana_offload_failures_total", "torana_audit_write_failures_total"} {
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

func TestModelMetricLabelsHaveFiniteCardinality(t *testing.T) {
	want := map[string]string{
		"claude-sonnet-4": "claude", "GPT-5": "openai", "o3-mini": "openai",
		"gemini-2.5-pro": "gemini", "deepseek-r1": "deepseek",
		"meta-llama/llama-4": "llama", "mixtral-8x7b": "mistral",
		"qwen3-coder": "qwen", "command-r": "command", "grok-4": "grok",
		"attacker-unique-1": "other", "": "other",
	}
	for input, family := range want {
		if got := modelFamily(input); got != family {
			t.Errorf("modelFamily(%q) = %q, want %q", input, got, family)
		}
	}

	seen := map[string]bool{}
	for i := range 10_000 {
		seen[modelFamily(fmt.Sprintf("client-controlled-%d", i))] = true
	}
	if len(seen) != 1 || !seen["other"] {
		t.Fatalf("10k attacker labels produced %v, want only other", seen)
	}
}

func TestRecordedMetricsNeverCarryExactModel(t *testing.T) {
	do := collect(t)
	secretModel := "client-secret-model-7f3d"
	RecordProxyRequest(context.Background(), secretModel, "oai", 200, 1)
	RecordTokens(context.Background(), secretModel, "oai", 1, 1)
	RecordCacheTokens(context.Background(), secretModel, "oai", 1, 1)
	for _, sm := range do().ScopeMetrics {
		for _, metric := range sm.Metrics {
			switch data := metric.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					assertBoundedModelAttributes(t, point.Attributes, secretModel)
				}
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					assertBoundedModelAttributes(t, point.Attributes, secretModel)
				}
			}
		}
	}
}

func assertBoundedModelAttributes(t *testing.T, attrs attribute.Set, forbidden string) {
	t.Helper()
	if _, present := attrs.Value("model"); present {
		t.Fatal("exact model label is present")
	}
	family, present := attrs.Value("model_family")
	if present && (family.AsString() != "other" || family.AsString() == forbidden) {
		t.Fatalf("model_family = %q", family.AsString())
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

func TestPluginMetricNamesAndSeriesAreBounded(t *testing.T) {
	do := collect(t)
	ctx := context.Background()
	for i := range maxPluginMetricNames + 100 {
		EmitPluginMetric(ctx, "attacker", fmt.Sprintf("attacker_metric_%03d", i), 0, 1, nil)
	}
	for i := range maxPluginMetricSeries + 100 {
		EmitPluginMetric(ctx, "attacker", "attacker_metric_000", 0, 1, map[string]string{"request": fmt.Sprintf("%03d", i)})
	}

	pluginMetrics.mu.Lock()
	nameCount := len(pluginMetrics.names["attacker"])
	seriesCount := len(pluginMetrics.series[pluginMetricKey{plugin: "attacker", name: "attacker_metric_000"}])
	instrumentCount := len(pluginMetrics.instruments)
	pluginMetrics.mu.Unlock()
	if nameCount != maxPluginMetricNames || instrumentCount != maxPluginMetricNames {
		t.Fatalf("metric names grew beyond cap: plugin=%d instruments=%d want=%d", nameCount, instrumentCount, maxPluginMetricNames)
	}
	if seriesCount != maxPluginMetricSeries {
		t.Fatalf("metric series grew to %d, want cap %d", seriesCount, maxPluginMetricSeries)
	}
	assertPluginMetricRejections(t, do(), map[string]int64{
		"name_limit":   100,
		"series_limit": 101, // nil labels already occupied one series.
	})
}

func TestPluginMetricInvalidInputsUseFixedRejectionReasons(t *testing.T) {
	do := collect(t)
	ctx := context.Background()
	rows := []struct {
		name   string
		typ    int
		value  float64
		labels map[string]string
		reason string
	}{
		{name: "bad/name", typ: 0, value: 1, reason: "invalid_name"},
		{name: "valid_name", typ: 9, value: 1, reason: "invalid_type"},
		{name: "valid_name", typ: 0, value: math.NaN(), reason: "invalid_value"},
		{name: "valid_name", typ: 0, value: 1, labels: map[string]string{"bad/key": "v"}, reason: "invalid_label_key"},
		{name: "valid_name", typ: 0, value: 1, labels: map[string]string{"plugin": "spoofed"}, reason: "invalid_label_key"},
		{name: "valid_name", typ: 0, value: 1, labels: map[string]string{"key": strings.Repeat("v", maxPluginLabelValue+1)}, reason: "invalid_label_value"},
		{name: "valid_name", typ: 0, value: 1, labels: nineLabels(), reason: "too_many_labels"},
		{name: "torana_requests_total", typ: 0, value: 1, reason: "invalid_name"},
	}
	want := map[string]int64{}
	for _, row := range rows {
		EmitPluginMetric(ctx, "attacker", row.name, row.typ, row.value, row.labels)
		want[row.reason]++
	}
	EmitPluginMetric(ctx, "attacker", "same_name", 0, 1, nil)
	EmitPluginMetric(ctx, "attacker", "same_name", 1, 1, nil)
	want["type_conflict"]++

	pluginMetrics.mu.Lock()
	if got := len(pluginMetrics.names["attacker"]); got != 1 {
		t.Errorf("invalid updates retained %d names, want only same_name", got)
	}
	pluginMetrics.mu.Unlock()
	assertPluginMetricRejections(t, do(), want)
}

func TestPluginMetricJSONFailsClosed(t *testing.T) {
	do := collect(t)
	ctx := context.Background()
	for _, raw := range [][]byte{
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(`{"a":"1","\u0061":"2"}`),
		[]byte(`{"a":`),
		{'{', '"', 'a', '"', ':', '"', 0xff, '"', '}'},
	} {
		EmitPluginMetricJSON(ctx, "attacker", "valid_name", 0, 1, raw)
	}
	EmitPluginMetricJSON(ctx, "attacker", "valid_name", 0, 1, []byte(`{"a":"1"}`))
	pluginMetrics.mu.Lock()
	seriesCount := len(pluginMetrics.series[pluginMetricKey{plugin: "attacker", name: "valid_name"}])
	pluginMetrics.mu.Unlock()
	if seriesCount != 1 {
		t.Fatalf("valid label object produced %d series, want 1", seriesCount)
	}
	assertPluginMetricRejections(t, do(), map[string]int64{"invalid_labels_json": 5})
}

func TestPluginMetricSeriesFramingIsExactAndOrderIndependent(t *testing.T) {
	_ = collect(t)
	ctx := context.Background()
	EmitPluginMetric(ctx, "plugin", "series_metric", 0, 1, map[string]string{"a": "1", "b": "2"})
	EmitPluginMetric(ctx, "plugin", "series_metric", 0, 1, map[string]string{"b": "2", "a": "1"})
	EmitPluginMetric(ctx, "plugin", "series_metric", 0, 1, map[string]string{"ab": "c"})
	EmitPluginMetric(ctx, "plugin", "series_metric", 0, 1, map[string]string{"a": "bc"})
	pluginMetrics.mu.Lock()
	got := len(pluginMetrics.series[pluginMetricKey{plugin: "plugin", name: "series_metric"}])
	pluginMetrics.mu.Unlock()
	if got != 3 {
		t.Fatalf("series framing retained %d sets, want 3 (one replay and two boundary-distinct)", got)
	}
}

func TestPluginMetricRegistryConcurrentBound(t *testing.T) {
	do := collect(t)
	var wg sync.WaitGroup
	for i := range 256 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			EmitPluginMetric(context.Background(), "parallel", fmt.Sprintf("metric_%03d", i), 0, 1, map[string]string{"series": fmt.Sprint(i)})
		}()
	}
	wg.Wait()
	pluginMetrics.mu.Lock()
	got := len(pluginMetrics.names["parallel"])
	pluginMetrics.mu.Unlock()
	if got != maxPluginMetricNames {
		t.Fatalf("concurrent registry retained %d names, want %d", got, maxPluginMetricNames)
	}
	// Collection proves all admitted instruments and the rejection counter are
	// valid under the SDK, not merely bounded in our side registry.
	if !metricNames(do())["torana_plugin_metric_rejections_total"] {
		t.Fatal("concurrent overflow emitted no rejection signal")
	}
}

func nineLabels() map[string]string {
	labels := map[string]string{}
	for i := range maxPluginMetricLabels + 1 {
		labels[fmt.Sprintf("label%d", i)] = "value"
	}
	return labels
}

func assertPluginMetricRejections(t *testing.T, rm metricdata.ResourceMetrics, want map[string]int64) {
	t.Helper()
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "torana_plugin_metric_rejections_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("rejection data = %T, want int64 sum", m.Data)
			}
			for _, point := range sum.DataPoints {
				reason, _ := point.Attributes.Value("reason")
				plugin, _ := point.Attributes.Value("plugin")
				if plugin.AsString() != "attacker" {
					t.Fatalf("rejection attributed to %q, want attacker", plugin.AsString())
				}
				got[reason.AsString()] += point.Value
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rejections = %v, want %v", got, want)
	}
}

// TestMetricsDisabledNoop: with no meter, the emit paths are safe no-ops.
func TestMetricsDisabledNoop(t *testing.T) {
	meter = nil
	RecordProxyRequest(context.Background(), "m", "p", 200, 1)
	EmitPluginMetric(context.Background(), "x", "y", 0, 1, nil)
	RegisterStatsObservables(NewStatsTracker())
}
