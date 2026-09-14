package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/testfixture"
)

func TestPluginTestRequiresCompiledBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"schema_version":1,"id":"test/x","name":"x","version":"0.1.0","abi_version":"v1","failure_mode":"pass","hooks":[],"permissions":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scenario.json"), []byte(`{"request":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := Run([]string{"plugin", "test", dir}, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "plugin.wasm") {
		t.Fatalf("err=%v", err)
	}
}

func TestPluginTestRejectsMalformedScenario(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"request":`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := Run([]string{"plugin", "test", dir, "--scenario", path}, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "parse scenario") {
		t.Fatalf("err=%v", err)
	}
}

func TestPluginTestRunsCompiledMutator(t *testing.T) {
	src := filepath.Join("..", "..", "examples", "plugins", "test-mutator")
	testfixture.Require(t, filepath.Join(src, "plugin.wasm"))
	dir := filepath.Join(t.TempDir(), "mutator")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dir); err != nil {
		t.Fatal(err)
	}
	scenario := filepath.Join(dir, "scenario.json")
	raw := `{"request":{"model":"user","messages":[{"role":"user","blocks":[{"text":{"text":"hello"}}]}]},"expected_request":{"model":"user","messages":[{"role":"user","blocks":[{"text":{"text":"hello [seen by test-mutator]"}}]}]}}`
	if err := os.WriteFile(scenario, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := Run([]string{"plugin", "test", dir}, &out, &stderr); err != nil {
		t.Fatalf("plugin test: %v\nstderr=%s", err, stderr.String())
	}
}

func TestPluginTestRunsCompiledStreamJournal(t *testing.T) {
	src := filepath.Join("..", "..", "examples", "plugins", "test-stream-journal")
	testfixture.Require(t, filepath.Join(src, "plugin.wasm"))
	dir := filepath.Join(t.TempDir(), "stream-journal")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dir); err != nil {
		t.Fatal(err)
	}
	scenario := filepath.Join(dir, "scenario.json")
	raw := `{"stream":[{"textDelta":"hello"}],"expected_stream":[{"textDelta":"SHOULD-NOT-RUN"}]}`
	if err := os.WriteFile(scenario, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := Run([]string{"plugin", "test", dir}, &out, &stderr); err != nil {
		t.Fatalf("plugin test: %v\nstderr=%s", err, stderr.String())
	}
}

func TestPluginTestRejectsInvalidStreamBeforeDispatch(t *testing.T) {
	src := filepath.Join("..", "..", "examples", "plugins", "test-stream-journal")
	testfixture.Require(t, filepath.Join(src, "plugin.wasm"))
	dir := filepath.Join(t.TempDir(), "stream-failure")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scenario.json"), []byte(`{"stream":[{"toolCallDelta":{"index":0,"argumentsDelta":"FAIL"}}],"expected_error":"no open tool block"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := Run([]string{"plugin", "test", dir}, &out, &stderr); err == nil || !strings.Contains(err.Error(), "no open tool block") {
		t.Fatalf("plugin test: %v\nstderr=%s", err, stderr.String())
	}
}

func runCompiledScenario(t *testing.T, name, scenario string) error {
	t.Helper()
	src := filepath.Join("..", "..", "examples", "plugins", name)
	testfixture.Require(t, filepath.Join(src, "plugin.wasm"))
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scenario.json"), []byte(scenario), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	return Run([]string{"plugin", "test", dir}, &out, &stderr)
}

func TestPluginTestChecksAllCanonicalHooks(t *testing.T) {
	for _, tc := range []struct{ name, fixture, scenario string }{
		{"http", "test-http-server", `{"http":{"method":"GET","path":"/"},"expected_http":{"status":200,"headersJson":"eyJDb250ZW50LVR5cGUiOlsidGV4dC9odG1sOyBjaGFyc2V0PXV0Zi04Il19","body":"PGgxPnRlc3QtaHR0cC1zZXJ2ZXI8L2gxPjxwPkdFVCAvPC9wPg=="}}`},
		{"tick", "test-ticker", `{"tick":{"tickId":"2","unixMillis":"1000","intervalMs":"500"},"expected_tick":{"actions":2,"note":"tick 2"}}`},
		{"idle_tick", "test-ticker", `{"tick":{"tickId":"1"},"expected_tick":null}`},
		{"response", "test-mutator", `{"response":{"model":"m","message":{"blocks":[{"toolCall":{"id":"call1","name":"f","argumentsJson":"e30="}}]}},"expected_response":{"model":"m","message":{"blocks":[{"toolCall":{"id":"call1","name":"f","argumentsJson":"eyJtdXRhdGVkX2J5IjoidGVzdC1tdXRhdG9yIn0="}}]}}}`},
		{"readonly_response", "test-mutator", `{"response_mutable":false,"response":{"message":{"blocks":[{"toolCall":{"id":"call1","name":"f","argumentsJson":"e30="}}]}},"expected_response":{"message":{"blocks":[{"toolCall":{"id":"call1","name":"f","argumentsJson":"e30="}}]}}}`},
		{"suppressed_tool", "test-stream-journal", `{"stream":[{"contentBlockStart":{"index":0,"toolCall":{"id":"call1","name":"f","invocationKind":"TOOL_INVOCATION_KIND_FUNCTION"}}},{"toolCallDelta":{"index":0,"argumentsDelta":"{}"}},{"contentBlockStop":{"index":0}}],"expected_stream":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runCompiledScenario(t, tc.fixture, tc.scenario); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPluginTestRejectsUnusedAndAmbiguousExpectations(t *testing.T) {
	for _, raw := range []string{`{}`, `{"expected_error":"anything"}`, `{"request":{},"request":{}}`, `{"request":{},"typo":true}`, `{"request":{},"config":null}`, `{"request":{},"config":{"nested":{"x":1,"x":2}}}`, `{"tick":{},"expected_request":{}}`, `{"response":null}`, `{"request":{},"expected_request":null}`, `{"response":{},"expected_response":null}`, `{"request":{},"expected_verdicts":null}`, `{"tick":{},"expected_verdicts":{}}`, `{"request":{},"expected_verdicts":{"block":null}}`, `{"request":{},"expected_verdicts":{"unknown":{}}}`, `{"request":{}} {}`} {
		dir := t.TempDir()
		name := filepath.Join(dir, "scenario.json")
		if err := os.WriteFile(name, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readPluginScenario(name); err == nil {
			t.Errorf("accepted invalid scenario %s", raw)
		}
	}
	if err := runCompiledScenario(t, "test-stream-journal", `{"stream":[{"textDelta":"hello"}],"expected_stream":[]}`); err == nil || !strings.Contains(err.Error(), "stream mismatch") {
		t.Fatalf("empty stream expectation ignored: %v", err)
	}
	if err := runCompiledScenario(t, "test-ticker", `{"tick":{"tickId":"2"},"expected_tick":null}`); err == nil || !strings.Contains(err.Error(), "tick outcome mismatch") {
		t.Fatalf("idle tick expectation ignored: %v", err)
	}
}

func TestPluginTestChecksCompiledVerdicts(t *testing.T) {
	request := func(text string) string {
		return `{"model":"m","messages":[{"role":"user","blocks":[{"text":{"text":"` + text + `"}}]}]}`
	}
	for _, tc := range []struct{ name, fixture, scenario string }{
		{"none", "test-blocker", `{"request":` + request("ordinary") + `,"expected_verdicts":{}}`},
		{"block", "test-blocker", `{"request":` + request("blockme") + `,"expected_verdicts":{"block":{"status":422,"code":"blocked_by_test","message":"Blocked by test-blocker: request contained the trigger word."}}}`},
		{"respond", "test-responder", `{"request":` + request("respondme") + `,"expected_verdicts":{"respond":{"message":{"blocks":[{"text":{"text":"canned response from test-responder"}}]},"finishReason":"stop"}}}`},
		{"route", "test-router", `{"request":` + request("routecheap") + `,"expected_verdicts":{"route":{"provider":"cheap","model":"small-model"}}}`},
		{"identity", "test-identity", `{"request":` + request("ordinary") + `,"expected_verdicts":{"identity":{"identity":"fixture-tenant"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runCompiledScenario(t, tc.fixture, tc.scenario); err != nil {
				t.Fatal(err)
			}
		})
	}
}
