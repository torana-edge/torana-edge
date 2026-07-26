package conversationcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/conversation"
)

// fakeControlPlane serves the conversations endpoint the CLI reads.
func fakeControlPlane(t *testing.T, records []conversation.Record) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_torana/api/conversations" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Conversations []conversation.Record `json:"conversations"`
		}{records})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func run(t *testing.T, addr string, extra ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"conversations", "--addr", addr}, extra...)
	err := Run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func TestListsConversationsAsTable(t *testing.T) {
	now := time.Now()
	addr := fakeControlPlane(t, []conversation.Record{
		{ID: "a3f9c2e1", Model: "claude-sonnet-4-5", Turns: 12, LastActive: now.Add(-2 * time.Minute), LastCacheRead: 118000},
		{ID: "7b1e04aa", Model: "gemini-2.5-pro", Turns: 3, LastActive: now.Add(-41 * time.Minute), LastCacheWrite: 62000},
	})

	stdout, _, err := run(t, addr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"a3f9c2e1", "2m ago", "claude-sonnet-4-5", "118k read", "7b1e04aa", "41m ago", "62k written"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

// TestEmptyListSaysSo — an empty table teaches nothing; the operator needs to
// know whether the proxy has simply not seen traffic yet.
func TestEmptyListSaysSo(t *testing.T) {
	stdout, _, err := run(t, fakeControlPlane(t, nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(stdout, "No conversations recorded yet") {
		t.Errorf("empty output was not explained:\n%s", stdout)
	}
}

func TestJSONOutputIsMachineReadable(t *testing.T) {
	addr := fakeControlPlane(t, []conversation.Record{
		{ID: "a3f9c2e1", Model: "sonnet", CachePrefixKey: "beef1234", Turns: 2},
	})

	stdout, _, err := run(t, addr, "--json")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got []conversation.Record
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json did not emit valid JSON: %v\n%s", err, stdout)
	}
	if len(got) != 1 || got[0].ID != "a3f9c2e1" || got[0].CachePrefixKey != "beef1234" {
		t.Errorf("round-trip lost data: %+v", got)
	}
}

// TestUnreachableProxyExplainsItself — the overwhelmingly likely cause is that
// the proxy is not running, so the error should say that rather than surfacing
// a bare dial error.
func TestUnreachableProxyExplainsItself(t *testing.T) {
	// Port 1 on loopback: reserved, and nothing will be listening.
	_, _, err := run(t, "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected an error against an unreachable proxy")
	}
	if !strings.Contains(err.Error(), "is it running?") {
		t.Errorf("error does not suggest the likely cause: %v", err)
	}
}

func TestServerErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "control plane is localhost-only", http.StatusForbidden)
	}))
	defer srv.Close()

	_, _, err := run(t, strings.TrimPrefix(srv.URL, "http://"))
	if err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "localhost-only") {
		t.Errorf("error dropped the server's explanation: %v", err)
	}
}

func TestHumanizeAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{2 * time.Minute, "2m ago"},
		{90 * time.Minute, "1h ago"},
		{50 * time.Hour, "2d ago"},
		{-5 * time.Second, "just now"}, // clock skew must not print a negative age
	} {
		if got := humanizeAge(tc.d); got != tc.want {
			t.Errorf("humanizeAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
