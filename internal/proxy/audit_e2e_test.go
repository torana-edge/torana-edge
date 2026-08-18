package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/auditlog"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestAuditRecordsInferenceAndMalformedButNeverAuxiliary(t *testing.T) {
	var upstreamHits atomic.Int64
	var inferenceBytes atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		inferenceBytes.Store(int64(len(body)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r-1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	providers := testProviderConfig(upstream.URL, "test", "openai")
	providers.Audit = &auditlog.Config{Enabled: true, Path: auditPath}
	providers.Plugins = provider.PluginsConfig{
		Dir:             "../../examples/plugins",
		Order:           []string{"test-mutator"},
		AllowUnapproved: true,
	}
	requireWASM(t, "../../examples/plugins/test-mutator/plugin.wasm")
	srv, err := New(Config{Port: "0", Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	client := &http.Client{Timeout: 5 * time.Second}

	aux, err := http.NewRequest(http.MethodPost, proxy.URL+"/provider/test/v1/models", strings.NewReader(`SECRET-auxiliary`))
	if err != nil {
		t.Fatal(err)
	}
	auxResp, err := client.Do(aux)
	if err != nil {
		t.Fatal(err)
	}
	_ = auxResp.Body.Close()

	const inferenceBody = `{"model":"gpt-test","messages":[{"role":"user","content":"inspect"},{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"server.go\",\"i\":\"inspect reload validation\"}"}}]}]}`
	inferenceResp, err := client.Post(proxy.URL+"/provider/test/v1/chat/completions", "application/json", strings.NewReader(inferenceBody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, inferenceResp.Body)
	_ = inferenceResp.Body.Close()
	if inferenceResp.StatusCode != http.StatusOK {
		t.Fatalf("inference status = %d", inferenceResp.StatusCode)
	}

	malformedResp, err := client.Post(proxy.URL+"/provider/test/v1/chat/completions", "application/json", strings.NewReader(`{`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, malformedResp.Body)
	_ = malformedResp.Body.Close()
	if malformedResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed status = %d", malformedResp.StatusCode)
	}

	proxy.Close()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := upstreamHits.Load(); got != 2 { // auxiliary + valid inference; malformed is host-local
		t.Fatalf("upstream hits = %d, want 2", got)
	}

	f, err := os.Open(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []auditlog.Record
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record auditlog.Record
		dec := json.NewDecoder(strings.NewReader(scanner.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&record); err != nil {
			t.Fatalf("decode audit line: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("audit records = %d, want exactly inference + malformed (auxiliary excluded): %#v", len(records), records)
	}
	valid := records[0]
	if valid.SchemaVersion != 1 || valid.InitialProvider != "test" || valid.Provider != "test" ||
		valid.Format != "openai" || valid.Path != "/v1/chat/completions" ||
		valid.InitialModel != "gpt-test" || valid.Model != "gpt-test" || valid.Status != http.StatusOK ||
		valid.IngressBytes != int64(len(inferenceBody)) || valid.UpstreamRequestBytes != inferenceBytes.Load() ||
		valid.UpstreamRequestBytes == valid.IngressBytes || !reflect.DeepEqual(valid.Plugins, []string{"test-mutator"}) ||
		valid.Verdict != "" || valid.ErrorCode != "" {
		t.Fatalf("valid audit record = %#v", valid)
	}
	if len(valid.ToolCalls) != 1 || valid.ToolCalls[0] != (auditlog.ToolCall{
		ID: "call-1", Name: "read_file", Intent: "inspect reload validation",
	}) {
		t.Fatalf("tool calls = %#v", valid.ToolCalls)
	}
	invalid := records[1]
	if invalid.Status != http.StatusBadRequest || invalid.ErrorCode != "invalid_request" ||
		invalid.IngressBytes != 1 || invalid.UpstreamRequestBytes != 0 || invalid.Model != "" || len(invalid.ToolCalls) != 0 {
		t.Fatalf("invalid audit record = %#v", invalid)
	}
}

func TestAuditRecordsAttributedPluginBlockWithoutUpstreamBytes(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	providers := testProviderConfig(upstream.URL, "test", "openai")
	providers.Audit = &auditlog.Config{Enabled: true, Path: auditPath}
	providers.Plugins = provider.PluginsConfig{
		Dir:             "../../examples/plugins",
		Order:           []string{"test-blocker"},
		AllowUnapproved: true,
	}
	requireWASM(t, "../../examples/plugins/test-blocker/plugin.wasm")
	srv, err := New(Config{Port: "0", Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())

	const body = `{"model":"m","messages":[{"role":"user","content":"please blockme now"}]}`
	resp, err := http.Post(proxy.URL+"/provider/test/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}

	proxy.Close()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("blocked request reached upstream %d times", got)
	}

	f, err := os.Open(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		t.Fatalf("missing block audit record: %v", scanner.Err())
	}
	var record auditlog.Record
	dec := json.NewDecoder(strings.NewReader(scanner.Text()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		t.Fatal(err)
	}
	if scanner.Scan() {
		t.Fatalf("unexpected second audit record: %s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if record.SchemaVersion != auditSchemaVersion || record.Status != http.StatusUnprocessableEntity ||
		record.Verdict != "block" || record.VerdictPlugin != "test-blocker" ||
		record.ErrorCode != "plugin_block" || record.UpstreamRequestBytes != 0 ||
		record.IngressBytes != int64(len(body)) || record.Model != "m" ||
		!reflect.DeepEqual(record.Plugins, []string{"test-blocker"}) {
		t.Fatalf("block audit record = %#v", record)
	}
}
