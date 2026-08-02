package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"

	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// TestE2E drives the full production path with REAL built WASM plugins:
// HTTP client → proxy → plugin pipeline → mock upstream → plugin pipeline →
// HTTP client. This is the coverage class whose absence let the stale-stub
// regression ship: every assertion here exercises the deployed .wasm files.
func TestE2E(t *testing.T) {
	bundles := officialBundlesDir(t)
	for _, name := range []string{"schema_translator", "intent", "compactor"} {
		requireBundle(t, bundles, name)
	}

	// --- mock upstream ------------------------------------------------------
	var mu sync.Mutex
	var lastToolsBody []byte      // last upstream request that carried tools
	var lastToolResultBody []byte // last upstream request carrying a tool result

	writeSSE := func(w http.ResponseWriter, lines []string) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
			if fl != nil {
				fl.Flush()
			}
		}
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// Anthropic-format streaming fixture.
		if strings.HasPrefix(r.URL.Path, "/anthropic") {
			mu.Lock()
			lastToolsBody = body
			mu.Unlock()
			writeSSE(w, []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_e2e","name":"write"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"i\":\"check config\","}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"env\":[{\"key\":\"A\",\"value\":\"1\"}]}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`,
			})
			return
		}

		// OpenAI format.
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		json.Unmarshal(body, &req)

		if len(req.Tools) > 0 {
			mu.Lock()
			lastToolsBody = body
			mu.Unlock()
		}
		for _, m := range req.Messages {
			// Ignore the intent plugin's injected few-shot example (a
			// synthetic tool-result message present in every translated
			// request).
			if m.Role == "tool" && m.ToolCallID != "call_mock_fewshot_1" {
				mu.Lock()
				lastToolResultBody = body
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
				return
			}
		}

		if req.Stream {
			// Echo the request model into the tool args so concurrent
			// clients can verify their streams aren't cross-contaminated.
			frag1 := `{\"i\":\"find the port\",\"env\":[{\"key\":\"K\",`
			frag2 := fmt.Sprintf(`\"value\":\"%s\"}]}`, req.Model)
			writeSSE(w, []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_e2e","type":"function","function":{"name":"write","arguments":""}}]}}]}`,
				fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"%s"}}]}}]}`, frag1),
				fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"%s"}}]}}]}`, frag2),
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			})
			return
		}

		// Non-streaming: tool call carrying "i" — intent gets cached for the
		// offload turn.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "chatcmpl-e2e", "model": "mock-1",
			"choices": [{"finish_reason": "tool_calls", "message": {
				"role": "assistant",
				"tool_calls": [{"id": "call_off_1", "type": "function", "function": {"name": "search", "arguments": "{\"i\":\"find the answer\",\"query\":\"x\"}"}}]
			}}],
			"usage": {"prompt_tokens": 9, "completion_tokens": 4}
		}`))
	}))
	defer upstream.Close()

	// --- mock offload provider ---------------------------------------------
	offload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-e2e" {
			t.Errorf("offload Authorization: got %q want Bearer sk-e2e", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"choices":[{"message":{"content":"summary of the answer"}}],
			"usage":{"prompt_tokens":20,"completion_tokens":5}
		}`))
	}))
	defer offload.Close()

	// --- proxy with real plugins ---------------------------------------------
	cacheRate := 1.0
	freeRate := 0.0
	cfg := Config{
		Port: "0",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{
				"oai": {
					URL: upstream.URL, Format: "openai",
					Pricing: map[string]economics.ModelPricing{
						"gpt-x": {CacheReadUSDPerMTok: &cacheRate, CacheWriteUSDPerMTok: &cacheRate},
					},
				},
				"anth": {URL: upstream.URL + "/anthropic", Format: "anthropic"},
				"cheap": {
					URL: offload.URL, Format: "openai",
					Pricing: map[string]economics.ModelPricing{
						"cheap-1": {InputUSDPerMTok: &freeRate, OutputUSDPerMTok: &freeRate},
					},
				},
			},
			Plugins: provider.PluginsConfig{
				Dir:             bundles,
				AllowUnapproved: true,
				// intent captures "i" into the cache; compactor consumes it.
				// keyword_compactor is its ALTERNATIVE (either/or) and is
				// deliberately not in this pipeline.
				Order: []string{"schema_translator", "intent", "compactor"},
				Config: map[string]json.RawMessage{
					"compactor": json.RawMessage(`{
						"expected_applications": 6,
						"tool_policies": [{"match": "search", "mode": "model"}]
					}`),
				},
			},
			Offload: provider.OffloadConfig{
				Enabled:  true,
				Provider: "cheap",
				Model:    "cheap-1",
			},
		},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 30 * time.Second}

	post := func(t *testing.T, path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-e2e")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, b)
		}
		return resp
	}

	openaiToolsReq := func(model string, stream bool) string {
		return fmt.Sprintf(`{
			"model": %q, "stream": %v,
			"messages": [{"role": "user", "content": "set the env"}],
			"tools": [{"type": "function", "function": {"name": "write", "parameters": {
				"type": "object",
				"properties": {"env": {"type": "object", "additionalProperties": {"type": "string"}}}
			}}}]
		}`, model, stream)
	}

	// assembleToolArgs reparses a proxied stream and returns the reassembled
	// tool-call argument payloads (one string per delta event).
	assembleToolArgs := func(t *testing.T, formatName string, body io.Reader) []string {
		t.Helper()
		var deltas []string
		for ev := range format.Lookup(formatName).Stream.ParseStream(body) {
			if ev.ToolCallDelta != nil {
				deltas = append(deltas, ev.ToolCallDelta.ArgumentsDelta)
			}
			if ev.Error != nil {
				t.Fatalf("stream error: %+v", ev.Error)
			}
		}
		return deltas
	}

	assertReversedArgs := func(t *testing.T, deltas []string, wantKey, wantVal string) {
		t.Helper()
		if len(deltas) != 1 {
			t.Fatalf("expected exactly 1 complete args delta, got %d: %v", len(deltas), deltas)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(deltas[0]), &args); err != nil {
			t.Fatalf("args not valid JSON: %v (%q)", err, deltas[0])
		}
		if _, hasI := args["i"]; hasI {
			t.Errorf(`"i" not stripped: %v`, args)
		}
		env, ok := args["env"].(map[string]any)
		if !ok || env[wantKey] != wantVal {
			t.Errorf("expected env.%s=%s (KV array reversed), got %v", wantKey, wantVal, args)
		}
	}

	t.Run("StreamingOpenAI", func(t *testing.T) {
		resp := post(t, "/provider/oai/v1/chat/completions", openaiToolsReq("gpt-x", true))
		defer resp.Body.Close()
		assertReversedArgs(t, assembleToolArgs(t, "openai", resp.Body), "K", "gpt-x")

		// The upstream must have received the TRANSLATED request: env as a
		// KV array and the injected required "i" field.
		mu.Lock()
		tools := string(lastToolsBody)
		mu.Unlock()
		if !strings.Contains(tools, `"i"`) {
			t.Errorf(`upstream request missing injected "i": %s`, tools)
		}
		if !strings.Contains(tools, `"array"`) {
			t.Errorf("upstream request env not converted to KV array: %s", tools)
		}
	})

	t.Run("StreamingAnthropic", func(t *testing.T) {
		body := `{
			"model": "claude-x", "max_tokens": 128, "stream": true,
			"messages": [{"role": "user", "content": "set the env"}],
			"tools": [{"name": "write", "description": "w", "input_schema": {
				"type": "object",
				"properties": {"env": {"type": "object", "additionalProperties": {"type": "string"}}}
			}}]
		}`
		resp := post(t, "/provider/anth/v1/messages", body)
		defer resp.Body.Close()
		assertReversedArgs(t, assembleToolArgs(t, "anthropic", resp.Body), "A", "1")
	})

	t.Run("OffloadFlow", func(t *testing.T) {
		// Turn 1 (non-streaming JSON path): tool call comes back with "i"
		// stripped, and the intent is cached under call_off_1.
		resp := post(t, "/provider/oai/v1/chat/completions", `{
			"model": "gpt-x",
			"messages": [{"role": "user", "content": "find it"}],
			"tools": [{"type": "function", "function": {"name": "search", "parameters": {
				"type": "object", "properties": {"query": {"type": "string"}}
			}}}]
		}`)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), `\"i\"`) || strings.Contains(string(b), `"i":`) {
			t.Errorf(`turn 1 response still contains "i": %s`, b)
		}
		if !strings.Contains(string(b), "chatcmpl-e2e") || !strings.Contains(string(b), "prompt_tokens") {
			t.Errorf("turn 1 response lost sibling fields: %s", b)
		}

		// Turn 2: send a huge fresh tool result. The model requested this
		// evidence and has not consumed it yet, so #166 requires that it reach
		// the upstream verbatim rather than being compacted on first exposure.
		bigResult := strings.Repeat("zzzz zzz zz\n", 300) // >2000 chars, no intent keywords
		turn2 := fmt.Sprintf(`{
			"model": "gpt-x",
			"messages": [
				{"role": "user", "content": "find it"},
				{"role": "assistant", "tool_calls": [{"id": "call_off_1", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"x\"}"}}]},
				{"role": "tool", "tool_call_id": "call_off_1", "content": %q}
			]
		}`, bigResult)
		resp2 := post(t, "/provider/oai/v1/chat/completions", turn2)
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()

		mu.Lock()
		upstreamSaw := string(lastToolResultBody)
		mu.Unlock()
		if !strings.Contains(upstreamSaw, "zzzz zzz zz") {
			t.Errorf("upstream did not receive the fresh raw tool result")
		}
		if strings.Contains(upstreamSaw, "summary of the answer") {
			t.Errorf("fresh tool result was compacted before first consumption")
		}

		// Turn 3: replay the now-consumed result with the assistant response
		// from turn 2 followed by a new user request. The result is historical,
		// so the compactor may offload it before it reaches the upstream.
		turn3 := fmt.Sprintf(`{
			"model": "gpt-x",
			"messages": [
				{"role": "user", "content": "find it"},
				{"role": "assistant", "tool_calls": [{"id": "call_off_1", "type": "function", "function": {"name": "search", "arguments": "{\"query\":\"x\"}"}}]},
				{"role": "tool", "tool_call_id": "call_off_1", "content": %q},
				{"role": "assistant", "content": "done"},
				{"role": "user", "content": "continue"}
			]
		}`, bigResult)
		resp3 := post(t, "/provider/oai/v1/chat/completions", turn3)
		io.Copy(io.Discard, resp3.Body)
		resp3.Body.Close()

		mu.Lock()
		upstreamSaw = string(lastToolResultBody)
		mu.Unlock()
		if !strings.Contains(upstreamSaw, "summary of the answer") {
			t.Errorf("upstream did not receive the offloaded summary; tool result was: %.200s", upstreamSaw)
		}
		if strings.Contains(upstreamSaw, "zzzz zzz zz") {
			t.Errorf("upstream still received the raw huge tool result")
		}

		// /stats must report the savings.
		statsResp, err := client.Get(base + "/stats")
		if err != nil {
			t.Fatalf("GET /stats: %v", err)
		}
		defer statsResp.Body.Close()
		var stats struct {
			Compactions int64 `json:"compactions"`
			BytesSaved  int64 `json:"bytes_saved"`
		}
		if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
			t.Fatalf("decode /stats: %v", err)
		}
		if stats.Compactions < 1 || stats.BytesSaved <= 0 {
			t.Errorf("stats did not record savings: %+v", stats)
		}
	})

	t.Run("StreamingReleasesRateLimitTokens", func(t *testing.T) {
		// Regression caught during live dogfooding: the SSE path replaced
		// resp.Body with the serializer pipe, so the upstream body's Close
		// (which releases the concurrency token) never ran — after
		// limits.concurrency streamed requests, every caller got 429.
		limCfg := cfg
		limCfg.Providers.Limits = provider.Limits{Concurrency: 2}
		limSrv, err := New(limCfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		limLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go limSrv.Serve(limLn)
		defer limSrv.Shutdown(context.Background())
		limBase := "http://" + limLn.Addr().String()

		// Well beyond the concurrency cap — every sequential request must
		// succeed because each stream releases its token on completion.
		for i := 0; i < 6; i++ {
			req, _ := http.NewRequest("POST", limBase+"/provider/oai/v1/chat/completions", strings.NewReader(openaiToolsReq("gpt-x", true)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("request %d: status %d — streaming leaked rate-limit tokens", i, resp.StatusCode)
			}
		}
	})

	t.Run("ConcurrentStreams", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				model := fmt.Sprintf("m-%d", i)
				req, _ := http.NewRequest("POST", base+"/provider/oai/v1/chat/completions", strings.NewReader(openaiToolsReq(model, true)))
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					t.Errorf("client %d: %v", i, err)
					return
				}
				defer resp.Body.Close()

				var deltas []string
				for ev := range format.Lookup("openai").Stream.ParseStream(resp.Body) {
					if ev.ToolCallDelta != nil {
						deltas = append(deltas, ev.ToolCallDelta.ArgumentsDelta)
					}
				}
				if len(deltas) != 1 {
					t.Errorf("client %d: expected 1 args delta, got %d: %v", i, len(deltas), deltas)
					return
				}
				var args map[string]any
				if err := json.Unmarshal([]byte(deltas[0]), &args); err != nil {
					t.Errorf("client %d: invalid args %q", i, deltas[0])
					return
				}
				env, _ := args["env"].(map[string]any)
				if env == nil || env["K"] != model {
					t.Errorf("client %d: cross-request contamination — want env.K=%s got %v", i, model, args)
				}
			}(i)
		}
		wg.Wait()
	})
}

// TestHotReloadDuringInflightRequest reproduces the review finding on #140:
// a pipeline swap while a request is streaming must not drain-and-close the
// runtime holding that request's state. The request pins its pipeline for
// its whole lifetime, so the old runtime's meta (fragment buffers, mutation
// registry) stays alive until the response completes.
func TestHotReloadDuringInflightRequest(t *testing.T) {
	// Host mechanics, not plugin behaviour: what is under test is that a
	// pipeline swap mid-stream does not drain the runtime holding an in-flight
	// request's state. Gating it on a real plugin took the assertion out of
	// this repository's suite entirely.
	//
	// test-fragment-buffer accumulates tool-call argument fragments in
	// request-scoped host state and emits them verbatim once complete. If the
	// swap drops that state the arguments never arrive, which is observable
	// from outside without knowing what any real plugin would have done to
	// them.
	requireWASM(t, fixturesDir+"/test-fragment-buffer/plugin.wasm")
	reloadBundles := fixturesDir

	release := make(chan struct{})
	reached := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_hr","type":"function","function":{"name":"write","arguments":""}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"env\":[{\"key\":\"A\","}}]}}]}`+"\n\n")
		fl.Flush()
		close(reached) // fragment is in flight, buffered in the plugin
		<-release      // hold the stream open while the test swaps pipelines
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"value\":\"1\"}]}"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	cfg := Config{
		Port: "0",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
			Plugins:   provider.PluginsConfig{Dir: reloadBundles, Order: []string{"test-fragment-buffer"}, AllowUnapproved: true},
		},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	// Fire the streaming request.
	type result struct {
		args []string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(
			"http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
			"application/json",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"write","parameters":{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}}}]}`))
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		var deltas []string
		for ev := range format.Lookup("openai").Stream.ParseStream(resp.Body) {
			if ev.ToolCallDelta != nil {
				deltas = append(deltas, ev.ToolCallDelta.ArgumentsDelta)
			}
		}
		resCh <- result{args: deltas}
	}()

	<-reached // request is mid-stream with a buffered fragment

	// Simulate the watcher: swap in a fresh pipeline and drain the old one.
	newRT := wasm.NewRuntime(context.Background())
	newPP, err := plugin.NewPipeline(newRT, plugin.PluginConfig{Dir: reloadBundles, Order: []string{"test-fragment-buffer"}, AllowUnapproved: true})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	old := srv.pluginPipeline.Swap(newPP).(*plugin.PluginPipeline)
	drained := make(chan struct{})
	go func() {
		old.DrainAndClose()
		close(drained)
	}()

	// The drain must NOT complete while the request is still in flight.
	select {
	case <-drained:
		t.Fatal("old pipeline drained while a request was still using it")
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // let the stream finish

	res := <-resCh
	if res.err != nil {
		t.Fatalf("request: %v", res.err)
	}
	if len(res.args) != 1 {
		t.Fatalf("expected 1 complete args delta, got %v", res.args)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(res.args[0]), &args); err != nil {
		t.Fatalf("args invalid: %v (%q)", err, res.args[0])
	}
	// The fragments spanned the pipeline swap. Their reassembled form arriving
	// intact is the whole assertion: had the swap drained the runtime holding
	// this request's buffer, the accumulated prefix would be gone and these
	// arguments would never have completed.
	env, _ := args["env"].([]any)
	if len(env) != 1 {
		t.Fatalf("state lost across hot reload — args: %v", args)
	}
	pair, _ := env[0].(map[string]any)
	if pair["key"] != "A" || pair["value"] != "1" {
		t.Fatalf("fragments reassembled incorrectly across the reload: %v", args)
	}

	// And after the request completes, the drain must finish promptly.
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("old pipeline never drained after request completion")
	}
}

// failure_mode: block must be enforced where the response is actually
// produced, not only inside the pipeline.
//
// The pipeline returned an error for a trapping block-mode plugin and both
// call sites logged it and carried on: the request went upstream, the caller
// got a normal completion, and only a unit-level pipeline test would have said
// "blocked". A security plugin whose manifest says block was fail-open on the
// real HTTP path.
//
// End-to-end on purpose. The bug lived in the gap between the pipeline and the
// transport, which is exactly the seam a pipeline-level test cannot see.
func TestFailureModeBlockRefusesAtTheTransport(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-trapper/plugin.wasm")

	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"leaked"}}]}`)
	}))
	defer upstream.Close()

	cfg := Config{
		Port: "0",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
			Plugins: provider.PluginsConfig{
				Dir: fixturesDir, Order: []string{"test-trapper"}, AllowUnapproved: true,
			},
		},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	body := `{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`
	// The /provider/<name>/ prefix is what routes to a configured provider. An
	// earlier version of this test posted to a bare /v1/chat/completions, got
	// "no provider configured for this path", and passed for that reason
	// instead of the block — it was green with the fix reverted.
	url := "http://" + ln.Addr().String() + "/provider/oai/v1/chat/completions"
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(got), "no provider configured") {
		t.Fatalf("the request never reached the pipeline (%s); this test would pass "+
			"for the wrong reason", got)
	}
	if n := atomic.LoadInt32(&upstreamCalls); n != 0 {
		t.Errorf("upstream was called %d times; a block-mode plugin trapped, so the "+
			"request must never have been sent", n)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("caller got 200 after a block-mode plugin trapped; body=%s", got)
	}
	if strings.Contains(string(got), "leaked") {
		t.Fatalf("the upstream completion reached the caller despite the block: %s", got)
	}
}

// failure_mode: block on the NON-STREAMING response path.
//
// The body has not been written yet — ModifyResponse still owns it — so this
// is refusable, and forwarding the provider's body after a plugin refused it
// is the same fail-open as sending the request upstream after a refusal.
// A different transport path from the request hook, and it was independently
// wrong.
func TestFailureModeBlockRefusesTheNonStreamingResponse(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-trapper-response/plugin.wasm")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"leaked"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-trapper-response"}, AllowUnapproved: true,
		},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	resp, err := http.Post("http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(got), "no provider configured") {
		t.Fatalf("the request never reached the pipeline (%s)", got)
	}
	if strings.Contains(string(got), "leaked") {
		t.Fatalf("the provider body reached the caller after a block-mode response "+
			"hook trapped: %s", got)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("caller got 200 after a block-mode response hook trapped: %s", got)
	}
}

// failure_mode: block on a stream whose headers and body have already begun.
//
// Nothing can be refused any more, so the honest action is to TERMINATE.
// Replaying the refused event was the fail-open — it delivers exactly the
// content the block policy exists to withhold.
func TestFailureModeBlockTerminatesTheStream(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-trapper-stream/plugin.wasm")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"leaked"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-trapper-stream"}, AllowUnapproved: true,
		},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	resp, err := http.Post("http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	got, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()

	if strings.Contains(string(got), "no provider configured") {
		t.Fatalf("the request never reached the pipeline (%s)", got)
	}
	if strings.Contains(string(got), "leaked") {
		t.Fatalf("the refused event was replayed to the caller: %s", got)
	}
	if readErr == nil || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("block-mode trap completed cleanly: err=%v body=%q", readErr, got)
	}
	if strings.Contains(string(got), "[DONE]") {
		t.Fatalf("block-mode trap emitted an OpenAI completion marker: %s", got)
	}
}

// A terminal plugin error must close the actual upstream response, not merely
// the downstream serializer channel. Keeping this handler open after the
// trapped event reproduces the parser→tap back-pressure shape: without the
// cancellation/drain path, the upstream request remains live after the client
// has already seen its truncated response.
func TestFailureModeBlockCancelsAndDrainsUpstream(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-trapper-stream/plugin.wasm")

	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"leaked"}}]}`+"\n\n")
		fl.Flush()
		// This second event can be waiting behind the tap when the first one
		// triggers the terminal. The handler must still observe a canceled
		// request rather than remaining blocked indefinitely.
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"later"}}]}`+"\n\n")
		fl.Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
		Plugins:   provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-trapper-stream"}, AllowUnapproved: true},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr == nil || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("block-mode trap completed cleanly: %v", readErr)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal stream did not close the upstream parse/tap chain")
	}
}

// The OpenAI Responses serializer has a distinct completion frame from Chat
// Completions. A block-mode trap must cancel it before response.completed (or
// [DONE]) is written; closing the pipe after the serializer returns is too
// late because clients treat either frame as success.
func TestFailureModeBlockTerminatesResponsesStreamWithoutCompletion(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-trapper-stream/plugin.wasm")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"leaked"}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
		Plugins:   provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-trapper-stream"}, AllowUnapproved: true},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	resp, err := http.Post("http://"+ln.Addr().String()+"/provider/oai/v1/responses",
		"application/json", strings.NewReader(`{"model":"gpt-x","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr == nil || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("Responses trap completed cleanly: err=%v body=%q", readErr, body)
	}
	for _, completion := range []string{"response.completed", "[DONE]", "leaked"} {
		if strings.Contains(string(body), completion) {
			t.Fatalf("Responses trap emitted %q: %s", completion, body)
		}
	}
}

// The streaming observational hook must finish before the handler records the
// feed and drops request state.
//
// The streaming goroutine closes the pipe BEFORE running run_after_response, so
// EOF lets ReverseProxy.ServeHTTP return while the hook is still executing. The
// handler then recorded the feed and ran EndRequest concurrently: plugin_failure
// was nondeterministically absent, request-scoped state could be deleted out
// from under the hook, and reqState was read and written by two goroutines.
//
// Asserted on the emitted feed event rather than a log line, because the field
// is the operator-visible artifact. Run this package under -race to catch the
// third symptom.
func TestStreamingObservationalHookCompletesBeforeFeedRecording(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-trapper-after-stream/plugin.wasm")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-trapper-after-stream"}, AllowUnapproved: true,
		},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	resp, err := http.Post("http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if strings.Contains(string(body), "no provider configured") {
		t.Fatalf("the request never reached the pipeline (%s)", body)
	}
	// The stream itself must still succeed: the hook is observational.
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("the streamed body did not reach the caller: %s", body)
	}

	// No sleep and no retry. If the handler no longer waits for the hook, the
	// event is recorded before PluginFailure is set and this fails every time.
	events := srv.feed.Snapshot()
	if len(events) == 0 {
		t.Fatal("no feed event was recorded")
	}
	last := events[len(events)-1]
	if !last.PluginFailure {
		t.Fatalf("plugin_failure is absent from the feed event (%+v) — the handler "+
			"recorded it before the observational hook finished", last)
	}
	if last.Verdict == "block" {
		t.Error("an observational failure was reported as a block; nothing was withheld")
	}
}

// The observational streaming hook IS on the client's critical path, and this
// pins that as a recorded decision.
//
// Closing the pipe does not give the client EOF: Go's HTTP server writes the
// chunked terminator when the HANDLER returns, and the handler waits for this
// hook. An earlier comment in server.go claimed the opposite — this test exists
// so the real behaviour cannot drift back into a comfortable assumption.
//
// "Observational" describes the hook's AUTHORITY (it cannot change the
// response), not its timing.
//
// The hook's duration is a HOST-CONTROLLED LATCH, not a timer: the fixture's
// after-response hook issues a granted torana_send_request to a provider whose
// httptest handler blocks on a Go channel the test owns. The test proves EOF
// is held while the hook is gated and completes only after release, with the
// stream body intact. If observational post-processing is ever moved off the
// response path, EOF completes before entered and this test FAILS — that is
// the point of pinning it. The 30s context is deadlock protection only, never
// the assertion.
func TestObservationalStreamingHookIsOnTheClientCriticalPath(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-slow-after-stream/plugin.wasm")

	enteredCh := make(chan struct{})
	releaseCh := make(chan struct{})
	// entered/release are idempotent (sync.Once) and release happens on EVERY
	// exit path, so a premature failure can never leave the latch handler
	// blocked and hang httptest.Server.Close in the deferred cleanup.
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	latch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(enteredCh) })
		select {
		case <-releaseCh:
		case <-r.Context().Done():
			return // cancellation-aware: never block server shutdown forever
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"released"}}]}`)
	}))
	// One combined cleanup: release BEFORE shutdown, on every exit path.
	// sync.Once makes the explicit failure-branch releases idempotent.
	defer func() { release(); latch.Close() }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{
			"oai":   {URL: upstream.URL, Format: "openai"},
			"latch": {URL: latch.URL, Format: "openai"},
		},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-slow-after-stream"},
			AllowUnapproved: true,
			Runtime: provider.PluginRuntimeConfig{
				Egress: map[string]provider.EgressBudget{
					"test-slow-after-stream": {MaxCallsPerMinute: 10},
				},
			},
		},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	addr := ln.Addr().String()

	// Deadlock protection only.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"http://"+addr+"/provider/oai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	eof := make(chan struct{})
	var body []byte
	var readErr error
	go func() {
		body, readErr = io.ReadAll(resp.Body) // reads to EOF, i.e. handler return
		resp.Body.Close()
		close(eof)
	}()

	select {
	case <-enteredCh:
	case <-ctx.Done():
		release()
		t.Fatal("the after-response hook never entered the egress latch")
	}

	// While the hook is gated, the client must NOT have EOF: the hook is on
	// the critical path by design. A non-blocking check, not a timing claim.
	select {
	case <-eof:
		release()
		t.Fatal("client EOF completed while the observational hook was still gated — " +
			"if post-processing was made asynchronous, invert this test and update " +
			"reqState.streamDone's contract")
	default:
	}

	release()

	select {
	case <-eof:
	case <-ctx.Done():
		t.Fatal("client EOF did not complete after the latch released")
	}
	if readErr != nil {
		t.Fatalf("reading the stream body: %v", readErr)
	}
	// Pin the EXACT body: the proxy's canonical chunk plus the protocol
	// terminator, with nothing inserted, dropped, or re-ordered between them.
	// (The upstream's raw frame is re-serialized by the proxy serializer.)
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"index\":0}],\"id\":\"chatcmpl-torana\",\"object\":\"chat.completion.chunk\"}\n\n" +
		"data: [DONE]\n\n"
	if string(body) != want {
		t.Fatalf("stream body differs from the exact expected SSE:\n got: %q\nwant: %q", body, want)
	}
	t.Log("the observational hook holds EOF until released — synchronous with HTTP completion by design")
}

// A client disconnect mid-stream must not take down the server.
//
// ReverseProxy panics with http.ErrAbortHandler when copying to a gone client;
// this handler re-panics it so net/http handles the abort quietly. Lifetime
// ordering (EndRequest after streamDone) is pinned by
// TestRequestCleanupWaitsForStreamingGoroutineOnExceptionalExit — this test
// only covers the network path still serving after that unwind.
func TestClientDisconnectDoesNotTakeDownServer(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-trapper-after-stream/plugin.wasm")

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"first"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-trapper-after-stream"}, AllowUnapproved: true,
		},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	buf := make([]byte, 16)
	_, _ = resp.Body.Read(buf)
	cancel()
	resp.Body.Close()
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		probe, err := http.Post("http://"+ln.Addr().String()+"/provider/oai/v1/chat/completions",
			"application/json", strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
		if err == nil {
			probe.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not survive the disconnect unwind: %v", lastErr)
}
