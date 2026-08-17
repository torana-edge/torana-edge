//go:build torana_benchmark_profile

package main

// This file is deliberately excluded from release builds. It gives the
// production-shaped benchmark a loopback-only profiling surface without
// shipping pprof or heap facts in the product binary.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"runtime"
	"time"
)

const benchmarkProfileAddrEnv = "TORANA_BENCH_PROFILE_ADDR"

type benchmarkMemStats struct {
	Alloc         uint64  `json:"alloc_bytes"`
	TotalAlloc    uint64  `json:"total_alloc_bytes"`
	HeapAlloc     uint64  `json:"heap_alloc_bytes"`
	HeapSys       uint64  `json:"heap_sys_bytes"`
	HeapIdle      uint64  `json:"heap_idle_bytes"`
	HeapInuse     uint64  `json:"heap_inuse_bytes"`
	HeapReleased  uint64  `json:"heap_released_bytes"`
	StackInuse    uint64  `json:"stack_inuse_bytes"`
	Mallocs       uint64  `json:"mallocs"`
	Frees         uint64  `json:"frees"`
	NumGC         uint32  `json:"num_gc"`
	PauseTotalNS  uint64  `json:"pause_total_ns"`
	GCCPUFraction float64 `json:"gc_cpu_fraction"`
}

func init() {
	addr := os.Getenv(benchmarkProfileAddrEnv)
	if addr == "" {
		return
	}
	if err := validateBenchmarkProfileAddr(addr); err != nil {
		log.Fatalf("invalid %s: %v", benchmarkProfileAddrEnv, err)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen on benchmark profile address: %v", err)
	}
	server := &http.Server{
		Handler:           benchmarkProfileMux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("Benchmark profile server stopped: %v", err)
		}
	}()
}

func validateBenchmarkProfileAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("host %q must be a literal loopback IP", host)
	}
	if port == "" {
		return fmt.Errorf("port is required")
	}
	return nil
}

func benchmarkProfileMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/torana/memstats", serveBenchmarkMemStats)
	mux.HandleFunc("/debug/pprof/", httppprof.Index)
	mux.Handle("/debug/pprof/allocs", httppprof.Handler("allocs"))
	mux.Handle("/debug/pprof/heap", httppprof.Handler("heap"))
	return mux
}

func serveBenchmarkMemStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Query().Get("gc") {
	case "":
	case "1":
		runtime.GC()
	default:
		http.Error(w, "gc must be absent or 1", http.StatusBadRequest)
		return
	}

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(benchmarkMemStats{
		Alloc: stats.Alloc, TotalAlloc: stats.TotalAlloc,
		HeapAlloc: stats.HeapAlloc, HeapSys: stats.HeapSys,
		HeapIdle: stats.HeapIdle, HeapInuse: stats.HeapInuse,
		HeapReleased: stats.HeapReleased, StackInuse: stats.StackInuse,
		Mallocs: stats.Mallocs, Frees: stats.Frees, NumGC: stats.NumGC,
		PauseTotalNS: stats.PauseTotalNs, GCCPUFraction: stats.GCCPUFraction,
	})
}
