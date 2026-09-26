package controlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPTokenIsScopedAndRedirectsAreRejected(t *testing.T) {
	const token = "private-token-sentinel"
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing MCP authentication")
		}
		http.Redirect(w, r, "/_torana/api/v1/config", http.StatusFound)
	}))
	defer server.Close()
	client, err := New(server.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	httpClient := client.MCPHTTPClient(token)
	defer httpClient.CloseIdleConnections()
	request, err := http.NewRequestWithContext(context.Background(), "GET", server.URL+"/_torana/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || received.Load() != 1 {
		t.Fatal("MCP client followed a redirect")
	}
	for _, target := range []string{server.URL + "/_torana/api/v1/config", "http://example.invalid/_torana/mcp"} {
		request, err := http.NewRequestWithContext(context.Background(), "GET", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := httpClient.Do(request)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatal("out-of-scope request was allowed or leaked its token")
		}
	}
	if received.Load() != 1 {
		t.Fatal("credential scope was not enforced before network access")
	}
}
