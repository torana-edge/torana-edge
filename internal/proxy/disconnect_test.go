package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"

	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// TestClientDisconnectCancelsUpstream pins the fix for #77: when a downstream
// client hangs up mid-stream, the cancellation must propagate to the upstream
// provider so it stops generating (and billing) tokens for a client that's gone.
//
// The mock upstream streams one SSE frame, then blocks watching its request
// context; the test connects, reads the first frame, disconnects, and asserts
// the upstream observes cancellation promptly.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstreamDone := make(chan struct{})

	upstream := newRawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		// Emit one frame so the client has something to read, then hold the
		// stream open and watch for cancellation.
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
			close(upstreamCancelled)
		case <-time.After(5 * time.Second):
			// Cancellation never arrived — the test will fail on the timeout below.
		}
	})
	defer upstream.Close()

	// Proxy with no plugins — this exercises the pure streaming path
	// (ParseStream → SerializeStream) where the disconnect handling lives.
	cfg := Config{
		Port: "0",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{
				"oai": {URL: upstream.URL, Format: "openai"},
			},
		},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())
	base := "http://" + ln.Addr().String()

	// Fire a streaming request, read the first frame, then cancel (disconnect).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/provider/oai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	buf := make([]byte, 32)
	if _, err := resp.Body.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("read first frame: %v", err)
	}

	// Client hangs up mid-stream.
	cancel()
	resp.Body.Close()

	select {
	case <-upstreamCancelled:
		// Success: the provider saw the cancellation and would stop generating.
	case <-time.After(3 * time.Second):
		t.Fatal("#77: upstream request context was NOT cancelled after client disconnect — provider would keep burning tokens")
	}
	<-upstreamDone
}

// newRawSSEServer starts an HTTP server that serves handler and is closed on
// cleanup. Kept separate so the handler can watch r.Context() for cancellation.
func newRawSSEServer(t *testing.T, handler http.HandlerFunc) *rawServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: handler}
	go s.Serve(ln)
	return &rawServer{URL: "http://" + ln.Addr().String(), srv: s}
}

type rawServer struct {
	URL string
	srv *http.Server
}

func (r *rawServer) Close() { r.srv.Close() }
