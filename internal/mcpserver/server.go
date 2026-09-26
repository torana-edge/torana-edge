// Package mcpserver adapts Torana's fixed model-facing tools to the official MCP
// SDK. Domain dispatch, binding and consent stay in the host, not the transport.
package mcpserver

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed tools.json
var toolDefinitions []byte

//go:embed instructions.txt
var instructions string

// Result is a model-safe domain outcome. A Go error is an internal failure,
// never a validation or consent outcome, and is not forwarded to clients.
type Result struct {
	OK               bool                 `json:"ok"`
	Namespace        string               `json:"namespace,omitempty"`
	Operation        string               `json:"operation,omitempty"`
	Result           any                  `json:"result,omitempty"`
	Status           string               `json:"status,omitempty"`
	Summary          string               `json:"summary,omitempty"`
	ExpiresInSeconds int                  `json:"expires_in_seconds,omitempty"`
	Conversation     *ConversationBinding `json:"conversation,omitempty"`
	Error            *DomainError         `json:"error,omitempty"`
	InterfaceVersion int                  `json:"interface_version"`
}

type ConversationBinding struct {
	Binding string `json:"binding"`
}

// DomainError contains only deliberately model-visible host-generated details.
// Raw provider errors, configuration and confirmation codes do not belong here.
type DomainError struct {
	Code      string        `json:"code"`
	Message   string        `json:"message"`
	Retryable bool          `json:"retryable"`
	Details   *ErrorDetails `json:"details,omitempty"`
}

// ErrorDetails is intentionally an allowlist, not a passthrough for handler
// output. Extend it only with explicitly model-safe diagnostic fields.
type ErrorDetails struct {
	Path              string `json:"path,omitempty"`
	Status            string `json:"status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

type Dispatch func(context.Context, string, json.RawMessage) (Result, error)

type Options struct {
	Version string
	// Token is evaluated for every HTTP request so rotation invalidates old
	// sessions too. The caller loads the token through Torana's secret store.
	Token    func() string
	Dispatch Dispatch
}

// Handler owns transport sessions and active requests. Shutdown stops admission,
// cancels streams, and closes sessions before host resources are released.
type Handler struct {
	http.Handler
	server *mcp.Server
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	active sync.WaitGroup
	once   sync.Once
	done   chan struct{}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		http.Error(w, "MCP is stopping", http.StatusServiceUnavailable)
		return
	}
	h.active.Add(1)
	h.mu.Unlock()
	defer h.active.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	defer cancel()
	h.Handler.ServeHTTP(w, r.WithContext(ctx))
}

func (h *Handler) Shutdown(ctx context.Context) error {
	h.once.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
		h.cancel()
		go func() {
			for session := range h.server.Sessions() {
				_ = session.Close()
			}
			h.active.Wait()
			// Initialization admitted just before shutdown can create a session
			// after the first snapshot. No new request can start at this point.
			for session := range h.server.Sessions() {
				_ = session.Close()
			}
			close(h.done)
		}()
	})
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func NewHandler(options Options) (*Handler, error) {
	if options.Token == nil || options.Dispatch == nil {
		return nil, errors.New("MCP token provider and dispatcher are required")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "torana", Version: options.Version}, &mcp.ServerOptions{Instructions: instructions})
	var tools []*mcp.Tool
	if err := json.Unmarshal(toolDefinitions, &tools); err != nil {
		return nil, err
	}
	for _, tool := range tools {
		name := tool.Name
		mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
			raw, err := json.Marshal(input)
			if err != nil {
				return nil, nil, err
			}
			output, err := options.Dispatch(ctx, name, raw)
			if err != nil || output.OK == (output.Error != nil) {
				// Do not forward transport/internal errors, which may contain
				// credentials or raw provider/configuration data, to the model.
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Torana could not complete this operation."}}}, nil, nil
			}
			output.InterfaceVersion = 1
			encoded, err := json.Marshal(output)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Torana could not complete this operation."}}}, nil, nil
			}
			return &mcp.CallToolResult{IsError: !output.OK, StructuredContent: output, Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil, nil
		})
	}
	// Correlation evidence expires after 120 seconds; MCP sessions do not.
	// Coding clients commonly remain idle while users inspect or edit code.
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: 24 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	handler := &Handler{server: server, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	handler.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r) {
			http.Error(w, "MCP is available only on loopback", http.StatusForbidden)
			return
		}
		values := r.Header.Values("Authorization")
		expected := options.Token()
		if len(values) != 1 || expected == "" || !strings.HasPrefix(values[0], "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(values[0], "Bearer ")), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="torana"`)
			http.Error(w, "MCP authentication required", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		transport.ServeHTTP(w, r)
	})
	return handler, nil
}

func loopbackHost(host string) bool {
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localRequest(r *http.Request) bool {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(remote)
	if ip == nil || !ip.IsLoopback() || !loopbackHost(r.Host) {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return false
	}
	if len(origins) == 1 {
		origin, err := url.Parse(origins[0])
		if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host != r.Host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			return false
		}
	}
	return true
}
