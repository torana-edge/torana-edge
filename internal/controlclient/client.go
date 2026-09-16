// Package controlclient is the shared, loopback-only client for Torana's live
// control plane. Administrative commands never edit the running server's files.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/instance"
	"github.com/torana-edge/torana-edge/internal/provider"
)

const BasePath = "/_torana/api/v1"
const MaxBodyBytes = 10 << 20

type Client struct {
	base       *url.URL
	http       *http.Client
	instanceID string
}

// New accepts host:port or an HTTP(S) origin, never a remote host, URL path,
// credential, or proxy. Redirects are rejected, including loopback redirects:
// an administrative write must go only to the endpoint the operator selected.
func New(addr string, timeout time.Duration) (*Client, error) {
	var instanceID string
	if addr == "" {
		var err error
		addr, instanceID, err = defaultTarget()
		if err != nil {
			return nil, err
		}
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid control-plane address")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("--addr must be a loopback HTTP(S) origin, without credentials, a path, query, or fragment")
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
		return nil, fmt.Errorf("control-plane address must use localhost or a loopback IP")
	}
	if p := u.Port(); p != "" {
		if _, err := validPort(p); err != nil {
			return nil, err
		}
	}
	u.Path = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		h, p, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		// Resolve localhost ourselves rather than trusting hosts/DNS overrides.
		if strings.EqualFold(h, "localhost") {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", p))
			if err == nil {
				return conn, nil
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort("::1", p))
		}
		if !net.ParseIP(h).IsLoopback() {
			return nil, fmt.Errorf("refusing non-loopback connection")
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &Client{base: u, instanceID: instanceID, http: &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Address() string { return c.base.String() }

// DefaultAddress reads configuration without materializing a managed store.
// A typo or unreadable configuration is an error, not permission to administer
// an unrelated server at the default port. --addr bypasses this lookup.
func DefaultAddress() (string, error) {
	addr, _, err := defaultTarget()
	return addr, err
}

func defaultTarget() (string, string, error) {
	// A daemon may have been started from a different shell with a port or
	// IPv6 bind override. Follow its recorded listener while it owns the store;
	// stale records after a crash are deliberately ignored. Explicit --addr
	// still bypasses this lookup.
	store, err := provider.ManagedStorePath()
	if err != nil {
		return "", "", err
	}
	active, err := instance.Running(filepath.Join(filepath.Dir(store), "instance.lock"))
	if err != nil {
		return "", "", err
	}
	if active {
		r, err := instance.ReadRecord(filepath.Join(filepath.Dir(store), "instance.json"))
		if err == nil {
			return r.Address, r.InstanceID, nil
		}
		if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("read running instance address: %w", err)
		}
		return "", "", fmt.Errorf("Torana owns the managed store but has not published its listener; wait for startup or inspect the logs")
	}
	addr, err := configuredAddress()
	return addr, "", err
}

// Listener names an explicitly requested listener, as `serve --port/--bind`
// does. Empty fields fall back to the environment and then the configuration.
//
// It exists so the process that LAUNCHES an instance and the clients that later
// TALK to one resolve the endpoint through the same code. Preflight resolving
// separately is how `start --port` came to probe the old port while launching a
// child on the new one.
type Listener struct {
	Port string
	Bind string
}

// ResolveAddress returns the loopback control-plane address for a listener,
// applying explicit values over TORANA_PORT/TORANA_BIND over the configuration.
func ResolveAddress(requested Listener) (string, error) {
	port := 0
	if requested.Port != "" {
		var err error
		port, err = validPort(requested.Port)
		if err != nil {
			return "", fmt.Errorf("--port: %w", err)
		}
	} else if value := os.Getenv("TORANA_PORT"); value != "" {
		var err error
		port, err = validPort(value)
		if err != nil {
			return "", fmt.Errorf("TORANA_PORT: %w", err)
		}
	} else {
		path, err := provider.ManagedStorePath()
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			path = os.Getenv("TORANA_CONFIG")
			if path == "" {
				path = "config.json"
			}
		} else if err != nil {
			return "", err
		}
		cfg, err := provider.Load(path)
		if err != nil {
			return "", fmt.Errorf("resolve control-plane port: %w (or specify --addr)", err)
		}
		port = cfg.Port
		if _, err := validPort(strconv.Itoa(port)); err != nil {
			return "", err
		}
	}
	host := "127.0.0.1"
	bind, source := requested.Bind, "--bind"
	if bind == "" {
		bind, source = os.Getenv("TORANA_BIND"), "TORANA_BIND"
	}
	if bind != "" {
		bind = strings.Trim(bind, "[]")
		ip := net.ParseIP(bind)
		switch {
		case strings.EqualFold(bind, "localhost"):
			host = "localhost"
		case ip != nil && ip.IsLoopback():
			host = bind
		case ip != nil && ip.IsUnspecified():
			if ip.To4() == nil {
				host = "::1"
			}
		default:
			return "", fmt.Errorf("%s does not expose a loopback control plane; specify --addr for a local tunnel", source)
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func configuredAddress() (string, error) { return ResolveAddress(Listener{}) }

func validPort(value string) (int, error) {
	p, err := strconv.Atoi(value)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("port must be an integer between 1 and 65535")
	}
	return p, nil
}

// APIError preserves the server's machine category and HTTP status.
type APIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control plane: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

func (c *Client) Open(ctx context.Context, method, path string, body []byte, revision string) (*http.Response, error) {
	return c.open(ctx, method, path, body, revision, "")
}

func (c *Client) open(ctx context.Context, method, path string, body []byte, revision, pluginDigest string) (*http.Response, error) {
	p, err := url.Parse(path)
	if err != nil || p.IsAbs() || p.Host != "" || p.Fragment != "" || !strings.HasPrefix(p.Path, BasePath+"/") || p.RawPath != "" || strings.Contains(p.Path, "..") {
		return nil, fmt.Errorf("invalid control-plane API path")
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("request exceeds %d bytes", MaxBodyBytes)
	}
	u := *c.base
	u.Path, u.RawQuery = p.Path, p.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Torana-Local-Request", "1")
	if c.instanceID != "" {
		req.Header.Set("X-Torana-Instance-ID", c.instanceID)
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if revision != "" {
		req.Header.Set("If-Match", revision)
	}
	if pluginDigest != "" {
		req.Header.Set("X-Torana-Plugin-Digest", pluginDigest)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the proxy at %s — is it running? A timed-out write may have applied; inspect before retrying: %w", c.base.Host, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, err := ReadBounded(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Error APIError `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	e := envelope.Error
	e.Status = resp.StatusCode
	if e.Code == "" {
		e.Code = "request_failed"
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(raw))
	}
	if resp.StatusCode == http.StatusPreconditionFailed && e.Code == "stale_revision" {
		e.Message = "configuration changed; read a fresh snapshot, review it, and retry"
	}
	return nil, &e
}

func (c *Client) JSON(ctx context.Context, method, path string, body []byte, revision string) (json.RawMessage, string, error) {
	return c.json(ctx, method, path, body, revision, "")
}

// CallPlugin binds invocation to the bundle whose operation/schema/risk was
// just discovered. A concurrent reload must not execute a different contract.
func (c *Client) CallPlugin(ctx context.Context, method, path string, body []byte, digest string) (json.RawMessage, string, error) {
	if digest == "" || !strings.HasPrefix(path, BasePath+"/agent/plugins/") {
		return nil, "", fmt.Errorf("plugin operation requires a discovered digest and plugin API path")
	}
	return c.json(ctx, method, path, body, "", digest)
}

func (c *Client) json(ctx context.Context, method, path string, body []byte, revision, pluginDigest string) (json.RawMessage, string, error) {
	resp, err := c.open(ctx, method, path, body, revision, pluginDigest)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := ReadBounded(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if !json.Valid(raw) {
		return nil, "", fmt.Errorf("control plane returned invalid JSON")
	}
	return raw, resp.Header.Get("ETag"), nil
}

func ReadBounded(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxBodyBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", MaxBodyBytes)
	}
	return raw, nil
}
