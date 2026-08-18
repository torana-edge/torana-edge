//go:build torana_benchmark_profile

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateBenchmarkProfileAddr(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		ok   bool
	}{
		{name: "IPv4 loopback", addr: "127.0.0.1:18082", ok: true},
		{name: "IPv6 loopback", addr: "[::1]:18082", ok: true},
		{name: "wildcard", addr: "0.0.0.0:18082"},
		{name: "public", addr: "192.0.2.1:18082"},
		{name: "hostname", addr: "localhost:18082"},
		{name: "missing port", addr: "127.0.0.1"},
		{name: "named port", addr: "127.0.0.1:http"},
		{name: "zero port", addr: "127.0.0.1:0"},
		{name: "large port", addr: "127.0.0.1:65536"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBenchmarkProfileAddr(tc.addr); (err == nil) != tc.ok {
				t.Fatalf("validateBenchmarkProfileAddr(%q) error = %v, want ok=%v", tc.addr, err, tc.ok)
			}
		})
	}
}

func TestBenchmarkProfileMux(t *testing.T) {
	for _, path := range []string{
		"/debug/torana/memstats?gc=1",
		"/debug/pprof/heap?gc=1",
		"/debug/pprof/allocs",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			benchmarkProfileMux().ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
			}
		})
	}

	recorder := httptest.NewRecorder()
	benchmarkProfileMux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug/torana/memstats?gc=2", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid gc status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	recorder = httptest.NewRecorder()
	benchmarkProfileMux().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/debug/torana/memstats", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestBenchmarkMemStatsShape(t *testing.T) {
	recorder := httptest.NewRecorder()
	benchmarkProfileMux().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug/torana/memstats", nil))
	var got benchmarkMemStats
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HeapSys == 0 || got.Mallocs == 0 {
		t.Fatalf("memstats were not populated: %+v", got)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("Cache-Control = %q", cacheControl)
	}
}
