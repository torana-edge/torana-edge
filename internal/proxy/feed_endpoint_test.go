package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestFeedSnapshotEndpoint verifies that GET /_torana/api/feed returns a JSON
// array, that the /_torana routes do NOT fall through to the provider handler,
// and that events appear in the feed after being added.
func TestFeedSnapshotEndpoint(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[]}`))
	})
	ups := &http.Server{Handler: upstream}
	upsLn, _ := net.Listen("tcp", ":0")
	go ups.Serve(upsLn)
	defer ups.Shutdown(context.Background())

	cfg := Config{
		Port:      "0",
		Providers: testProviderConfig("http://"+upsLn.Addr().String(), "test", "openai"),
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, _ := net.Listen("tcp", ":0")
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	// Feed should start empty — endpoint returns an empty JSON array.
	resp, err := client.Get(base + "/_torana/api/feed")
	if err != nil {
		t.Fatalf("GET /feed: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if string(b) != "[]" {
		t.Errorf("empty feed = %s, want []", string(b))
	}

	// Manually add an event and check it appears in the snapshot.
	srv.feed.Add(metrics.RequestEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Provider:  "test",
		Model:     "gpt-4o",
		Status:    200,
		LatencyMS: 42.0,
	})

	resp2, err := client.Get(base + "/_torana/api/feed")
	if err != nil {
		t.Fatalf("GET /feed (after add): %v", err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	var events []metrics.RequestEvent
	if err := json.Unmarshal(b2, &events); err != nil {
		t.Fatalf("unmarshal feed: %v\nbody: %s", err, string(b2))
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Provider != "test" {
		t.Errorf("events[0].Provider = %q, want test", events[0].Provider)
	}
	if events[0].LatencyMS != 42.0 {
		t.Errorf("events[0].LatencyMS = %f, want 42.0", events[0].LatencyMS)
	}

	// Verify the feed route does NOT fall through to the provider catch-all:
	// calling a non-GET method must return 405, not a provider error.
	resp3, err := client.Post(base+"/_torana/api/feed", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /feed: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /feed status = %d, want 405", resp3.StatusCode)
	}
}

// TestFeedSSEStreamSnapshotReplay verifies that connecting to /_torana/api/stream
// causes any pre-existing events to be replayed as SSE data frames before live
// events start (snapshot-on-connect behaviour). This avoids the race between
// Add() and Subscribe() that makes the live-events path hard to test reliably.
func TestFeedSSEStreamSnapshotReplay(t *testing.T) {
	cfg := Config{
		Port:      "0",
		Providers: provider.Config{Providers: map[string]provider.Provider{"test": {URL: "http://localhost", Format: "openai"}}},
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, _ := net.Listen("tcp", ":0")
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	base := "http://" + ln.Addr().String()

	// Populate the feed BEFORE connecting to the SSE endpoint; the handler
	// will replay it as the initial snapshot.
	srv.feed.Add(metrics.RequestEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Provider:  "snapshot-provider",
		Status:    200,
	})

	// Connect to the SSE endpoint.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/_torana/api/stream", nil)
	tr := &http.Transport{}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// The handler replays the snapshot before blocking on new events, so
	// the very first SSE frame must carry our pre-seeded event.
	gotLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				gotLine <- line
				return
			}
		}
		close(gotLine)
	}()

	select {
	case line, ok := <-gotLine:
		if !ok {
			t.Fatal("SSE stream closed before snapshot replay arrived")
		}
		if !strings.Contains(line, "\"provider\":\"snapshot-provider\"") {
			t.Errorf("SSE replay: unexpected data, got: %s", line)
		}
	case <-time.After(5 * time.Second):
		t.Error("timed out waiting for SSE snapshot replay")
	}
}
