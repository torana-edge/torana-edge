package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestSuccessfulRequestDoesNotLogTrafficOrQuerySecrets(t *testing.T) {
	const secret = "SUCCESS-QUERY-SECRET-7f3d9c2a"
	querySeen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		querySeen <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(upstream.Close)

	logs := captureLogs(t)
	srv, err := New(Config{Providers: provider.Config{Providers: map[string]provider.Provider{
		"openai": {URL: upstream.URL, Format: "openai"},
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Shutdown(context.Background())
		ln.Close()
	})

	target := "http://" + ln.Addr().String() + "/provider/openai/v1/chat/completions?api_key=" + secret
	req, err := http.NewRequest(http.MethodPost, target,
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := <-querySeen; got != "api_key="+secret {
		t.Fatalf("upstream query = %q", got)
	}

	completeLog := logs.String()
	for _, forbidden := range []string{secret, "Proxying request to", "Upstream returned 200"} {
		if strings.Contains(completeLog, forbidden) {
			t.Fatalf("successful request log contains %q: %s", forbidden, completeLog)
		}
	}
}
