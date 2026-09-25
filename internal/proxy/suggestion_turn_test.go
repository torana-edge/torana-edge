package proxy

import (
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/annotate"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/secret"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

func TestUserTurnSignatureIgnoresToolResultContinuations(t *testing.T) {
	chat := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "first"}}}},
	}}
	first := userTurnSignature(chat)
	if first == "" {
		t.Fatal("first user message has no signature")
	}
	chat.Messages = append(chat.Messages, engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{
		{Text: &engine.TextBlock{Text: "tool context"}},
		{ToolResult: &engine.ToolResultBlock{}},
	}})
	if got := userTurnSignature(chat); got != first {
		t.Fatalf("tool continuation changed user turn: %q != %q", got, first)
	}
	chat.Messages = append(chat.Messages, engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "next"}}}})
	if got := userTurnSignature(chat); got == first {
		t.Fatal("new user message reused prior signature")
	}
}

func TestConsecutiveLocalCommandsRemainDistinctUserTurns(t *testing.T) {
	first := []byte(`{"model":"m","messages":[{"role":"user","content":"torana> status"}]}`)
	second := []byte(`{"model":"m","messages":[{"role":"user","content":"torana> status"},{"role":"assistant","content":"local reply"},{"role":"user","content":"torana> status"}]}`)
	parseSignature := func(body []byte) string {
		t.Helper()
		chat, err := format.Lookup("openai").Request.Unmarshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return userTurnSignature(chat)
	}
	firstSignature, secondSignature := parseSignature(first), parseSignature(second)
	if firstSignature == "" || secondSignature == "" || firstSignature == secondSignature {
		t.Fatal("distinct local command turns need distinct admitted signatures")
	}
	firstStripped, _, _, err := stripDirectiveText(first, "openai-chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondStripped, _, _, err := stripDirectiveText(second, "openai-chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if parseSignature(firstStripped) != parseSignature(secondStripped) {
		t.Fatal("fixture no longer reproduces the post-strip turn collision")
	}
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := suggest.New(state)
	for _, tc := range []struct {
		signature string
		want      uint64
	}{
		{firstSignature, 1}, {firstSignature, 1}, // retry
		{secondSignature, 2}, {secondSignature, 2}, // retry
	} {
		turn, err := store.ObserveUserTurn("conversation", tc.signature)
		if err != nil || turn != tc.want {
			t.Fatalf("turn = %d, want %d; error %v", turn, tc.want, err)
		}
	}
}

func TestResponsesLocalParentDistinguishesIdenticalCommandTurns(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chat := &engine.ChatRequest{Messages: []engine.Message{{
		Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "torana> status"}}},
	}}}
	var signatures []string
	for i := 0; i < 2; i++ {
		localID, err := annotate.EncodeLocalResponseID(signer, "resp_real")
		if err != nil {
			t.Fatal(err)
		}
		quoted, _ := json.Marshal(localID)
		body := append([]byte(`{"previous_response_id":`), quoted...)
		body = append(body, '}')
		parent := responsesTurnParent(body)
		if parent != localID {
			t.Fatal("local parent was not captured before rewrite")
		}
		clean, changed, err := rewriteLocalPreviousResponseID(body, signer)
		if err != nil || !changed || responsesTurnParent(clean) != "resp_real" {
			t.Fatalf("parent rewrite = %s, %v, %v", clean, changed, err)
		}
		signature := userTurnSignatureWithParent(chat, parent)
		if signature == "" || signature != userTurnSignatureWithParent(chat, responsesTurnParent(body)) {
			t.Fatal("retry did not keep the same turn signature")
		}
		signatures = append(signatures, signature)
	}
	if signatures[0] == signatures[1] {
		t.Fatal("successive local Responses parents collapsed identical commands")
	}
}
