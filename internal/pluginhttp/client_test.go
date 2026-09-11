package pluginhttp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestScopedLoopbackRequest(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/check" || r.Header.Get("X-Test") != "one" {
			t.Errorf("request = %s, header %q", r.URL.Path, r.Header.Get("X-Test"))
		}
		w.Header().Add("X-Result", "a")
		w.Header().Add("X-Result", "b")
		_, _ = w.Write([]byte("ok"))
	}))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	resource := wasm.HTTPResource{Name: "service", Origin: server.URL, Methods: map[string]bool{"GET": true}, Timeout: time.Second, MaxRequestBytes: 10, MaxResponseBytes: 10, MaxCallsPerMinute: 2}
	out, err := New().Do(context.Background(), "plugin", resource, &pbv1.OutboundHTTPRequestArgs{Method: "GET", Path: "/v1/check", Headers: []*pbv1.HTTPHeader{{Name: "X-Test", Values: []string{"one"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != 200 || string(out.Body) != "ok" || len(out.Headers) == 0 {
		t.Fatalf("response = %+v", out)
	}
}

func TestScopedRequestsReuseOneTransportConnection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()

	client := New()
	defer client.CloseIdleConnections()
	resource := wasm.HTTPResource{Name: "service", Origin: server.URL, Methods: map[string]bool{"GET": true}, Timeout: time.Second, MaxRequestBytes: 10, MaxResponseBytes: 10, MaxCallsPerMinute: 100}
	for i := 0; i < 20; i++ {
		out, err := client.Do(context.Background(), "plugin", resource, &pbv1.OutboundHTTPRequestArgs{Method: "GET", Path: "/"})
		if err != nil {
			t.Fatal(err)
		}
		if string(out.Body) != "ok" {
			t.Fatalf("response %d = %q", i, out.Body)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("new connections = %d, want one reused connection", got)
	}
}

func TestScopedRequestRejectsUnapprovedInputs(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()

	resource := wasm.HTTPResource{Name: "service", Origin: server.URL, Methods: map[string]bool{"GET": true}, Timeout: time.Second, MaxRequestBytes: 1, MaxResponseBytes: 1, MaxCallsPerMinute: 100}
	cases := []struct {
		name string
		in   *pbv1.OutboundHTTPRequestArgs
		want string
	}{
		{"method", &pbv1.OutboundHTTPRequestArgs{Method: "POST", Path: "/"}, "method is not approved"},
		{"absolute path", &pbv1.OutboundHTTPRequestArgs{Method: "GET", Path: "https://example.com/"}, "path must be relative"},
		{"request size", &pbv1.OutboundHTTPRequestArgs{Method: "GET", Path: "/", Body: []byte("xx")}, "request exceeds approved size"},
		{"host header", &pbv1.OutboundHTTPRequestArgs{Method: "GET", Path: "/", Headers: []*pbv1.HTTPHeader{{Name: "Host", Values: []string{"example.com"}}}}, `header "Host" is not allowed`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := New()
			defer client.CloseIdleConnections()
			if _, err := client.Do(context.Background(), "plugin", resource, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("rejected requests reached the endpoint %d times", got)
	}
}

func TestUnsafeIPClassification(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "100.64.0.1", "::1"} {
		if !unsafeIP(net.ParseIP(raw)) {
			t.Errorf("%s was not blocked", raw)
		}
	}
	if unsafeIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address classified unsafe")
	}
}
