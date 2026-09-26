package proxy

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

func TestCandidateAllowsOnlyPreexistingUnrelatedSkippedPlugins(t *testing.T) {
	broken := plugin.SkippedPlugin{Name: "broken", Digest: "sha256:old", Reason: "unapproved"}
	if introducesSkippedPlugins([]plugin.SkippedPlugin{broken}, []plugin.SkippedPlugin{broken}, "logger") {
		t.Fatal("unrelated existing skipped plugin froze change")
	}
	if !introducesSkippedPlugins([]plugin.SkippedPlugin{broken}, []plugin.SkippedPlugin{broken}, "broken") {
		t.Fatal("target skipped plugin accepted")
	}
	changed := broken
	changed.Digest = "sha256:new"
	if !introducesSkippedPlugins([]plugin.SkippedPlugin{broken}, []plugin.SkippedPlugin{changed}, "logger") {
		t.Fatal("new skipped digest accepted")
	}
	if !introducesSkippedPlugins(nil, []plugin.SkippedPlugin{broken}, "logger") {
		t.Fatal("new skipped plugin accepted")
	}
}

func TestAppliedHistoryIncompleteIsNotRetryableFailure(t *testing.T) {
	result := appliedHistoryIncomplete("logger", "_disable")
	if !result.OK || result.Error != nil || result.Status != "applied_history_incomplete" || !strings.Contains(result.Summary, "Do not retry") {
		t.Fatalf("result=%+v", result)
	}
}
