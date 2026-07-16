package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// routedReq captures what an upstream saw for one request.
type routedReq struct {
	Model string
	Auth  string
	XKey  string
}

// routerEnv starts a proxy with the test-router fixture, a "main" upstream and
// a "cheap" upstream (keyed via TEST_CHEAP_KEY), plus a format-mismatched
// "wrongfmt" provider. Returns a post helper and per-upstream request logs.
func routerEnv(t *testing.T) (func(msg string) (int, []byte), *sync.Map) {
	t.Helper()
	requireWASM(t, "../../examples/plugins/test-router/plugin.wasm")
	t.Setenv("TEST_CHEAP_KEY", "cheap-secret")

	var seen sync.Map // upstream name → []routedReq
	record := func(name string, w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		entry := routedReq{Model: req.Model, Auth: r.Header.Get("Authorization"), XKey: r.Header.Get("X-Api-Key")}
		v, _ := seen.LoadOrStore(name, []routedReq{})
		seen.Store(name, append(v.([]routedReq), entry))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","model":"`+req.Model+`","choices":[{"message":{"role":"assistant","content":"from `+name+`"}}]}`)
	}
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { record("main", w, r) }))
	cheapUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { record("cheap", w, r) }))
	t.Cleanup(mainUp.Close)
	t.Cleanup(cheapUp.Close)

	srv, err := New(Config{
		Providers: provider.Config{
			Providers: map[string]provider.Provider{
				"main":     {URL: mainUp.URL, Format: "openai"},
				"cheap":    {URL: cheapUp.URL, Format: "openai", APIKeyEnv: "TEST_CHEAP_KEY"},
				"wrongfmt": {URL: cheapUp.URL, Format: "anthropic"},
			},
			Plugins: provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-router"}},
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

	post := func(msg string) (int, []byte) {
		body := `{"model":"gpt-premium","messages":[{"role":"user","content":"` + msg + `"}]}`
		req, _ := http.NewRequest("POST", base+"/provider/main/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer caller-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	return post, &seen
}

func requestsTo(seen *sync.Map, name string) []routedReq {
	v, ok := seen.Load(name)
	if !ok {
		return nil
	}
	return v.([]routedReq)
}

// TestRouteToCheapProvider: the plugin reroutes to the cheap provider — the
// request lands there with the target's own key (never the caller's) and the
// overridden model.
func TestRouteToCheapProvider(t *testing.T) {
	post, seen := routerEnv(t)

	status, body := post("routecheap please")
	if status != http.StatusOK {
		t.Fatalf("status = %d; body=%s", status, body)
	}
	if !strings.Contains(string(body), "from cheap") {
		t.Fatalf("response should come from the cheap upstream: %s", body)
	}
	if got := requestsTo(seen, "main"); len(got) != 0 {
		t.Fatalf("main upstream called %d times, want 0", len(got))
	}
	got := requestsTo(seen, "cheap")
	if len(got) != 1 {
		t.Fatalf("cheap upstream called %d times, want 1", len(got))
	}
	r := got[0]
	if r.Model != "small-model" {
		t.Errorf("model = %q, want small-model", r.Model)
	}
	if r.Auth != "Bearer cheap-secret" || r.XKey != "cheap-secret" {
		t.Errorf("target credentials wrong: auth=%q xkey=%q", r.Auth, r.XKey)
	}
	if strings.Contains(r.Auth, "caller-secret") || strings.Contains(r.XKey, "caller-secret") {
		t.Errorf("caller credential leaked to rerouted provider: auth=%q", r.Auth)
	}
}

// TestRouteModelOnlyOverride: an empty provider with a model overrides the
// model on the original provider, caller credentials intact.
func TestRouteModelOnlyOverride(t *testing.T) {
	post, seen := routerEnv(t)

	status, _ := post("routemodel please")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	got := requestsTo(seen, "main")
	if len(got) != 1 {
		t.Fatalf("main upstream called %d times, want 1", len(got))
	}
	if got[0].Model != "tiny-model" {
		t.Errorf("model = %q, want tiny-model", got[0].Model)
	}
	if got[0].Auth != "Bearer caller-secret" {
		t.Errorf("caller credential should be intact on the original provider, got %q", got[0].Auth)
	}
}

// TestRouteFailsOpen: verdicts naming an unknown provider or a provider with
// a different wire format are ignored — the request proceeds on the original
// route (with the model override applied, since that part is valid).
func TestRouteFailsOpen(t *testing.T) {
	for _, trigger := range []string{"routebroken", "routewrongfmt"} {
		t.Run(trigger, func(t *testing.T) {
			post, seen := routerEnv(t)

			status, body := post(trigger + " please")
			if status != http.StatusOK {
				t.Fatalf("status = %d; body=%s", status, body)
			}
			if !strings.Contains(string(body), "from main") {
				t.Fatalf("response should come from the original upstream: %s", body)
			}
			if got := requestsTo(seen, "main"); len(got) != 1 {
				t.Fatalf("main upstream called %d times, want 1", len(got))
			}
		})
	}
}
