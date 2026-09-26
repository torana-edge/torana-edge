package annotate

import (
	"errors"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/secret"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

func TestSignedNoticeStripsAcrossRestart(t *testing.T) {
	directory := t.TempDir()
	first, err := secret.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	item := suggest.Suggestion{ID: "sg_abc", Code: "7f3k", Title: "Try Opus", Body: "Repeated tool failures", HarnessTargetModel: "opus"}
	notice, err := Render(first, "conversation-a", item)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secret.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	content := "Original model answer."
	replayed := content + notice
	got, changed, err := Strip(second, "conversation-a", replayed)
	if err != nil || !changed || got != content {
		t.Fatalf("strip after restart = %q, changed %v, error %v", got, changed, err)
	}
	if got, changed, err := Strip(second, "conversation-b", replayed); err != nil || changed || got != replayed {
		t.Fatalf("cross-conversation strip = %q, changed %v, error %v", got, changed, err)
	}
}

func TestSignedProbeStripsWithoutBecomingALocalReply(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe, err := RenderProbe(signer, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(probe, "local_") || strings.Contains(probe, "accept") {
		t.Fatalf("probe looks actionable or like a local reply: %q", probe)
	}
	got, changed, err := Strip(signer, "conversation", "answer"+probe)
	if err != nil || !changed || got != "answer" {
		t.Fatalf("probe strip = %q, %v, %v", got, changed, err)
	}
}

func TestSignedNoticeAllowsInteriorReflowButRefusesBrokenEnd(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notice, err := Render(signer, "c", suggest.Suggestion{ID: "sg_abc", Code: "7f3k", Title: "Try Opus"})
	if err != nil {
		t.Fatal(err)
	}
	reflowed := strings.Replace(notice, "To accept, reply with:\n", "To accept,\n  reply with:\n", 1)
	got, changed, err := Strip(signer, "c", "answer"+reflowed)
	if err != nil || !changed || got != "answer" {
		t.Fatalf("reflow strip = %q, changed %v, error %v", got, changed, err)
	}
	broken := notice[:strings.Index(notice, endPrefix)]
	if _, _, err := Strip(signer, "c", "answer"+broken); !errors.Is(err, ErrStripFailed) {
		t.Fatalf("broken signed span: %v", err)
	}
}
