package metrics

import (
	"context"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"testing"
)

func TestMCPConnectionMetricsBoundLabelsAndIgnoreUnknownCatalogs(t *testing.T) {
	read := collect(t)
	RecordMCPConnection(context.Background(), "codex-thread", "present")
	RecordMCPConnection(context.Background(), "untrusted-header", "absent")
	RecordMCPConnection(context.Background(), "untrusted-header", "not_observed")
	RecordMCPConnection(context.Background(), "untrusted-header", "untrusted-state")
	got := map[string]int64{}
	for _, scope := range read().ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != "torana_mcp_connected" {
				continue
			}
			for _, point := range instrument.Data.(metricdata.Sum[int64]).DataPoints {
				harness, _ := point.Attributes.Value("harness")
				state, _ := point.Attributes.Value("state")
				got[harness.AsString()+"/"+state.AsString()] = point.Value
			}
		}
	}
	if len(got) != 2 || got["codex-thread/present"] != 1 || got["other/absent"] != 1 {
		t.Fatalf("series=%v", got)
	}
}
