package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")
	requireWASM(t, "../../plugins/intent/plugin.wasm")
	requireWASM(t, "../../plugins/compactor/plugin.wasm")

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
		w.Write([]byte(`{"choices":[{"message":{"content":"summary of the answer"}}]}`))
	}))
	defer offload.Close()

	// --- proxy with real plugins ---------------------------------------------
	cfg := Config{
		Port: "0",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{
				"oai":   {URL: upstream.URL, Format: "openai"},
				"anth":  {URL: upstream.URL + "/anthropic", Format: "anthropic"},
				"cheap": {URL: offload.URL, Format: "openai"},
			},
			Plugins: provider.PluginsConfig{
				Dir: "../../plugins",
				// intent captures "i" into the cache; compactor consumes it.
				// keyword_compactor is its ALTERNATIVE (either/or) and is
				// deliberately not in this pipeline.
				Order: []string{"schema_translator", "intent", "compactor"},
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
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")

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
			Plugins:   provider.PluginsConfig{Dir: "../../plugins", Order: []string{"schema_translator"}},
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
	newPP, err := plugin.NewPipeline(newRT, plugin.PluginConfig{Dir: "../../plugins", Order: []string{"schema_translator"}})
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
	env, _ := args["env"].(map[string]any)
	if env == nil || env["A"] != "1" {
		t.Fatalf("state lost across hot reload — args: %v", args)
	}

	// And after the request completes, the drain must finish promptly.
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("old pipeline never drained after request completion")
	}
}
