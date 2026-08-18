package metrics

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/torana-edge/torana-edge/internal/economics"
	pbjsontext "github.com/torana-edge/torana-plugin-sdk/pb/v2/jsontext"
)

// InitOTel sets up OpenTelemetry metrics if OTEL_EXPORTER_OTLP_ENDPOINT is set.
func InitOTel(ctx context.Context) (func(context.Context) error, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(endpoint), otlpmetricgrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("torana-edge")),
	)
	if err != nil {
		return nil, err
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(5*time.Second))),
		sdkmetric.WithResource(res),
	)

	otel.SetMeterProvider(provider)
	initInstruments(provider.Meter("torana.edge"))

	log.Printf("[metrics] OpenTelemetry enabled, exporting to %s", endpoint)

	return provider.Shutdown, nil
}

// initInstruments installs the meter and creates the host-owned request
// instruments. The host is the only component that sees every response
// (errors skip the plugin pipeline) and owns request timing, so these live
// here rather than in a plugin. Split out so tests can install a manual reader.
func initInstruments(m metric.Meter) {
	meter = m
	pluginMetrics = newPluginMetricRegistry()
	reqDuration, _ = m.Float64Histogram("torana_request_duration_ms", metric.WithUnit("ms"))
	reqTotal, _ = m.Int64Counter("torana_requests_total")
	tokensTotal, _ = m.Int64Counter("torana_tokens_total")
	pluginSaved, _ = m.Int64Counter("torana_bytes_saved_total")
	compactionApplications, _ = m.Int64Counter("torana_compaction_applications_total")
	compactionEstimatedTokens, _ = m.Int64Counter("torana_compaction_estimated_tokens_total")
	compactionEstimatedUSD, _ = m.Float64Counter("torana_compaction_estimated_usd_total")
	compactionUnavailable, _ = m.Int64Counter("torana_compaction_savings_unavailable_total")
	routedTotal, _ = m.Int64Counter("torana_routed_requests_total")
	pluginMetricRejected, _ = m.Int64Counter("torana_plugin_metric_rejections_total")
}

var (
	meter                     metric.Meter
	reqDuration               metric.Float64Histogram
	reqTotal                  metric.Int64Counter
	tokensTotal               metric.Int64Counter
	pluginSaved               metric.Int64Counter
	compactionApplications    metric.Int64Counter
	compactionEstimatedTokens metric.Int64Counter
	compactionEstimatedUSD    metric.Float64Counter
	compactionUnavailable     metric.Int64Counter
	routedTotal               metric.Int64Counter
	pluginMetricRejected      metric.Int64Counter
	pluginMetrics             = newPluginMetricRegistry()
)

type pluginMetricKey struct {
	plugin string
	name   string
}

type pluginMetricInstrument struct {
	typ       int
	counter   metric.Float64Counter
	histogram metric.Float64Histogram
	gauge     metric.Float64Gauge
}

// pluginMetricRegistry bounds every guest-selected dimension that the OTel
// SDK otherwise retains for the lifetime of the process. Entries are never
// evicted: eviction would let a guest keep creating new OTel aggregations even
// though this registry appeared bounded.
type pluginMetricRegistry struct {
	mu          sync.Mutex
	instruments map[string]pluginMetricInstrument
	names       map[string]map[string]struct{}
	series      map[pluginMetricKey]map[string]struct{}
}

func newPluginMetricRegistry() *pluginMetricRegistry {
	return &pluginMetricRegistry{
		instruments: map[string]pluginMetricInstrument{},
		names:       map[string]map[string]struct{}{},
		series:      map[pluginMetricKey]map[string]struct{}{},
	}
}

// RecordProxyRequest records one proxied request's latency and outcome,
// labeled by bounded model family, provider, and status class (2xx/4xx/5xx).
// Exact model is caller-controlled and deliberately never becomes a label.
// No-op unless OTel is configured.
func RecordProxyRequest(ctx context.Context, model, provider string, status int, durationMs float64) {
	if meter == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("model_family", modelFamily(model)),
		attribute.String("provider", provider),
		attribute.String("status_class", statusClass(status)),
	)
	reqDuration.Record(ctx, durationMs, attrs)
	reqTotal.Add(ctx, 1, attrs)
}

// RecordTokens records provider-reported token usage for one request, labeled
// by bounded model family, provider, and direction (input/output). No-op unless OTel is
// configured; zero counts (provider didn't report) are skipped.
func RecordTokens(ctx context.Context, model, provider string, in, out int) {
	if meter == nil || (in == 0 && out == 0) {
		return
	}
	base := []attribute.KeyValue{
		attribute.String("model_family", modelFamily(model)),
		attribute.String("provider", provider),
	}
	if in > 0 {
		tokensTotal.Add(ctx, int64(in), metric.WithAttributes(append(base, attribute.String("direction", "input"))...))
	}
	if out > 0 {
		tokensTotal.Add(ctx, int64(out), metric.WithAttributes(append(base, attribute.String("direction", "output"))...))
	}
}

// RecordCacheTokens records provider-reported prompt-cache usage for one
// request (direction=cache_read/cache_write). No-op unless OTel is configured;
// zero counts are skipped.
func RecordCacheTokens(ctx context.Context, model, provider string, read, write int) {
	if meter == nil || (read == 0 && write == 0) {
		return
	}
	base := []attribute.KeyValue{
		attribute.String("model_family", modelFamily(model)),
		attribute.String("provider", provider),
	}
	if read > 0 {
		tokensTotal.Add(ctx, int64(read), metric.WithAttributes(append(base, attribute.String("direction", "cache_read"))...))
	}
	if write > 0 {
		tokensTotal.Add(ctx, int64(write), metric.WithAttributes(append(base, attribute.String("direction", "cache_write"))...))
	}
}

// RecordRoutedRequest counts a plugin-initiated reroute
// (torana_routed_requests_total{from_provider, to_provider}). No-op unless
// OTel is configured.
func RecordRoutedRequest(ctx context.Context, from, to string) {
	if meter == nil {
		return
	}
	routedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("from_provider", from),
		attribute.String("to_provider", to),
	))
}

// RecordPluginSavings records compaction savings attributed to one plugin
// (torana_bytes_saved_total{plugin}). No-op unless OTel is configured.
func RecordPluginSavings(ctx context.Context, plugin string, savedBytes int64) {
	if meter == nil || savedBytes <= 0 {
		return
	}
	pluginSaved.Add(ctx, savedBytes, metric.WithAttributes(attribute.String("plugin", plugin)))
}

// RecordCompactionEconomics exports one applied batch. Dollar values are
// emitted only when every required operator-supplied price and usage field is
// available; otherwise a labeled unavailable counter explains why.
func RecordCompactionEconomics(ctx context.Context, plugin string, report economics.CompactionReport, targetPricing, offloadPricing *economics.ModelPricing) {
	if meter == nil {
		return
	}
	base := []attribute.KeyValue{attribute.String("plugin", plugin), attribute.String("source", report.Source)}
	compactionApplications.Add(ctx, 1, metric.WithAttributes(base...))
	if report.EstimatedTokensRemoved > 0 {
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("kind", "avoided"))
		compactionEstimatedTokens.Add(ctx, report.EstimatedTokensRemoved, metric.WithAttributes(attrs...))
	}
	if report.Source == "transformation" && report.EstimatedRewriteSpanTokens > 0 {
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("kind", "rewrite_span"))
		compactionEstimatedTokens.Add(ctx, report.EstimatedRewriteSpanTokens, metric.WithAttributes(attrs...))
	}
	if report.Source == "legacy" {
		return
	}
	if targetPricing == nil {
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("reason", economics.UnavailablePricing))
		compactionUnavailable.Add(ctx, 1, metric.WithAttributes(attrs...))
		return
	}
	est := economics.EstimateApplicationSavings(report, *targetPricing, offloadPricing)
	if est.EstimatedGrossUSD != nil {
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("kind", "gross"))
		compactionEstimatedUSD.Add(ctx, *est.EstimatedGrossUSD, metric.WithAttributes(attrs...))
	}
	if est.EstimatedNetUSD != nil {
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("kind", "net"))
		compactionEstimatedUSD.Add(ctx, *est.EstimatedNetUSD, metric.WithAttributes(attrs...))
	}
	if est.UnavailableReason != "" {
		attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("reason", est.UnavailableReason))
		compactionUnavailable.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "other"
	}
}

// RegisterStatsObservables bridges the running StatsTracker to OTLP as
// observable counters, so throughput and offload failures export without any
// plugin. Savings bytes are NOT bridged here — they export as the labeled
// sync counter torana_bytes_saved_total{plugin} (see RecordPluginSavings);
// registering the same name as an observable would conflict.
// No-op unless OTel is configured. Call once after InitOTel.
func RegisterStatsObservables(st *StatsTracker) {
	if meter == nil || st == nil {
		return
	}
	compactions, _ := meter.Int64ObservableCounter("torana_compactions_total")
	offloadFails, _ := meter.Int64ObservableCounter("torana_offload_failures_total")
	auditWriteFails, _ := meter.Int64ObservableCounter("torana_audit_write_failures_total")
	bytesIn, _ := meter.Int64ObservableCounter("torana_bytes_in_total")
	bytesOut, _ := meter.Int64ObservableCounter("torana_bytes_out_total")
	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := st.Snapshot()
		o.ObserveInt64(compactions, s.Compactions)
		o.ObserveInt64(offloadFails, s.OffloadFailures)
		o.ObserveInt64(auditWriteFails, s.AuditWriteFailures)
		o.ObserveInt64(bytesIn, s.TotalBytesIn)
		o.ObserveInt64(bytesOut, s.TotalBytesOut)
		return nil
	}, compactions, offloadFails, auditWriteFails, bytesIn, bytesOut)
}

// EmitPluginMetric records a custom metric emitted by a WASM plugin, tagged
// with the plugin name plus bounded plugin-supplied labels. Invalid or
// over-limit updates increment torana_plugin_metric_rejections_total with a
// host-owned reason instead of silently disappearing or retaining guest data.
// type: 0=counter, 1=histogram, 2=gauge
func EmitPluginMetric(ctx context.Context, pluginName, metricName string, metricType int, value float64, labels map[string]string) {
	if meter == nil {
		return
	}
	attrs, series, reason := validatePluginMetric(metricName, metricType, value, labels)
	if reason != "" {
		recordPluginMetricRejection(ctx, pluginName, reason)
		return
	}
	instrument, reason := pluginMetrics.admit(meter, pluginName, metricName, metricType, series)
	if reason != "" {
		recordPluginMetricRejection(ctx, pluginName, reason)
		return
	}
	attrs = append([]attribute.KeyValue{attribute.String("plugin", pluginName)}, attrs...)
	opt := metric.WithAttributes(attrs...)
	switch instrument.typ {
	case 0:
		instrument.counter.Add(ctx, value, opt)
	case 1:
		instrument.histogram.Record(ctx, value, opt)
	case 2:
		instrument.gauge.Record(ctx, value, opt)
	}
}

// EmitPluginMetricJSON is the handwritten-guest boundary. It rejects malformed,
// duplicate-key, non-object, null, and parser-differential label documents
// before decoding them; the Go SDK emits either no bytes or a valid object.
func EmitPluginMetricJSON(ctx context.Context, pluginName, metricName string, metricType int, value float64, labelsJSON []byte) {
	var labels map[string]string
	if len(labelsJSON) > 0 {
		if err := pbjsontext.Validate(labelsJSON); err != nil || json.Unmarshal(labelsJSON, &labels) != nil || labels == nil {
			recordPluginMetricRejection(ctx, pluginName, "invalid_labels_json")
			return
		}
	}
	EmitPluginMetric(ctx, pluginName, metricName, metricType, value, labels)
}

func validatePluginMetric(name string, typ int, value float64, labels map[string]string) ([]attribute.KeyValue, string, string) {
	if typ < 0 || typ > 2 {
		return nil, "", "invalid_type"
	}
	if !validPluginTelemetryName(name) || hostMetricNames[name] {
		return nil, "", "invalid_name"
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, "", "invalid_value"
	}
	if len(labels) > maxPluginMetricLabels {
		return nil, "", "too_many_labels"
	}
	keys := make([]string, 0, len(labels))
	for key, value := range labels {
		if !validPluginTelemetryName(key) || key == "plugin" {
			return nil, "", "invalid_label_key"
		}
		if !validPluginLabelValue(value) {
			return nil, "", "invalid_label_value"
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attrs := make([]attribute.KeyValue, 0, len(keys))
	framed := make([]byte, 0, len(keys)*32)
	var frame [8]byte
	for _, key := range keys {
		value := labels[key]
		binary.LittleEndian.PutUint64(frame[:], uint64(len(key)))
		framed = append(framed, frame[:]...)
		framed = append(framed, key...)
		binary.LittleEndian.PutUint64(frame[:], uint64(len(value)))
		framed = append(framed, frame[:]...)
		framed = append(framed, value...)
		attrs = append(attrs, attribute.String(key, value))
	}
	return attrs, string(framed), ""
}

func (r *pluginMetricRegistry) admit(m metric.Meter, pluginName, metricName string, metricType int, series string) (pluginMetricInstrument, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pluginNames := r.names[pluginName]
	_, nameKnown := pluginNames[metricName]
	if !nameKnown && len(pluginNames) >= maxPluginMetricNames {
		return pluginMetricInstrument{}, "name_limit"
	}
	instrument, instrumentKnown := r.instruments[metricName]
	if instrumentKnown && instrument.typ != metricType {
		return pluginMetricInstrument{}, "type_conflict"
	}
	if !instrumentKnown {
		var err error
		instrument.typ = metricType
		switch metricType {
		case 0:
			instrument.counter, err = m.Float64Counter(metricName)
		case 1:
			instrument.histogram, err = m.Float64Histogram(metricName)
		case 2:
			instrument.gauge, err = m.Float64Gauge(metricName)
		}
		if err != nil {
			return pluginMetricInstrument{}, "instrument_error"
		}
	}
	key := pluginMetricKey{plugin: pluginName, name: metricName}
	knownSeries := r.series[key]
	if _, ok := knownSeries[series]; !ok && len(knownSeries) >= maxPluginMetricSeries {
		return pluginMetricInstrument{}, "series_limit"
	}
	if pluginNames == nil {
		pluginNames = map[string]struct{}{}
		r.names[pluginName] = pluginNames
	}
	pluginNames[metricName] = struct{}{}
	if !instrumentKnown {
		r.instruments[metricName] = instrument
	}
	if knownSeries == nil {
		knownSeries = map[string]struct{}{}
		r.series[key] = knownSeries
	}
	knownSeries[series] = struct{}{}
	return instrument, ""
}

var hostMetricNames = map[string]bool{
	"torana_request_duration_ms":                  true,
	"torana_requests_total":                       true,
	"torana_tokens_total":                         true,
	"torana_bytes_saved_total":                    true,
	"torana_compaction_applications_total":        true,
	"torana_compaction_estimated_tokens_total":    true,
	"torana_compaction_estimated_usd_total":       true,
	"torana_compaction_savings_unavailable_total": true,
	"torana_routed_requests_total":                true,
	"torana_compactions_total":                    true,
	"torana_offload_failures_total":               true,
	"torana_audit_write_failures_total":           true,
	"torana_bytes_in_total":                       true,
	"torana_bytes_out_total":                      true,
	"torana_plugin_metric_rejections_total":       true,
}

func recordPluginMetricRejection(ctx context.Context, pluginName, reason string) {
	if pluginMetricRejected == nil {
		return
	}
	pluginMetricRejected.Add(ctx, 1, metric.WithAttributes(
		attribute.String("plugin", pluginName),
		attribute.String("reason", reason),
	))
}
