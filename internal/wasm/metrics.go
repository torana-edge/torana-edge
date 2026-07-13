package wasm

import (
	"log"
	"sync/atomic"
)

// PluginMetrics tracks per-plugin counters auto-tagged by plugin name.
// Must be created via NewPluginMetrics().
type PluginMetrics struct {
	requests   atomic.Int64
	bytesIn    atomic.Int64
	bytesOut   atomic.Int64
}

func NewPluginMetrics() *PluginMetrics { return &PluginMetrics{} }

// Record adds a tagged plugin metric entry.
func (pm *PluginMetrics) Record(pluginName string, inBytes, outBytes int64) {
	pm.requests.Add(1)
	pm.bytesIn.Add(inBytes)
	pm.bytesOut.Add(outBytes)
	log.Printf("[metrics] plugin=%s requests=%d bytes_in=%d bytes_out=%d bytes_saved=%d",
		pluginName, pm.requests.Load(), pm.bytesIn.Load(), pm.bytesOut.Load(), pm.bytesIn.Load()-pm.bytesOut.Load())
}
