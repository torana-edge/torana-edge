package proxy

// End-to-end tests for the stream-signature enforcement's transport-level
// contract (Migration B part 2b): a typed terminal error must abort the
// client's response — truncated body, no finish marker, connection closed
// without the chunked terminator — and a valid signed stream must complete
// cleanly.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestStreamEnforcementTerminalAbortsClientBody is the client-level proof of
// the termination semantics: a plugin that rewrites a signed block's args
// while keeping the token (stale — test-tool-rewriter) is discovered at the
// block's scope close, AFTER the block's earlier content has already been
// serialized to the client. The response must not appear to have completed
// normally: the client reads a partial body and then hits unexpected EOF —
// the connection was closed without the chunked terminator.
//
// The upstream deliberately pauses between the (valid, forwarded) signed text
// block and the (violating) signed tool call so the text frame clears the
// proxy's 200ms stream flush interval before the abort; otherwise the buffered
// frame could be lost with the connection and the test would only prove an
// empty truncation.
func TestStreamEnforcementTerminalAbortsClientBody(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-tool-rewriter/plugin.wasm")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frame := func(s string) {
			fmt.Fprint(w, s+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		// Signed text block: passes verification, serialized to the client.
		frame(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello from gemini","thoughtSignature":"SIG_TEXT"}]}}]}`)
		// Pause so the text frame is flushed to the client before the abort.
		time.Sleep(300 * time.Millisecond)
		// Signed tool call: test-tool-rewriter rewrites the args while the
		// token rides the start — stale at the block's scope close.
		frame(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{"q":"original"}},"thoughtSignature":"SIG_CALL"}]}}]}`)
		frame(`data: {"candidates":[{"finishReason":"STOP"}]}`)
	}))
	defer upstream.Close()

	// Capture the log line carrying the violation attribution. The server's
	// background goroutines may still log (drain, observational hook) after
	// the client body read returns, so the capture must be synchronized.
	var logBuf syncLogBuffer
	oldOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldOut)

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"gem": {URL: upstream.URL, Format: "gemini"}},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-tool-rewriter"}, AllowUnapproved: true,
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

	resp, err := http.Post("http://"+ln.Addr().String()+"/provider/gem/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"gemini-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()

	if strings.Contains(string(body), "no provider configured") {
		t.Fatalf("the request never reached the pipeline (%s)", body)
	}

	// The response must NOT appear to have completed normally.
	if readErr == nil {
		t.Fatalf("terminated stream completed cleanly: body=%q", body)
	}
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF from the aborted response, got %v (body=%q)", readErr, body)
	}
	// The finish frame must never have been written.
	if strings.Contains(string(body), "finishReason") {
		t.Fatalf("the finish marker reached the client on a terminated stream: %q", body)
	}
	// The violating tool call must never have been serialized (the gemini
	// serializer emits the functionCall part at ToolCallEnd, which was never
	// forwarded).
	if strings.Contains(string(body), "functionCall") {
		t.Fatalf("the violating tool call reached the client: %q", body)
	}
	// The pre-terminal content IS visible: the abort happens after a partial
	// body, not before any of it.
	if !strings.Contains(string(body), "hello from gemini") {
		t.Fatalf("expected the pre-terminal text frame on the wire, got %q", body)
	}

	// Observability: the violation is attributed (plugin + invariant + index)
	// in the logs. The signed tool block is index 1 (the text block took 0),
	// and it is the SECOND scope to close.
	gotLog := logBuf.String()
	for _, want := range []string{"test-tool-rewriter", "stale", "block 1"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("terminal attribution missing %q from logs:\n%s", want, gotLog)
		}
	}

	// The server survives the abort unwind.
	probe, err := http.Post("http://"+ln.Addr().String()+"/provider/gem/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"gemini-x","messages":[{"role":"user","content":"again"}]}`))
	if err != nil {
		t.Fatalf("server did not survive the abort: %v", err)
	}
	probe.Body.Close()
}

// syncLogBuffer is a mutex-guarded bytes.Buffer for log.SetOutput: the server
// goroutines log concurrently with the test reading the captured output.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncLogBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncLogBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestStreamEnforcementValidSignedStreamCompletes: a valid signed stream
// through the verified path completes cleanly — the client sees the full
// body including the function call and the finish marker, with a clean
// chunked terminator. Enforcement must not fire on valid streams.
func TestStreamEnforcementValidSignedStreamCompletes(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frames := []string{
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"all good","thoughtSignature":"SIG_TEXT"}]}}]}`,
			`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{"q":"x"}},"thoughtSignature":"SIG_CALL"}]}}]}`,
			`data: {"candidates":[{"finishReason":"STOP"}]}`,
		}
		for _, f := range frames {
			fmt.Fprint(w, f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"gem": {URL: upstream.URL, Format: "gemini"}},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-stream-mutator"}, AllowUnapproved: true,
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

	resp, err := http.Post("http://"+ln.Addr().String()+"/provider/gem/v1/chat/completions",
		"application/json", strings.NewReader(`{"model":"gemini-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()

	if strings.Contains(string(body), "no provider configured") {
		t.Fatalf("the request never reached the pipeline (%s)", body)
	}
	if readErr != nil {
		t.Fatalf("a valid signed stream must complete cleanly: %v (body=%q)", readErr, body)
	}
	for _, want := range []string{"all good", "SIG_TEXT", "SIG_CALL", "functionCall", "finishReason"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("valid signed stream body missing %q: %s", want, body)
		}
	}
}
