package plugin_sdk

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/pkg/pb"
)

func TestToolNamesByCallID(t *testing.T) {
	names := ToolNamesByCallID([]*pb.Message{{Role: "assistant", ToolCalls: []*pb.ToolCall{{Id: "call-1", Name: "read_file"}}}})
	if names["call-1"] != "read_file" {
		t.Fatalf("tool name not recovered: %v", names)
	}
}

func TestMatchToolPolicyOrderedAndCaseInsensitive(t *testing.T) {
	rules := []ToolPolicyRule{
		{Match: "WEB_*", Mode: "deterministic", FirstPass: true},
		{Match: "web_search", Mode: "exact"},
	}
	got, ok := MatchToolPolicy(rules, "Web_Search")
	if !ok || got.Mode != "deterministic" || !got.FirstPass {
		t.Fatalf("first case-insensitive match must win: got %+v, ok=%v", got, ok)
	}
	if _, ok := MatchToolPolicy(rules, "unknown"); ok {
		t.Fatal("unknown tool unexpectedly matched")
	}
}

func TestToolResultMustStayExact(t *testing.T) {
	for _, tool := range []string{"apply_patch", "WriteFile", "git-diff", "text_editor", "report_errors"} {
		if !ToolResultMustStayExact(tool, "successful output") {
			t.Errorf("safety-sensitive tool %q was not protected", tool)
		}
	}
	if !ToolResultMustStayExact("shell", "output\nerror: compile failed") {
		t.Fatal("error output was not protected")
	}
	for _, output := range []string{
		"file.go:12: error: undefined symbol",
		"error[E0382]: use of moved value",
		"FAIL package/example",
		"FAILED integration test",
		"exit status 1",
		"Process exited with code 137",
		`{"exit_code": 2}`,
	} {
		if !ToolResultMustStayExact("shell", output) {
			t.Errorf("failure output was not protected: %q", output)
		}
	}
	if ToolResultMustStayExact("web_search", "successful recoverable results") {
		t.Fatal("recoverable output unexpectedly protected")
	}
}

func TestDeterministicToolReplacementStableAndRecoverable(t *testing.T) {
	content := strings.Repeat("head data\n", 300) + strings.Repeat("tail data\n", 300)
	wantHash := ToolOutputSHA256(content)
	a := DeterministicToolReplacement("Web_Search", `{"q":"torana"}`, content, "deterministic", "Repeat the same query.", false)
	b := DeterministicToolReplacement("Web_Search", `{"q":"torana"}`, content, "deterministic", "Repeat the same query.", false)
	if a != b {
		t.Fatal("replacement changed for identical input")
	}
	for _, want := range []string{
		ToolOutputMarker,
		`tool_arguments: {"q":"torana"}`,
		"original_bytes: ",
		"sha256: " + wantHash,
		"retained_bytes: ",
		"omitted_bytes: ",
		"rerun: Repeat the same query.",
		"--- retained head ---",
		"--- retained tail ---",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("replacement missing %q", want)
		}
	}
	if !IsDeterministicToolReplacement(a) {
		t.Fatal("canonical replacement was not recognized on replay")
	}

	marker := DeterministicToolReplacement("read_file", `{"path":"main.go"}`, content, "source", "Read the file again.", true)
	if strings.Contains(marker, "retained head") || !strings.Contains(marker, "retained_bytes: 0") {
		t.Fatalf("source reread marker retained source text: %q", marker)
	}
}
