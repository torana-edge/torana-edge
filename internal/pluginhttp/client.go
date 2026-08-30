package pluginhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

type Client struct {
	mu      sync.Mutex
	windows map[string]*window
}
type window struct {
	start  time.Time
	calls  int
	active int
}

func New() *Client { return &Client{windows: map[string]*window{}} }

func (c *Client) Do(ctx context.Context, plugin string, resource wasm.HTTPResource, in *pbv1.OutboundHTTPRequestArgs) (*pbv1.OutboundHTTPResponse, error) {
	if in == nil || !resource.Methods[in.Method] {
		return nil, fmt.Errorf("method is not approved")
	}
	if int64(len(in.Body)) > resource.MaxRequestBytes {
		return nil, fmt.Errorf("request exceeds approved size")
	}
	release, err := c.acquire(plugin+"\x00"+resource.Name, resource.MaxCallsPerMinute)
	if err != nil {
		return nil, err
	}
	defer release()
	origin, err := url.Parse(resource.Origin)
	if err != nil {
		return nil, fmt.Errorf("invalid approved origin")
	}
	relative, err := url.Parse(in.Path)
	if err != nil || relative.IsAbs() || relative.Host != "" || strings.HasPrefix(in.Path, "//") {
		return nil, fmt.Errorf("path must be relative to approved origin")
	}
	target := origin.ResolveReference(relative)
	if target.Scheme != origin.Scheme || target.Host != origin.Host {
		return nil, fmt.Errorf("path escaped approved origin")
	}
	timeout := resource.Timeout
	if in.TimeoutMs > 0 && time.Duration(in.TimeoutMs)*time.Millisecond < timeout {
		timeout = time.Duration(in.TimeoutMs) * time.Millisecond
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("endpoint has no timeout budget")
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, in.Method, target.String(), bytes.NewReader(in.Body))
	if err != nil {
		return nil, err
	}
	for _, header := range in.Headers {
		if forbiddenHeader(header.Name) {
			return nil, fmt.Errorf("header %q is not allowed", header.Name)
		}
		for _, value := range header.Values {
			req.Header.Add(header.Name, value)
		}
	}
	transport, err := transportFor(origin)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, resource.MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > resource.MaxResponseBytes {
		return nil, fmt.Errorf("response exceeds approved size")
	}
	out := &pbv1.OutboundHTTPResponse{Status: int32(resp.StatusCode), Body: body}
	for name, values := range resp.Header {
		if forbiddenHeader(name) {
			continue
		}
		out.Headers = append(out.Headers, &pbv1.HTTPHeader{Name: name, Values: append([]string(nil), values...)})
	}
	return out, nil
}

func (c *Client) acquire(key string, rpm int) (func(), error) {
	if rpm <= 0 {
		return nil, fmt.Errorf("endpoint has no call budget")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	w := c.windows[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		c.windows[key] = w
	}
	if w.calls >= rpm {
		return nil, fmt.Errorf("endpoint rate limit exceeded")
	}
	if w.active >= 4 {
		return nil, fmt.Errorf("endpoint concurrency limit exceeded")
	}
	w.calls++
	w.active++
	return func() { c.mu.Lock(); w.active--; c.mu.Unlock() }, nil
}

func forbiddenHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Trailer", "Host":
		return true
	}
	return false
}

func transportFor(origin *url.URL) (*http.Transport, error) {
	host := origin.Hostname()
	port := origin.Port()
	if port == "" {
		if origin.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	literal := net.ParseIP(host)
	if origin.Scheme == "http" && (literal == nil || !literal.IsLoopback()) {
		return nil, fmt.Errorf("plaintext endpoint is not literal loopback")
	}
	return &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var ips []net.IP
			if literal != nil {
				ips = []net.IP{literal}
			} else {
				resolved, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
				ips = resolved
			}
			for _, ip := range ips {
				if origin.Scheme == "https" && unsafeIP(ip) {
					continue
				}
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
			return nil, fmt.Errorf("approved origin resolved only to blocked addresses")
		},
		DisableCompression:     true,
		MaxIdleConnsPerHost:    4,
		MaxResponseHeaderBytes: 64 << 10,
		IdleConnTimeout:        30 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
	}, nil
}

func unsafeIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 169 && v4[1] == 254 {
			return true
		}
		if v4[0] == 100 && v4[1]&0xc0 == 0x40 {
			return true
		}
	}
	return false
}
