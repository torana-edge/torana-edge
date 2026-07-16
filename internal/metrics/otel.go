package metrics

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
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
	reqDuration, _ = m.Float64Histogram("torana_request_duration_ms", metric.WithUnit("ms"))
	reqTotal, _ = m.Int64Counter("torana_requests_total")
}

var (
	meter          metric.Meter
	reqDuration    metric.Float64Histogram
	reqTotal       metric.Int64Counter
	counterCache   sync.Map
	histogramCache sync.Map
)

// RecordProxyRequest records one proxied request's latency and outcome,
// labeled by model, provider, and status class (2xx/4xx/5xx). No-op unless OTel
// is configured.
func RecordProxyRequest(ctx context.Context, model, provider string, status int, durationMs float64) {
	if meter == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("model", model),
		attribute.String("provider", provider),
		attribute.String("status_class", statusClass(status)),
	)
	reqDuration.Record(ctx, durationMs, attrs)
	reqTotal.Add(ctx, 1, attrs)
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
// observable counters, so compaction savings and offload failures export
// without any plugin. No-op unless OTel is configured. Call once after InitOTel.
func RegisterStatsObservables(st *StatsTracker) {
	if meter == nil || st == nil {
		return
	}
	bytesSaved, _ := meter.Int64ObservableCounter("torana_bytes_saved_total")
	compactions, _ := meter.Int64ObservableCounter("torana_compactions_total")
	offloadFails, _ := meter.Int64ObservableCounter("torana_offload_failures_total")
	bytesIn, _ := meter.Int64ObservableCounter("torana_bytes_in_total")
	bytesOut, _ := meter.Int64ObservableCounter("torana_bytes_out_total")
	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := st.Snapshot()
		o.ObserveInt64(bytesSaved, s.BytesSaved)
		o.ObserveInt64(compactions, s.Compactions)
		o.ObserveInt64(offloadFails, s.OffloadFailures)
		o.ObserveInt64(bytesIn, s.TotalBytesIn)
		o.ObserveInt64(bytesOut, s.TotalBytesOut)
		return nil
	}, bytesSaved, compactions, offloadFails, bytesIn, bytesOut)
}

// EmitPluginMetric records a custom metric emitted by a WASM plugin, tagged
// with the plugin name plus any plugin-supplied labels.
// type: 0=counter, 1=histogram, 2=gauge
func EmitPluginMetric(ctx context.Context, pluginName, metricName string, metricType int, value float64, labels map[string]string) {
	if meter == nil {
		return
	}

	attrs := make([]attribute.KeyValue, 0, len(labels)+1)
	attrs = append(attrs, attribute.String("plugin", pluginName))
	for k, v := range labels {
		attrs = append(attrs, attribute.String(k, v))
	}
	opt := metric.WithAttributes(attrs...)

	switch metricType {
	case 0:
		v, ok := counterCache.Load(metricName)
		if !ok {
			c, _ := meter.Float64Counter(metricName)
			v, _ = counterCache.LoadOrStore(metricName, c)
		}
		v.(metric.Float64Counter).Add(ctx, value, opt)
	case 1:
		v, ok := histogramCache.Load(metricName)
		if !ok {
			h, _ := meter.Float64Histogram(metricName)
			v, _ = histogramCache.LoadOrStore(metricName, h)
		}
		v.(metric.Float64Histogram).Record(ctx, value, opt)
	case 2:
		// Not implementing gauge cache for simplicity
	}
}
