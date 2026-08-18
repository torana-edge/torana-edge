package wasm

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

const wasmPageBytes = 64 << 10

type guestLinearMemoryRecord struct {
	Guest              string   `json:"guest"`
	WASMBytes          int      `json:"wasm_bytes"`
	PoolSize           int      `json:"pool_size"`
	InitializedBytes   []uint32 `json:"initialized_bytes"`
	AfterOneCallBytes  []uint32 `json:"after_one_call_bytes"`
	InitializedTotal   uint64   `json:"initialized_total_bytes"`
	AfterOneCallTotal  uint64   `json:"after_one_call_total_bytes"`
	PerInstanceGrowth  []uint32 `json:"per_instance_growth_bytes"`
	RSSBaselineBytes   uint64   `json:"rss_baseline_bytes,omitempty"`
	RSSRuntimeBytes    uint64   `json:"rss_runtime_bytes,omitempty"`
	RSSLoadedBytes     uint64   `json:"rss_loaded_bytes,omitempty"`
	RSSAfterGrantBytes uint64   `json:"rss_after_grant_bytes,omitempty"`
	RSSPrewarmedBytes  uint64   `json:"rss_prewarmed_bytes,omitempty"`
	RSSFullPoolBytes   uint64   `json:"rss_full_pool_bytes,omitempty"`
	RSSAfterCallsBytes uint64   `json:"rss_after_calls_bytes,omitempty"`
}

type guestRepeatedMemoryRecord struct {
	Guest            string                        `json:"guest"`
	InitializedBytes []uint32                      `json:"initialized_bytes"`
	Checkpoints      []guestRepeatedMemorySnapshot `json:"checkpoints"`
}

type guestRepeatedMemorySnapshot struct {
	CallsPerInstance int      `json:"calls_per_instance"`
	LinearBytes      []uint32 `json:"linear_bytes"`
	RSSBytes         uint64   `json:"rss_bytes,omitempty"`
}

type guestHostMemoryRecord struct {
	Guest  string                    `json:"guest"`
	Stages []guestHostMemorySnapshot `json:"stages"`
}

type guestHostMemorySnapshot struct {
	Stage         string `json:"stage"`
	RSSBytes      uint64 `json:"rss_bytes,omitempty"`
	HeapAlloc     uint64 `json:"heap_alloc_bytes"`
	HeapInuse     uint64 `json:"heap_inuse_bytes"`
	HeapSys       uint64 `json:"heap_sys_bytes"`
	HeapReleased  uint64 `json:"heap_released_bytes"`
	StackInuse    uint64 `json:"stack_inuse_bytes"`
	OtherSys      uint64 `json:"other_sys_bytes"`
	GCSys         uint64 `json:"gc_sys_bytes"`
	TotalHostSys  uint64 `json:"total_host_sys_bytes"`
	LinearBytes   uint64 `json:"linear_bytes,omitempty"`
	InstanceCount int    `json:"instance_count,omitempty"`
}

type guestIdleRetirementRecord struct {
	Guest                 string   `json:"guest"`
	PoolSize              int      `json:"pool_size"`
	BeforeIdleLinearBytes []uint32 `json:"before_idle_linear_bytes"`
	AfterIdleLinearBytes  []uint32 `json:"after_idle_linear_bytes"`
	RSSBeforeIdleBytes    uint64   `json:"rss_before_idle_bytes,omitempty"`
	RSSAfterIdleBytes     uint64   `json:"rss_after_idle_bytes,omitempty"`
	RegrowDurationMicros  int64    `json:"regrow_duration_micros"`
}

// TestGuestLinearMemoryProfile measures the memory visible through the Wasm
// linear-memory contract, independently of process RSS. It is intentionally
// bundle-gated: the SDK builds the equivalent current-v2 Go and Rust logger
// guests, then passes their paths here. Each pool instance is measured after
// initialization and after exactly one real before-request hook call.
//
// Run with:
//
//	TORANA_GO_GUEST=/path/to/go-logger.wasm \
//	TORANA_RUST_GUEST=/path/to/rust-logger.wasm \
//	go test ./internal/wasm -run TestGuestLinearMemoryProfile -v
func TestGuestLinearMemoryProfile(t *testing.T) {
	guests := []struct {
		name string
		env  string
	}{
		{name: "go", env: "TORANA_GO_GUEST"},
		{name: "rust", env: "TORANA_RUST_GUEST"},
	}

	for _, guest := range guests {
		guest := guest
		t.Run(guest.name, func(t *testing.T) {
			baselineRSS := retainedRSSBytes(t)
			path := os.Getenv(guest.env)
			if path == "" {
				t.Skipf("%s unset", guest.env)
			}
			wasmBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			const poolSize = 4
			r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
				PoolSize:    poolSize,
				CallTimeout: 10 * time.Second,
			})
			defer r.Close()
			runtimeRSS := retainedRSSBytes(t)
			p, err := r.LoadPlugin(guest.name+"-logger", wasmBytes)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			loadedRSS := retainedRSSBytes(t)
			p.SetGrants([]string{"env.log"})
			afterGrantRSS := retainedRSSBytes(t)

			// SetGrants recycles the instance LoadPlugin used to validate the
			// guest because stdout/stderr wiring is an instantiation property.
			// Build the first retained instance before naming this stage prewarm.
			prewarmed := acquireInstances(t, p, 1)
			releaseInstances(p, prewarmed)
			prewarmedRSS := retainedRSSBytes(t)

			instances := acquireInstances(t, p, poolSize)
			defer func() { releaseInstances(p, instances) }()
			initialized := instanceMemoryBytes(t, instances)
			fullPoolRSS := retainedRSSBytes(t)

			input := benchmarkBeforeRequestInput(t)

			exerciseEachInstance(t, p, instances, input, true)

			after := instanceMemoryBytes(t, instances)
			afterCallsRSS := retainedRSSBytes(t)
			record := guestLinearMemoryRecord{
				Guest:              guest.name,
				WASMBytes:          len(wasmBytes),
				PoolSize:           poolSize,
				InitializedBytes:   initialized,
				AfterOneCallBytes:  after,
				PerInstanceGrowth:  make([]uint32, poolSize),
				RSSBaselineBytes:   baselineRSS,
				RSSRuntimeBytes:    runtimeRSS,
				RSSLoadedBytes:     loadedRSS,
				RSSAfterGrantBytes: afterGrantRSS,
				RSSPrewarmedBytes:  prewarmedRSS,
				RSSFullPoolBytes:   fullPoolRSS,
				RSSAfterCallsBytes: afterCallsRSS,
			}
			for i := range initialized {
				if after[i] < initialized[i] {
					t.Fatalf("instance %d memory shrank from %d to %d", i, initialized[i], after[i])
				}
				record.InitializedTotal += uint64(initialized[i])
				record.AfterOneCallTotal += uint64(after[i])
				record.PerInstanceGrowth[i] = after[i] - initialized[i]
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			t.Logf("WASM_LINEAR_MEMORY %s", encoded)
		})
	}
}

// TestGuestIdleRetirementProfile measures the production idle-retirement
// policy after one representative call per burst-created instance. It is
// bundle-gated because the standard-Go guest owns the measured footprint.
func TestGuestIdleRetirementProfile(t *testing.T) {
	path := os.Getenv("TORANA_GO_GUEST")
	if path == "" {
		t.Skip("TORANA_GO_GUEST unset")
	}
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const poolSize = 4
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:            poolSize,
		CallTimeout:         10 * time.Second,
		InstanceIdleTimeout: 50 * time.Millisecond,
	})
	defer r.Close()
	p, err := r.LoadPlugin("go-logger-idle-retirement", wasmBytes)
	if err != nil {
		t.Fatal(err)
	}
	p.SetGrants([]string{"env.log"})
	instances := acquireInstancesConcurrently(t, p, poolSize)
	exerciseEachInstance(t, p, instances, benchmarkBeforeRequestInput(t), true)
	before := instanceMemoryBytes(t, instances)
	rssBefore := retainedRSSBytes(t)
	releaseInstances(p, instances)

	deadline := time.Now().Add(2 * time.Second)
	for len(p.pool) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(p.pool); got != 1 {
		t.Fatalf("idle pool = %d, want one", got)
	}
	retained := acquireInstances(t, p, 1)
	after := instanceMemoryBytes(t, retained)
	releaseInstances(p, retained)
	rssAfter := retainedRSSBytes(t)

	regrowStart := time.Now()
	regrown := acquireInstancesConcurrently(t, p, poolSize)
	regrowDuration := time.Since(regrowStart)
	releaseInstances(p, regrown)
	record := guestIdleRetirementRecord{
		Guest:                 "go",
		PoolSize:              poolSize,
		BeforeIdleLinearBytes: before,
		AfterIdleLinearBytes:  after,
		RSSBeforeIdleBytes:    rssBefore,
		RSSAfterIdleBytes:     rssAfter,
		RegrowDurationMicros:  regrowDuration.Microseconds(),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("WASM_IDLE_RETIREMENT %s", encoded)
}

// TestGuestLinearMemoryRepeatedProfile distinguishes first-call heap growth
// from growth proportional to request count. Calls are serialized onto each
// exact instance; this is a retention probe, not a throughput benchmark.
func TestGuestLinearMemoryRepeatedProfile(t *testing.T) {
	guests := []struct {
		name string
		env  string
	}{
		{name: "go", env: "TORANA_GO_GUEST"},
		{name: "rust", env: "TORANA_RUST_GUEST"},
	}
	for _, guest := range guests {
		guest := guest
		t.Run(guest.name, func(t *testing.T) {
			path := os.Getenv(guest.env)
			if path == "" {
				t.Skipf("%s unset", guest.env)
			}
			wasmBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			const poolSize = 4
			r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
				PoolSize:    poolSize,
				CallTimeout: 10 * time.Second,
			})
			defer r.Close()
			p, err := r.LoadPlugin(guest.name+"-logger-repeat", wasmBytes)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			p.SetGrants([]string{"env.log"})
			instances := acquireInstances(t, p, poolSize)
			defer func() { releaseInstances(p, instances) }()
			initialized := instanceMemoryBytes(t, instances)
			input := benchmarkBeforeRequestInput(t)

			oldWriter := log.Writer()
			log.SetOutput(io.Discard)
			defer log.SetOutput(oldWriter)
			checkpoints := []int{1, 10, 100, 1000, 10_000}
			record := guestRepeatedMemoryRecord{Guest: guest.name, InitializedBytes: initialized}
			for calls := 1; calls <= checkpoints[len(checkpoints)-1]; calls++ {
				exerciseEachInstance(t, p, instances, input, true)
				if calls == checkpoints[len(record.Checkpoints)] {
					record.Checkpoints = append(record.Checkpoints, guestRepeatedMemorySnapshot{
						CallsPerInstance: calls,
						LinearBytes:      instanceMemoryBytes(t, instances),
						RSSBytes:         retainedRSSBytes(t),
					})
				}
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			t.Logf("WASM_REPEATED_MEMORY %s", encoded)
		})
	}
}

// TestGuestHostMemoryProfile separates Go-host heap classes from RSS and guest
// linear memory at each plugin lifecycle boundary. Run each guest subtest in a
// fresh process: the compiler cache and Go allocator are process-scoped.
func TestGuestHostMemoryProfile(t *testing.T) {
	guests := []struct {
		name string
		env  string
	}{
		{name: "go", env: "TORANA_GO_GUEST"},
		{name: "rust", env: "TORANA_RUST_GUEST"},
	}
	for _, guest := range guests {
		guest := guest
		t.Run(guest.name, func(t *testing.T) {
			path := os.Getenv(guest.env)
			if path == "" {
				t.Skipf("%s unset", guest.env)
			}
			wasmBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			record := guestHostMemoryRecord{Guest: guest.name}
			record.Stages = append(record.Stages, retainedHostMemorySnapshot(t, "baseline", nil))

			const poolSize = 4
			r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
				PoolSize:    poolSize,
				CallTimeout: 10 * time.Second,
			})
			defer r.Close()
			record.Stages = append(record.Stages, retainedHostMemorySnapshot(t, "runtime", nil))
			p, err := r.LoadPlugin(guest.name+"-logger-host-memory", wasmBytes)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			record.Stages = append(record.Stages, retainedHostMemorySnapshot(t, "loaded", nil))
			p.SetGrants([]string{"env.log"})
			record.Stages = append(record.Stages, retainedHostMemorySnapshot(t, "after_grant", nil))

			prewarmed := acquireInstances(t, p, 1)
			releaseInstances(p, prewarmed)
			record.Stages = append(record.Stages, retainedHostMemorySnapshot(t, "prewarmed", prewarmed))

			instances := acquireInstances(t, p, poolSize)
			defer func() { releaseInstances(p, instances) }()
			record.Stages = append(record.Stages, retainedHostMemorySnapshot(t, "full_pool", instances))
			exerciseEachInstance(t, p, instances, benchmarkBeforeRequestInput(t), true)
			record.Stages = append(record.Stages, retainedHostMemorySnapshot(t, "after_calls", instances))

			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			t.Logf("WASM_HOST_MEMORY %s", encoded)
		})
	}
}

// TestOfficialPluginLinearMemoryProfile applies the portable portion of the
// same probe to every official bundle. Process RSS is intentionally excluded:
// all bundles run in one test process, and wazero's compilation cache retains
// code across runtimes. Run each guest control above in a fresh process when
// process-level attribution is required.
func TestOfficialPluginLinearMemoryProfile(t *testing.T) {
	dir := officialBundlesDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read bundles directory: %v", err)
	}
	type manifest struct {
		Name        string `json:"name"`
		Permissions []struct {
			Name string `json:"name"`
		} `json:"permissions"`
	}
	type bundle struct {
		name        string
		manifest    manifest
		wasmBytes   []byte
		permissions []string
	}
	var bundles []bundle
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		bundleDir := filepath.Join(dir, entry.Name())
		manifestBytes, err := os.ReadFile(filepath.Join(bundleDir, "plugin.json"))
		if err != nil {
			continue
		}
		var m manifest
		if err := json.Unmarshal(manifestBytes, &m); err != nil {
			t.Fatalf("decode %s manifest: %v", entry.Name(), err)
		}
		wasmPath := filepath.Join(bundleDir, "plugin.wasm")
		wasmBytes, err := os.ReadFile(wasmPath)
		if err != nil {
			t.Fatalf("read %s: %v", wasmPath, err)
		}
		permissions := make([]string, len(m.Permissions))
		for i := range m.Permissions {
			permissions[i] = m.Permissions[i].Name
		}
		bundles = append(bundles, bundle{
			name:        entry.Name(),
			manifest:    m,
			wasmBytes:   wasmBytes,
			permissions: permissions,
		})
	}
	if len(bundles) == 0 {
		t.Fatal("no official plugin bundles found")
	}
	sort.Slice(bundles, func(i, j int) bool { return bundles[i].name < bundles[j].name })
	wantNames := []string{
		"auth",
		"cache_tier_selector",
		"cache_warmer",
		"compactor",
		"intent",
		"keyword_compactor",
		"otel",
		"pii",
		"schema_translator",
		"tool_governor",
	}
	gotNames := make([]string, len(bundles))
	for i := range bundles {
		gotNames[i] = bundles[i].name
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("official bundle inventory = %v, want %v", gotNames, wantNames)
	}

	for _, bundle := range bundles {
		bundle := bundle
		t.Run(bundle.name, func(t *testing.T) {
			if bundle.manifest.Name != bundle.name {
				t.Fatalf("manifest name = %q, directory = %q", bundle.manifest.Name, bundle.name)
			}
			const poolSize = 4
			r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
				PoolSize:    poolSize,
				CallTimeout: 10 * time.Second,
			})
			defer r.Close()
			p, err := r.LoadPlugin(bundle.name, bundle.wasmBytes)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			p.SetGrants(bundle.permissions)
			if !p.supports(pbv2.Hook_HOOK_BEFORE_REQUEST) {
				t.Fatal("official plugin has no before-request hook; memory call would be vacuous")
			}

			instances := acquireInstances(t, p, poolSize)
			defer func() { releaseInstances(p, instances) }()
			initialized := instanceMemoryBytes(t, instances)
			input := benchmarkBeforeRequestInput(t)
			exerciseEachInstance(t, p, instances, input, false)
			after := instanceMemoryBytes(t, instances)
			record := guestLinearMemoryRecord{
				Guest:             bundle.name,
				WASMBytes:         len(bundle.wasmBytes),
				PoolSize:          poolSize,
				InitializedBytes:  initialized,
				AfterOneCallBytes: after,
				PerInstanceGrowth: make([]uint32, poolSize),
			}
			for i := range initialized {
				if after[i] < initialized[i] {
					t.Fatalf("instance %d memory shrank from %d to %d", i, initialized[i], after[i])
				}
				record.InitializedTotal += uint64(initialized[i])
				record.AfterOneCallTotal += uint64(after[i])
				record.PerInstanceGrowth[i] = after[i] - initialized[i]
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			t.Logf("WASM_LINEAR_MEMORY %s", encoded)
		})
	}
}

func benchmarkBeforeRequestInput(t *testing.T) []byte {
	t.Helper()
	input, err := proto.Marshal(&pbv2.HookInput{
		RequestId: 1,
		Payload: &pbv2.HookInput_ChatRequest{ChatRequest: &pbv2.ChatRequest{
			Model: "benchmark-model",
			Messages: []*pbv2.Message{
				{Role: "system", Blocks: benchmarkTextBlocks("You are a coding assistant.")},
				{Role: "user", Blocks: benchmarkTextBlocks("Inspect the parser failure.")},
				{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{
					ToolUse: &pbv2.RequestToolUseBlock{
						Id: "call_bench", Name: "read_file", ArgumentsJson: []byte(`{"path":"internal/parser.go"}`),
					},
				}}}},
				{Role: "tool", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolResult{
					ToolResult: &pbv2.RequestToolResultBlock{
						ToolCallId: "call_bench",
						ToolName:   "read_file",
						Content: []*pbv2.ToolResultContentBlock{{Kind: &pbv2.ToolResultContentBlock_Text{
							Text: &pbv2.ToolResultTextBlock{Text: strings.Repeat("p", 16<<10)},
						}}},
					},
				}}}},
				{Role: "assistant", Blocks: benchmarkTextBlocks("I inspected the tool output.")},
				{Role: "user", Blocks: benchmarkTextBlocks("Continue with the fix.")},
			},
			Tools: []*pbv2.ToolDef{{
				Name:           "read_file",
				Description:    "Read one workspace file",
				ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"},"metadata":{"type":"object","additionalProperties":{"type":"string"}}},"required":["path"]}`),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal hook input: %v", err)
	}
	return input
}

func benchmarkTextBlocks(text string) []*pbv2.RequestBlock {
	return []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{
		Text: &pbv2.RequestTextBlock{Text: text},
	}}}
}

// retainedRSSBytes makes process-level attribution less sensitive to Go heap
// debris from guest construction. The probe remains informational: linear
// memory is the portable contract measurement, while /proc RSS is Linux-only
// and intentionally omitted on other hosts.
func retainedRSSBytes(t *testing.T) uint64 {
	t.Helper()
	if runtime.GOOS != "linux" {
		return 0
	}
	runtime.GC()
	debug.FreeOSMemory()
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		t.Fatalf("read process RSS: %v", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		t.Fatalf("parse process RSS: got %q", b)
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parse resident pages %q: %v", fields[1], err)
	}
	pageSize := os.Getpagesize()
	if pageSize <= 0 {
		t.Fatalf("invalid OS page size %d", pageSize)
	}
	return pages * uint64(pageSize)
}

func retainedHostMemorySnapshot(t *testing.T, stage string, instances []*pluginInstance) guestHostMemorySnapshot {
	t.Helper()
	rss := retainedRSSBytes(t)
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	snapshot := guestHostMemorySnapshot{
		Stage:         stage,
		RSSBytes:      rss,
		HeapAlloc:     stats.HeapAlloc,
		HeapInuse:     stats.HeapInuse,
		HeapSys:       stats.HeapSys,
		HeapReleased:  stats.HeapReleased,
		StackInuse:    stats.StackInuse,
		OtherSys:      stats.OtherSys,
		GCSys:         stats.GCSys,
		TotalHostSys:  stats.Sys,
		InstanceCount: len(instances),
	}
	for _, inst := range instances {
		if inst != nil && inst.mod != nil && inst.mod.Memory() != nil {
			snapshot.LinearBytes += uint64(inst.mod.Memory().Size())
		}
	}
	return snapshot
}

func acquireInstances(t *testing.T, p *Plugin, count int) []*pluginInstance {
	t.Helper()
	instances := make([]*pluginInstance, 0, count)
	for range count {
		inst, err := p.acquire(context.Background())
		if err != nil {
			releaseInstances(p, instances)
			t.Fatalf("acquire instance %d: %v", len(instances), err)
		}
		instances = append(instances, inst)
	}
	return instances
}

func acquireInstancesConcurrently(t *testing.T, p *Plugin, count int) []*pluginInstance {
	t.Helper()
	instances := make([]*pluginInstance, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			instances[i], errs[i] = p.acquire(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			releaseInstances(p, instances)
			t.Fatalf("acquire instance %d: %v", i, err)
		}
	}
	return instances
}

// exerciseEachInstance keeps every other slot occupied while invoking one
// released instance. This makes the call target deterministic without adding
// a production inspection or dispatch API solely for the benchmark.
func exerciseEachInstance(t *testing.T, p *Plugin, instances []*pluginInstance, input []byte, requirePass bool) {
	t.Helper()
	for i := range instances {
		inst := instances[i]
		instances[i] = nil
		p.release(inst)
		var output []byte
		if err := p.CallRequest(context.Background(), pbv2.Hook_HOOK_BEFORE_REQUEST, 1, input, &output); err != nil {
			t.Fatalf("instance %d call: %v", i, err)
		}
		if requirePass && len(output) != 0 {
			t.Fatalf("instance %d returned %d bytes, want pass-through", i, len(output))
		}
		instances[i] = acquireInstances(t, p, 1)[0]
	}
}

func releaseInstances(p *Plugin, instances []*pluginInstance) {
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		p.release(inst)
	}
}

func instanceMemoryBytes(t *testing.T, instances []*pluginInstance) []uint32 {
	t.Helper()
	sizes := make([]uint32, len(instances))
	for i, inst := range instances {
		if inst == nil || inst.mod == nil || inst.mod.Memory() == nil {
			t.Fatalf("instance %d has no memory", i)
		}
		size := inst.mod.Memory().Size()
		if size%wasmPageBytes != 0 {
			t.Fatalf("instance %d memory = %d bytes, not page aligned", i, size)
		}
		sizes[i] = size
	}
	return sizes
}
