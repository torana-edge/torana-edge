package mcpshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
)

type capturedPipe struct {
	*io.PipeWriter
	mu   sync.Mutex
	data bytes.Buffer
}

func (p *capturedPipe) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.data.Write(data)
	p.mu.Unlock()
	return p.PipeWriter.Write(data)
}
func (p *capturedPipe) text() string { p.mu.Lock(); defer p.mu.Unlock(); return p.data.String() }

func TestStdioOfficialClientParityAndCancellation(t *testing.T) {
	const token = "private-token-must-not-reach-stdio"
	handler, err := mcpserver.NewHandler(mcpserver.Options{Version: "host-test", Token: func() string { return token }, Dispatch: func(_ context.Context, name string, input json.RawMessage) (mcpserver.Result, error) {
		return mcpserver.Result{OK: true, Result: map[string]any{"name": name, "input": input}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	defer handler.Shutdown(context.Background())
	operator, err := controlclient.New(server.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()
	captured := &capturedPipe{PipeWriter: outW}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, operator, token, &mcp.IOTransport{Reader: inR, Writer: captured, MaxLineLength: 64 << 10})
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "test"}, nil)
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	session, err := client.Connect(deadline, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(deadline, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatal("stdio fixed catalog failed")
	}
	if session.InitializeResult().ServerInfo.Version != "host-test" || !strings.Contains(session.InitializeResult().Instructions, "Interface version: 1") {
		t.Fatal("stdio initialization drifted from host")
	}
	for _, tool := range tools.Tools {
		args := map[string]any{}
		switch tool.Name {
		case "torana_search":
			args["query"] = "status"
		case "torana_describe":
			args["namespace"] = "torana"
		case "torana_invoke":
			args["namespace"] = "torana"
			args["operation"] = "system.status"
		}
		result, err := session.CallTool(deadline, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
		if err != nil || result.IsError {
			encoded, _ := json.Marshal(result)
			t.Fatalf("stdio tool %s forwarding failed: %v; synthetic result=%s", tool.Name, err, encoded)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil || !strings.Contains(string(encoded), tool.Name) || !strings.Contains(string(encoded), `"interface_version":1`) {
			t.Fatal("stdio lost the host result envelope")
		}
	}
	if strings.Contains(captured.text(), token) {
		t.Fatal("stdio leaked the MCP token")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown result=%v", err)
		}
	case <-deadline.Done():
		t.Fatal("stdio cancellation did not unblock its readers")
	}
}

func TestStdioConnectionFailureDoesNotLeakToken(t *testing.T) {
	client, err := controlclient.New("127.0.0.1:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	local, _ := mcp.NewInMemoryTransports()
	err = Run(ctx, client, "private-failure-sentinel", local)
	if err == nil || strings.Contains(err.Error(), "private-failure-sentinel") {
		t.Fatal("connection failure leaked its token or was accepted")
	}
}
