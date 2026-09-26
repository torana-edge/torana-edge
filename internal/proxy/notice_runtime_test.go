package proxy

import (
	"bytes"
	"testing"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/secret"
	"github.com/torana-edge/torana-edge/internal/suggest"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestAppendPendingNoticeOnlyOnCompletedCurrentTurn(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := suggest.New(state)
	turn, err := store.ObserveUserTurn("conversation", "question")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("conversation", "router", turn, &pb.SuggestArgs{Kind: "model_switch", DedupeKey: "up", Title: "Try Opus", Body: "A stronger model may help."}); err != nil {
		t.Fatal(err)
	}
	s := &Server{secrets: signer, suggestions: store}
	rs := &reqState{ConversationID: "conversation", UserTurn: turn, NoticeEnabled: true}
	complete := []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"answer"}}]}`)
	got := s.appendPendingNotice(complete, rs, "openai-chat")
	if !bytes.Contains(got, []byte("torana:begin:")) {
		t.Fatal("current-turn suggestion was not delivered")
	}
	tool := []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{}]}}]}`)
	if got := s.appendPendingNotice(tool, rs, "openai-chat"); !bytes.Equal(got, tool) {
		t.Fatal("notice inserted on tool-calling turn")
	}
	rs.UserTurn++
	if got := s.appendPendingNotice(complete, rs, "openai-chat"); !bytes.Equal(got, complete) {
		t.Fatal("old suggestion repeated on later turn")
	}
	rs.NoticeEnabled = false
	if got := s.appendPendingNotice(complete, rs, "openai-chat"); !bytes.Equal(got, complete) {
		t.Fatal("notice inserted for unapproved harness")
	}
}

func TestNoticeProbeUsesNormalPlacementWithoutSuggestion(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{secrets: signer, suggestions: suggest.New(state)}
	rs := &reqState{ConversationID: "conversation", UserTurn: 1, NoticeEnabled: true, NoticeProbe: true}
	complete := []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"answer"}}]}`)
	got := s.appendPendingNotice(complete, rs, "openai-chat")
	if !bytes.Contains(got, []byte("Notice probe")) || bytes.Contains(got, []byte("accept")) {
		t.Fatalf("probe not delivered safely: %s", got)
	}
	tool := []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{}]}}]}`)
	if got := s.appendPendingNotice(tool, rs, "openai-chat"); !bytes.Equal(got, tool) {
		t.Fatal("probe inserted on tool-calling turn")
	}
	rs.NoticeEnabled = false
	if got := s.appendPendingNotice(complete, rs, "openai-chat"); !bytes.Equal(got, complete) {
		t.Fatal("probe inserted for unapproved harness")
	}
}

func TestUnknownIdentitySourceCannotEnableNoticeDelivery(t *testing.T) {
	for _, source := range []string{"", "content-root", "harness-thread", "harness-session", "openai-responses-conversation", "openai-responses-parent"} {
		if knownNoticeHarnessSource(source) {
			t.Fatalf("unknown harness source %q became notice-eligible", source)
		}
	}
	for _, source := range []string{"claude-code-session", "codex-thread", "codex-session", "codex-client-thread", "gemini-code-assist-session"} {
		if !knownNoticeHarnessSource(source) {
			t.Fatalf("named harness source %q was not notice-eligible", source)
		}
	}
}
