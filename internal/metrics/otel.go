package metrics

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
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
	meter = provider.Meter("torana.edge.plugins")

	log.Printf("[metrics] OpenTelemetry enabled, exporting to %s", endpoint)

	return provider.Shutdown, nil
}

var (
	meter          metric.Meter
	counterCache   sync.Map
	histogramCache sync.Map
)

// EmitPluginMetric records a custom metric emitted by a WASM plugin.
// type: 0=counter, 1=histogram, 2=gauge
func EmitPluginMetric(ctx context.Context, pluginName, metricName string, metricType int, value float64) {
	if meter == nil {
		return
	}

	switch metricType {
	case 0:
		v, ok := counterCache.Load(metricName)
		if !ok {
			c, _ := meter.Float64Counter(metricName)
			v, _ = counterCache.LoadOrStore(metricName, c)
		}
		c := v.(metric.Float64Counter)
		c.Add(ctx, value, metric.WithAttributes(semconv.ServiceNameKey.String(pluginName)))
	case 1:
		v, ok := histogramCache.Load(metricName)
		if !ok {
			h, _ := meter.Float64Histogram(metricName)
			v, _ = histogramCache.LoadOrStore(metricName, h)
		}
		h := v.(metric.Float64Histogram)
		h.Record(ctx, value, metric.WithAttributes(semconv.ServiceNameKey.String(pluginName)))
	case 2:
		// Not implementing gauge cache for simplicity
	}
}
