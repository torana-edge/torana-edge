package proxy

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/torana-edge/torana-plugin-sdk/pb"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

// The tick scheduler drives run_on_tick, the only hook that fires with no
// request in flight.
//
// It is off unless an operator sets plugins.runtime.tick_interval_seconds AND
// some loaded plugin both declares the hook and holds env.background_tick.
// Background execution is opt-in twice over on purpose: a plugin running
// outside any request is work an operator cannot see in a trace, and it may
// spend money.

type tickScheduler struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// startTicker launches the scheduler when configured and wanted, and returns
// nil otherwise. A nil scheduler is inert, so the caller needs no nil checks.
func (s *Server) startTicker(interval time.Duration) *tickScheduler {
	if interval <= 0 {
		return nil
	}
	t := &tickScheduler{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.tickLoop(t, interval)
	return t
}

func (t *tickScheduler) Close() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stop) })
	<-t.done
}

func (s *Server) tickLoop(t *tickScheduler, interval time.Duration) {
	defer close(t.done)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var tickID uint64
	logged := false
	for {
		select {
		case <-t.stop:
			return
		case now := <-ticker.C:
			raw := s.pluginPipeline.Load()
			if raw == nil {
				continue
			}
			pp, ok := raw.(*plugin.PluginPipeline)
			if !ok || !pp.TicksEnabled() {
				continue
			}
			if !logged {
				log.Printf("[tick] background plugin ticks running every %s", interval)
				logged = true
			}
			tickID++
			s.runTick(pp, tickID, now, interval)
		}
	}
}

// runTick fires one tick across the pipeline.
//
// The pipeline is pinned for the duration so a concurrent hot-reload cannot
// close the runtime underneath a running hook, and each tick takes a request ID
// from the same counter the request path uses. Sharing the counter matters:
// plugin state is keyed by request ID host-side, so a tick that reused an ID
// would read another request's scratch space.
func (s *Server) runTick(pp *plugin.PluginPipeline, tickID uint64, now time.Time, interval time.Duration) {
	if !pp.TryAcquire() {
		return // draining; the next tick will find the new pipeline
	}
	defer pp.Release()

	reqID := reqCounter.Add(1)
	defer pp.EndRequest(reqID)

	// Bound a tick to its own interval. A hook that hangs must not let the next
	// tick pile up behind it, and a plugin with nothing to do returns at once.
	ctx, cancel := context.WithTimeout(context.Background(), interval)
	defer cancel()

	outcomes := pp.RunOnTick(ctx, reqID, &pb.TickRequest{
		TickId:     tickID,
		UnixMillis: now.UnixMilli(),
		IntervalMs: interval.Milliseconds(),
	})

	for _, o := range outcomes {
		s.stats.RecordPluginCounter(o.Plugin, "tick_actions", int64(o.Actions))
		if o.Note != "" {
			log.Printf("[tick] %s: %s", o.Plugin, o.Note)
		}
	}
}
