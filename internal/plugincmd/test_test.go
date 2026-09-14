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
	if err := os.WriteFile(filepath.Join(dir, "scenario.json"), []byte(`{"expected_error":"missing"}`), 0600); err != nil {
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

func TestPluginTestReportsStreamCallbackFailure(t *testing.T) {
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
	if err := Run([]string{"plugin", "test", dir}, &out, &stderr); err != nil {
		t.Fatalf("plugin test: %v\nstderr=%s", err, stderr.String())
	}
}
