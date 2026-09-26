package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tokenTransport struct{ token string }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.Header = r.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestFixedToolsAndOfficialSDKClient(t *testing.T) {
	handler, err := NewHandler(Options{Version: "test", Token: func() string { return "test-token" }, Dispatch: func(_ context.Context, name string, input json.RawMessage) (Result, error) {
		return Result{OK: true, Result: map[string]any{"tool": name}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: tokenTransport{token: "test-token"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	got, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var expected []*mcp.Tool
	if err := json.Unmarshal(toolDefinitions, &expected); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 4 {
		t.Fatalf("tool count=%d", len(got.Tools))
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range got.Tools {
		byName[tool.Name] = tool
	}
	for _, tool := range expected {
		actual := byName[tool.Name]
		if actual == nil || actual.Description != tool.Description {
			t.Fatalf("tool contract changed: %s", tool.Name)
		}
		a, _ := json.Marshal(actual.InputSchema)
		b, _ := json.Marshal(tool.InputSchema)
		if string(a) != string(b) {
			t.Fatalf("schema changed: %s: %s != %s", tool.Name, a, b)
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "torana_namespaces", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("call=%+v err=%v", result, err)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "torana_invoke", Arguments: map[string]any{"arbitrary": "field"}})
	if err == nil && !result.IsError {
		t.Fatal("invalid invocation input accepted")
	}
}

func TestHandlerShutdownCancelsRequestsAndStopsAdmission(t *testing.T) {
	handler, err := NewHandler(Options{Token: func() string { return "test" }, Dispatch: func(context.Context, string, json.RawMessage) (Result, error) {
		return Result{OK: true}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	exited := make(chan struct{})
	handler.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://localhost/", nil))
		close(exited)
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("shutdown left a request running")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed status=%d", w.Code)
	}
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerShutdownClosesOfficialClientSession(t *testing.T) {
	handler, err := NewHandler(Options{Token: func() string { return "test-token" }, Dispatch: func(context.Context, string, json.RawMessage) (Result, error) {
		return Result{OK: true}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "shutdown-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: tokenTransport{token: "test-token"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for range handler.server.Sessions() {
		t.Fatal("shutdown retained a session")
	}
	if _, err := session.ListTools(ctx, nil); err == nil {
		t.Fatal("closed endpoint still served a session")
	}
}

func TestTransportGuardsAndRotation(t *testing.T) {
	token := "old-token"
	handler, err := NewHandler(Options{Token: func() string { return token }, Dispatch: func(context.Context, string, json.RawMessage) (Result, error) { return Result{OK: true}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, remote, host, origin, auth string
		status                           int
	}{
		{"remote", "192.0.2.1:1234", "localhost", "", "Bearer old-token", 403},
		{"foreign_host", "127.0.0.1:1234", "attacker.example", "", "Bearer old-token", 403},
		{"foreign_origin", "127.0.0.1:1234", "localhost", "http://attacker.example", "Bearer old-token", 403},
		{"missing_token", "127.0.0.1:1234", "localhost", "", "", 401},
		{"wrong_token", "127.0.0.1:1234", "localhost", "", "Bearer wrong", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://localhost/", strings.NewReader(`{}`))
			r.RemoteAddr = tc.remote
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
	token = "rotated-token"
	r := httptest.NewRequest("POST", "http://localhost/", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer old-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("old token remained valid after rotation")
	}
}

func TestInternalErrorsAreNotSentToModel(t *testing.T) {
	handler, err := NewHandler(Options{Token: func() string { return "test-token" }, Dispatch: func(context.Context, string, json.RawMessage) (Result, error) {
		return Result{}, errors.New("secret-canary-123")
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: tokenTransport{token: "test-token"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "torana_namespaces", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "secret-canary") {
		t.Fatal("internal error leaked to model")
	}
}

func TestDomainOutcomesHaveStructuredAndTextEnvelope(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "invalid_input"}[failure], func(t *testing.T) {
			output := Result{OK: true, Namespace: "logger", Operation: "_disable", Status: "pending_confirmation", Summary: "Disable logger"}
			if failure {
				output = Result{Error: &DomainError{Code: "invalid_input", Message: "must be an integer", Details: &ErrorDetails{Path: "/threshold"}}}
			}
			handler, err := NewHandler(Options{Token: func() string { return "test-token" }, Dispatch: func(context.Context, string, json.RawMessage) (Result, error) { return output, nil }})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: tokenTransport{token: "test-token"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			got, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "torana_namespaces", Arguments: map[string]any{}})
			if err != nil || got.IsError != failure {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			structured, err := json.Marshal(got.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Result
			if err := json.Unmarshal(structured, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.InterfaceVersion != 1 || decoded.OK == failure {
				t.Fatalf("envelope=%s", structured)
			}
			if failure && (decoded.Error == nil || decoded.Error.Code != "invalid_input" || decoded.Error.Details == nil || decoded.Error.Details.Path != "/threshold") {
				t.Fatalf("domain error lost: %s", structured)
			}
			if len(got.Content) != 1 {
				t.Fatal("missing text copy")
			}
			textContent, ok := got.Content[0].(*mcp.TextContent)
			var textValue, structuredValue any
			if !ok || json.Unmarshal([]byte(textContent.Text), &textValue) != nil || json.Unmarshal(structured, &structuredValue) != nil || !reflect.DeepEqual(textValue, structuredValue) {
				t.Fatal("text and structured envelopes differ")
			}
		})
	}
}
