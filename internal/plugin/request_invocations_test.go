package plugin

import (
	"slices"
	"sync"
	"testing"
)

func TestRequestPluginInvocationsAreOrderedUniqueAndScoped(t *testing.T) {
	pp := &PluginPipeline{requestPlugins: make(map[uint64]*requestPluginInvocations)}
	pp.recordInvocation(7, "pii")
	pp.recordInvocation(7, "intent")
	pp.recordInvocation(7, "pii")
	pp.recordInvocation(8, "otel")

	got := pp.InvokedPlugins(7)
	if !slices.Equal(got, []string{"pii", "intent"}) {
		t.Fatalf("request 7 plugins = %v", got)
	}
	got[0] = "tampered"
	if fresh := pp.InvokedPlugins(7); !slices.Equal(fresh, []string{"pii", "intent"}) {
		t.Fatalf("caller mutated invocation state: %v", fresh)
	}
	if got := pp.InvokedPlugins(8); !slices.Equal(got, []string{"otel"}) {
		t.Fatalf("request 8 plugins = %v", got)
	}
	pp.dropRequestTracking(7)
	if got := pp.InvokedPlugins(7); got != nil {
		t.Fatalf("finished request retained plugin telemetry: %v", got)
	}
}

func TestRequestPluginInvocationRecordingIsConcurrentSafe(t *testing.T) {
	pp := &PluginPipeline{requestPlugins: make(map[uint64]*requestPluginInvocations)}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pp.recordInvocation(9, "stream-observer")
		}()
	}
	wg.Wait()
	if got := pp.InvokedPlugins(9); !slices.Equal(got, []string{"stream-observer"}) {
		t.Fatalf("concurrent plugins = %v", got)
	}
}
