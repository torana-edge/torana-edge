package controlclient

import (
	"fmt"
	"net/http"
)

type mcpTokenTransport struct {
	base   http.RoundTripper
	origin string
	token  string
}

func (t mcpTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != t.origin || r.URL.Path != "/_torana/mcp" || r.URL.User != nil {
		return nil, fmt.Errorf("refusing to send MCP credentials outside the selected local endpoint")
	}
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// MCPHTTPClient preserves the operator client's loopback-only dialer and
// redirect rejection. The token is scoped to the exact MCP endpoint, never
// passed in a URL, environment variable, subprocess argument, or diagnostic.
// Long-lived streams use request contexts rather than a total HTTP timeout.
func (c *Client) MCPHTTPClient(token string) *http.Client {
	base := c.http.Transport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = 0
	return &http.Client{Transport: mcpTokenTransport{base: base, origin: c.Address(), token: token}, CheckRedirect: c.http.CheckRedirect}
}

func (t mcpTokenTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
