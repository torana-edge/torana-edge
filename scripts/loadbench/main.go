// Command loadbench provides the dependency-free upstream and load generator
// used by scripts/benchmark-production.sh. It is intentionally outside the
// product binary: benchmark machinery must not enlarge Torana's release.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: loadbench upstream|load [flags]")
	}
	var err error
	switch os.Args[1] {
	case "metadata":
		err = runMetadata(os.Args[2:])
	case "upstream":
		err = runUpstream(os.Args[2:])
	case "load":
		err = runLoad(os.Args[2:])
	case "memory-summary":
		err = runMemorySummary(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func runMetadata(args []string) error {
	fs := flag.NewFlagSet("metadata", flag.ContinueOnError)
	revision := fs.String("revision", "unknown", "git revision under test")
	profile := fs.String("profile", "provider", "benchmark profile")
	firstByte := fs.String("upstream-first-byte", "100ms", "upstream first-byte delay")
	eventDelay := fs.String("upstream-event-delay", "10ms", "upstream SSE event delay")
	events := fs.Int("upstream-events", 100, "upstream SSE event count")
	responseBytes := fs.Int("upstream-response-bytes", 4096, "upstream response payload bytes")
	payloadBytes := fs.String("payload-bytes", "4096", "space-separated request payload sizes")
	nonstreamConcurrency := fs.String("nonstream-concurrency", "1 8 32", "space-separated non-streaming concurrency levels")
	streamConcurrency := fs.String("stream-concurrency", "1 8", "space-separated streaming concurrency levels")
	runStream := fs.Int("run-stream", 1, "whether the profile measures streaming rows")
	requestShape := fs.String("request-shape", "plain", "request body shape")
	duration := fs.String("duration", "10s", "measured duration per row")
	warmup := fs.String("warmup", "2s", "warmup duration per row")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runStream != 0 && *runStream != 1 {
		return errors.New("run-stream must be 0 or 1")
	}
	if err := validateRequestShape(*requestShape); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"kind": "metadata", "revision": *revision, "go_version": runtime.Version(),
		"goos": runtime.GOOS, "goarch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0),
		"num_cpu": runtime.NumCPU(), "profile": *profile,
		"upstream_first_byte": *firstByte, "upstream_event_delay": *eventDelay,
		"upstream_events": *events, "upstream_response_bytes": *responseBytes,
		"payload_bytes": *payloadBytes, "nonstream_concurrency": *nonstreamConcurrency,
		"stream_concurrency": *streamConcurrency, "run_stream": *runStream == 1,
		"request_shape": *requestShape, "duration": *duration, "warmup": *warmup,
	})
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "loadbench: "+format+"\n", args...)
	os.Exit(2)
}

func runUpstream(args []string) error {
	fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:18081", "listen address")
	firstByte := fs.Duration("first-byte", 100*time.Millisecond, "delay before the response")
	eventDelay := fs.Duration("event-delay", 10*time.Millisecond, "delay between SSE text events")
	events := fs.Int("events", 100, "text events per streaming response")
	responseBytes := fs.Int("response-bytes", 4096, "minimum non-streaming response bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *events <= 0 || *responseBytes < 0 || *firstByte < 0 || *eventDelay < 0 {
		return errors.New("events must be positive and sizes/durations non-negative")
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		streaming := bytes.Contains(body, []byte(`"stream":true`))
		time.Sleep(*firstByte)
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			for i := 0; i < *events; i++ {
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"bench\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-bench\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
				flusher.Flush()
				if *eventDelay > 0 {
					time.Sleep(*eventDelay)
				}
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"bench\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-bench\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			flusher.Flush()
			return
		}

		content := strings.Repeat("x", *responseBytes)
		response := map[string]any{
			"id": "bench", "object": "chat.completion", "created": 1, "model": "gpt-bench",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1024, "completion_tokens": 256, "total_tokens": 1280},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})

	srv := &http.Server{
		Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: *firstByte + time.Duration(*events)**eventDelay + 30*time.Second,
		IdleTimeout: 30 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Printf("upstream listening on %s\n", *listen)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type loadResult struct {
	Name              string  `json:"name"`
	Target            string  `json:"target"`
	Streaming         bool    `json:"streaming"`
	Concurrency       int     `json:"concurrency"`
	PayloadBytes      int     `json:"payload_bytes"`
	RequestShape      string  `json:"request_shape"`
	DurationSeconds   float64 `json:"duration_seconds"`
	Requests          int     `json:"requests"`
	Errors            int     `json:"errors"`
	RequestsPerSecond float64 `json:"requests_per_second"`
	P50Millis         float64 `json:"p50_ms"`
	P95Millis         float64 `json:"p95_ms"`
	P99Millis         float64 `json:"p99_ms"`
	MaxMillis         float64 `json:"max_ms"`
	SSEEvents         int64   `json:"sse_events,omitempty"`
	SSEEventsPerReq   float64 `json:"sse_events_per_request,omitempty"`
	RSSStartBytes     int64   `json:"rss_start_bytes,omitempty"`
	RSSPeakBytes      int64   `json:"rss_peak_bytes,omitempty"`
	ProcessCPUSeconds float64 `json:"process_cpu_seconds,omitempty"`
}

type memoryStatsSnapshot struct {
	Alloc        uint64  `json:"alloc_bytes"`
	TotalAlloc   uint64  `json:"total_alloc_bytes"`
	HeapAlloc    uint64  `json:"heap_alloc_bytes"`
	HeapSys      uint64  `json:"heap_sys_bytes"`
	HeapIdle     uint64  `json:"heap_idle_bytes"`
	HeapInuse    uint64  `json:"heap_inuse_bytes"`
	HeapReleased uint64  `json:"heap_released_bytes"`
	StackInuse   uint64  `json:"stack_inuse_bytes"`
	Mallocs      uint64  `json:"mallocs"`
	Frees        uint64  `json:"frees"`
	NumGC        uint32  `json:"num_gc"`
	PauseTotalNS uint64  `json:"pause_total_ns"`
	GCCPUFrac    float64 `json:"gc_cpu_fraction"`
}

type memorySummary struct {
	Kind                  string  `json:"kind"`
	Name                  string  `json:"name"`
	Requests              int     `json:"requests"`
	PayloadBytes          int     `json:"payload_bytes"`
	Concurrency           int     `json:"concurrency"`
	TotalAllocBytes       uint64  `json:"total_alloc_bytes"`
	AllocBytesPerRequest  float64 `json:"alloc_bytes_per_request"`
	Mallocs               uint64  `json:"mallocs"`
	MallocsPerRequest     float64 `json:"mallocs_per_request"`
	Frees                 uint64  `json:"frees"`
	GCs                   uint32  `json:"gcs"`
	GCPauseMillis         float64 `json:"gc_pause_ms"`
	GCCPUFractionBefore   float64 `json:"gc_cpu_fraction_before"`
	GCCPUFractionAfter    float64 `json:"gc_cpu_fraction_after"`
	HeapAllocBeforeBytes  uint64  `json:"heap_alloc_before_bytes"`
	HeapAllocAfterBytes   uint64  `json:"heap_alloc_after_bytes"`
	HeapAllocDeltaBytes   int64   `json:"heap_alloc_delta_bytes"`
	HeapInuseBeforeBytes  uint64  `json:"heap_inuse_before_bytes"`
	HeapInuseAfterBytes   uint64  `json:"heap_inuse_after_bytes"`
	HeapInuseDeltaBytes   int64   `json:"heap_inuse_delta_bytes"`
	HeapSysBeforeBytes    uint64  `json:"heap_sys_before_bytes"`
	HeapSysAfterBytes     uint64  `json:"heap_sys_after_bytes"`
	HeapReleasedAfter     uint64  `json:"heap_released_after_bytes"`
	StackInuseBeforeBytes uint64  `json:"stack_inuse_before_bytes"`
	StackInuseAfterBytes  uint64  `json:"stack_inuse_after_bytes"`
}

func runMemorySummary(args []string) error {
	fs := flag.NewFlagSet("memory-summary", flag.ContinueOnError)
	resultsPath := fs.String("results", "", "benchmark JSONL path")
	profileDir := fs.String("profile-dir", "", "directory containing before/after memstats")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *resultsPath == "" || *profileDir == "" {
		return errors.New("results and profile-dir are required")
	}
	results, err := os.Open(*resultsPath)
	if err != nil {
		return err
	}
	defer results.Close()
	return summarizeMemory(results, *profileDir, os.Stdout)
}

func summarizeMemory(results io.Reader, profileDir string, out io.Writer) error {
	scanner := bufio.NewScanner(results)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	encoder := json.NewEncoder(out)
	rows := 0
	for scanner.Scan() {
		var envelope struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			return fmt.Errorf("decode benchmark envelope: %w", err)
		}
		if envelope.Kind == "metadata" || !strings.HasPrefix(envelope.Name, "torana/") {
			continue
		}
		var result loadResult
		if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
			return fmt.Errorf("decode benchmark result: %w", err)
		}
		if result.Requests <= 0 {
			return fmt.Errorf("%s has no successful requests", result.Name)
		}
		base := profileDir + "/" + profileFileName(result.Name)
		before, err := readMemoryStats(base + ".before.memstats.json")
		if err != nil {
			return fmt.Errorf("%s before stats: %w", result.Name, err)
		}
		after, err := readMemoryStats(base + ".after.memstats.json")
		if err != nil {
			return fmt.Errorf("%s after stats: %w", result.Name, err)
		}
		if after.TotalAlloc < before.TotalAlloc || after.Mallocs < before.Mallocs || after.Frees < before.Frees || after.NumGC < before.NumGC || after.PauseTotalNS < before.PauseTotalNS {
			return fmt.Errorf("%s cumulative memory counters moved backwards", result.Name)
		}
		alloc := after.TotalAlloc - before.TotalAlloc
		mallocs := after.Mallocs - before.Mallocs
		summary := memorySummary{
			Kind: "memory_profile", Name: result.Name, Requests: result.Requests,
			PayloadBytes: result.PayloadBytes, Concurrency: result.Concurrency,
			TotalAllocBytes: alloc, AllocBytesPerRequest: float64(alloc) / float64(result.Requests),
			Mallocs: mallocs, MallocsPerRequest: float64(mallocs) / float64(result.Requests),
			Frees: after.Frees - before.Frees, GCs: after.NumGC - before.NumGC,
			GCPauseMillis:       float64(after.PauseTotalNS-before.PauseTotalNS) / float64(time.Millisecond),
			GCCPUFractionBefore: before.GCCPUFrac, GCCPUFractionAfter: after.GCCPUFrac,
			HeapAllocBeforeBytes: before.HeapAlloc, HeapAllocAfterBytes: after.HeapAlloc,
			HeapAllocDeltaBytes:  int64(after.HeapAlloc) - int64(before.HeapAlloc),
			HeapInuseBeforeBytes: before.HeapInuse, HeapInuseAfterBytes: after.HeapInuse,
			HeapInuseDeltaBytes: int64(after.HeapInuse) - int64(before.HeapInuse),
			HeapSysBeforeBytes:  before.HeapSys, HeapSysAfterBytes: after.HeapSys,
			HeapReleasedAfter:     after.HeapReleased,
			StackInuseBeforeBytes: before.StackInuse, StackInuseAfterBytes: after.StackInuse,
		}
		if err := encoder.Encode(summary); err != nil {
			return err
		}
		rows++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if rows == 0 {
		return errors.New("benchmark results contain no Torana rows")
	}
	return nil
}

func profileFileName(name string) string {
	return strings.NewReplacer("/", "_", "=", "_").Replace(name)
}

func readMemoryStats(path string) (memoryStatsSnapshot, error) {
	var stats memoryStatsSnapshot
	raw, err := os.ReadFile(path)
	if err != nil {
		return stats, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stats); err != nil {
		return stats, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return stats, errors.New("memstats contain trailing JSON")
	}
	if stats.TotalAlloc == 0 || stats.HeapSys == 0 || stats.Mallocs == 0 {
		return stats, errors.New("memstats are missing required runtime counters")
	}
	return stats, nil
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	name := fs.String("name", "unnamed", "scenario name")
	target := fs.String("target", "", "request URL")
	duration := fs.Duration("duration", 10*time.Second, "measured duration")
	warmup := fs.Duration("warmup", 2*time.Second, "warmup duration")
	concurrency := fs.Int("concurrency", 8, "worker count")
	streaming := fs.Bool("stream", false, "request and validate SSE")
	minEvents := fs.Int("min-sse-events", 0, "minimum data events required per successful stream")
	allowErrors := fs.Bool("allow-errors", false, "report request errors without failing the command")
	payloadBytes := fs.Int("payload-bytes", 4096, "user-message byte count")
	requestShape := fs.String("request-shape", "plain", "plain or agent conversation")
	rssPID := fs.Int("rss-pid", 0, "Linux process ID whose RSS should be sampled")
	clockTicks := fs.Int64("clock-ticks", 100, "Linux clock ticks per second (getconf CLK_TCK)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" || *duration <= 0 || *warmup < 0 || *concurrency <= 0 || *payloadBytes < 0 || *minEvents < 0 || *clockTicks <= 0 {
		return errors.New("target, positive duration/concurrency, and non-negative warmup/payload are required")
	}

	payload, err := requestPayload(*requestShape, *payloadBytes, *streaming)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        *concurrency * 2,
		MaxIdleConnsPerHost: *concurrency * 2,
		MaxConnsPerHost:     *concurrency,
	}, Timeout: 2 * time.Minute}
	defer client.CloseIdleConnections()

	if *warmup > 0 {
		warmLatencies, warmFailures, _, warmErr := loadPhase(client, *target, payload, *streaming, *concurrency, *warmup)
		if len(warmLatencies) == 0 || (warmFailures > 0 && !*allowErrors) {
			return fmt.Errorf("warmup: %d successes, %d failures: %w", len(warmLatencies), warmFailures, warmErr)
		}
	}
	rssStart, _ := processRSS(*rssPID)
	cpuStart, cpuStartErr := processCPUTicks(*rssPID)
	rssStop := make(chan struct{})
	rssPeak := make(chan int64, 1)
	go sampleRSS(*rssPID, rssStart, rssStop, rssPeak)
	started := time.Now()
	latencies, failures, eventCount, firstErr := loadPhase(client, *target, payload, *streaming, *concurrency, *duration)
	elapsed := time.Since(started)
	close(rssStop)
	peak := <-rssPeak
	cpuEnd, cpuEndErr := processCPUTicks(*rssPID)
	cpuSeconds := 0.0
	if cpuStartErr == nil && cpuEndErr == nil && cpuEnd >= cpuStart {
		cpuSeconds = float64(cpuEnd-cpuStart) / float64(*clockTicks)
	}
	if len(latencies) == 0 {
		return fmt.Errorf("no successful requests (%d failures): %w", failures, firstErr)
	}
	if *streaming && eventCount < int64(len(latencies)**minEvents) {
		return fmt.Errorf("stream event integrity: got %d events across %d requests, want at least %d per request", eventCount, len(latencies), *minEvents)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	result := loadResult{
		Name: *name, Target: *target, Streaming: *streaming, Concurrency: *concurrency,
		PayloadBytes: *payloadBytes, RequestShape: *requestShape,
		DurationSeconds: elapsed.Seconds(), Requests: len(latencies), Errors: failures,
		RequestsPerSecond: float64(len(latencies)) / elapsed.Seconds(),
		P50Millis:         millis(percentile(latencies, 0.50)), P95Millis: millis(percentile(latencies, 0.95)),
		P99Millis: millis(percentile(latencies, 0.99)), MaxMillis: millis(latencies[len(latencies)-1]),
		SSEEvents: eventCount, SSEEventsPerReq: float64(eventCount) / float64(len(latencies)),
		RSSStartBytes: rssStart, RSSPeakBytes: peak,
		ProcessCPUSeconds: cpuSeconds,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		return err
	}
	if failures > 0 && !*allowErrors {
		return fmt.Errorf("measured phase had %d request failures: %w", failures, firstErr)
	}
	return nil
}

func requestPayload(shape string, payloadBytes int, streaming bool) ([]byte, error) {
	if err := validateRequestShape(shape); err != nil {
		return nil, err
	}
	content := strings.Repeat("p", payloadBytes)
	var messages []any
	var tools []any
	switch shape {
	case "plain":
		messages = []any{map[string]any{"role": "user", "content": content}}
	case "agent":
		messages = []any{
			map[string]any{"role": "system", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "Inspect the parser failure."},
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_bench", "type": "function",
					"function": map[string]any{"name": "read_file", "arguments": `{"path":"internal/parser.go"}`},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_bench", "content": content},
			map[string]any{"role": "assistant", "content": "I inspected the tool output."},
			map[string]any{"role": "user", "content": "Continue with the fix."},
		}
		tools = []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file", "description": "Read one workspace file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":     map[string]any{"type": "string"},
						"metadata": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
					},
					"required": []string{"path"},
				},
			},
		}}
	}
	request := map[string]any{"model": "gpt-bench", "messages": messages, "stream": streaming}
	if len(tools) > 0 {
		request["tools"] = tools
	}
	return json.Marshal(request)
}

func validateRequestShape(shape string) error {
	switch shape {
	case "plain", "agent":
		return nil
	default:
		return fmt.Errorf("unknown request shape %q (want plain or agent)", shape)
	}
}

func loadPhase(client *http.Client, target string, payload []byte, streaming bool, concurrency int, duration time.Duration) ([]time.Duration, int, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	latencies := make([]time.Duration, 0, concurrency*64)
	failures := 0
	var events int64
	var firstErr error
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				started := time.Now()
				n, err := oneRequest(client, target, payload, streaming)
				elapsed := time.Since(started)
				mu.Lock()
				if err != nil {
					failures++
					if firstErr == nil {
						firstErr = err
					}
				} else {
					latencies = append(latencies, elapsed)
					events += int64(n)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return latencies, failures, events, firstErr
}

func oneRequest(client *http.Client, target string, payload []byte, streaming bool) (int, error) {
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	if !streaming {
		_, err = io.Copy(io.Discard, resp.Body)
		return 0, err
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	events := 0
	done := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			continue
		}
		if data != "" {
			events++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if !done {
		return 0, errors.New("stream ended without [DONE]")
	}
	return events, nil
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	index := int(math.Ceil(float64(len(sorted))*q)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func sampleRSS(pid int, initial int64, stop <-chan struct{}, result chan<- int64) {
	peak := initial
	if pid <= 0 {
		result <- 0
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			result <- peak
			return
		case <-ticker.C:
			if current, err := processRSS(pid); err == nil && current > peak {
				peak = current
			}
		}
	}
}

func processRSS(pid int) (int64, error) {
	if pid <= 0 {
		return 0, nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, fmt.Errorf("unexpected VmRSS line %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		return kb * 1024, err
	}
	return 0, errors.New("VmRSS absent")
}

func processCPUTicks(pid int) (int64, error) {
	if pid <= 0 {
		return 0, nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseProcessCPUTicks(string(data))
}

func parseProcessCPUTicks(stat string) (int64, error) {
	// comm is parenthesized and may itself contain spaces or ')'. Split after
	// the final close paren; fields[0] is then Linux proc field 3 (state), so
	// utime/stime fields 14/15 are indexes 11/12.
	closeParen := strings.LastIndex(stat, ")")
	if closeParen < 0 || closeParen+1 >= len(stat) {
		return 0, errors.New("malformed proc stat command")
	}
	fields := strings.Fields(stat[closeParen+1:])
	if len(fields) <= 12 {
		return 0, errors.New("malformed proc stat fields")
	}
	user, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	system, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return user + system, nil
}
