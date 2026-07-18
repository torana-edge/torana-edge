package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/provider"

	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
)

// fixtureEnv starts a proxy loading the given example fixture(s) against an
// upstream in the given format. Returns a post helper and the upstream hit
// counter.
func fixtureEnv(t *testing.T, order []string, formatName string, upstream http.HandlerFunc) (func(body string) (int, http.Header, []byte), *int32) {
	t.Helper()
	for _, name := range order {
		requireWASM(t, "../../examples/plugins/"+name+"/plugin.wasm")
	}

	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		upstream(w, r)
	}))
	t.Cleanup(up.Close)

	srv, err := New(Config{
		Providers: provider.Config{
			Providers: map[string]provider.Provider{"p": {URL: up.URL, Format: formatName}},
			Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: order},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 30 * time.Second}

	post := func(body string) (int, http.Header, []byte) {
		req, _ := http.NewRequest("POST", base+"/provider/p/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, b
	}
	return post, &hits
}

// respondReq builds a minimal request in the given format whose user message
// contains the responder trigger word.
func respondReq(formatName string, stream bool) string {
	switch formatName {
	case "anthropic":
		s := `{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"respondme please"}]`
		if stream {
			s += `,"stream":true`
		}
		return s + `}`
	case "bedrock":
		return `{"messages":[{"role":"user","content":[{"text":"respondme please"}]}]}`
	case "gemini":
		return `{"contents":[{"role":"user","parts":[{"text":"respondme please"}]}]}`
	default:
		s := `{"model":"gpt-x","messages":[{"role":"user","content":"respondme please"}]`
		if stream {
			s += `,"stream":true`
		}
		return s + `}`
	}
}

// TestRespondDirectlyAllFormats: the responder fixture serves a canned
// completion in each provider format — a valid envelope the format's own
// adapter can parse back — and upstream is never called.
func TestRespondDirectlyAllFormats(t *testing.T) {
	for _, formatName := range []string{"openai", "anthropic", "bedrock", "gemini"} {
		t.Run(formatName, func(t *testing.T) {
			post, hits := fixtureEnv(t, []string{"test-responder"}, formatName,
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{}`)
				})

			status, hdr, body := post(respondReq(formatName, false))
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, body)
			}
			if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !strings.Contains(string(body), "canned response from test-responder") {
				t.Fatalf("canned content missing: %s", body)
			}
			// The envelope must be well-formed for the format: check the
			// format-specific completion markers.
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("response is not valid JSON: %v", err)
			}
			marker := map[string]string{
				"openai":    "choices",
				"anthropic": "content",
				"bedrock":   "output",
				"gemini":    "candidates",
			}[formatName]
			if _, ok := decoded[marker]; !ok {
				t.Fatalf("%s envelope missing %q: %s", formatName, marker, body)
			}
			if n := atomic.LoadInt32(hits); n != 0 {
				t.Fatalf("upstream called %d times; direct response must not reach upstream", n)
			}
		})
	}
}

// TestRespondDirectlyStreaming: with stream:true the canned response arrives
// as a well-formed SSE stream the format adapter can parse back.
func TestRespondDirectlyStreaming(t *testing.T) {
	post, hits := fixtureEnv(t, []string{"test-responder"}, "openai",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		})

	status, hdr, body := post(respondReq("openai", true))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("stream missing [DONE]:\n%s", body)
	}

	// Parse the synthetic stream back through the adapter: content + finish.
	f := format.Lookup("openai")
	var text string
	var finished bool
	for ev := range f.Stream.ParseStream(strings.NewReader(string(body))) {
		if ev.TextDelta != nil {
			text += *ev.TextDelta
		}
		if ev.FinishReason != "" {
			finished = true
		}
	}
	if text != "canned response from test-responder" || !finished {
		t.Fatalf("reparse: text=%q finished=%v", text, finished)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times, want 0", n)
	}
}

// TestRespondDirectlyRequiresGrant: a _respond verdict from a plugin without
// env.respond_request is ignored — the request reaches upstream.
func TestRespondDirectlyRequiresGrant(t *testing.T) {
	post, hits := fixtureEnv(t, []string{"test-responder-nogrant"}, "openai",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"x","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"upstream says hi"}}]}`)
		})

	status, _, body := post(respondReq("openai", false))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(string(body), "this must never reach a client") {
		t.Fatalf("ungranted _respond verdict was honored: %s", body)
	}
	if !strings.Contains(string(body), "upstream says hi") {
		t.Fatalf("request should have been forwarded upstream: %s", body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream called %d times, want 1", n)
	}
}

// TestOriginalRequestAndResponseVisibility: a plugin that mutates the request
// can still read the caller's pristine request and the raw upstream body via
// the env.original_request / env.original_response host calls.
func TestOriginalRequestAndResponseVisibility(t *testing.T) {
	var upstreamModel atomic.Value
	post, _ := fixtureEnv(t, []string{"test-original"}, "openai",
		func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &req)
			upstreamModel.Store(req.Model)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"x","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"pristine-upstream-marker"}}]}`)
		})

	status, _, body := post(`{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	// The plugin's mutation reached upstream…
	if got, _ := upstreamModel.Load().(string); got != "gpt-x-mutated" {
		t.Fatalf("upstream model = %q, want gpt-x-mutated (plugin mutation lost)", got)
	}
	// …but the plugin still saw the pristine originals.
	if !strings.Contains(string(body), "orig-model=gpt-x ") {
		t.Fatalf("plugin could not read the pristine request: %s", body)
	}
	if !strings.Contains(string(body), "raw=pristine") {
		t.Fatalf("plugin could not read the raw upstream body: %s", body)
	}
}
