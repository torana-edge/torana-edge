package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestUnsupportedOutputFormatReturnsClient400(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-router/plugin.wasm")
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer upstream.Close()
	srv, err := New(Config{Providers: provider.Config{
		Providers: map[string]provider.Provider{"main": {URL: upstream.URL, Format: "anthropic"}},
		Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-router"}, AllowUnapproved: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"routemodel"}],"output_config":{"format":{"type":"json_object"}}}`
	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/provider/main/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(out), "cannot be represented") {
		t.Fatalf("response = %d %s", resp.StatusCode, out)
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream calls = %d", hits.Load())
	}
}
