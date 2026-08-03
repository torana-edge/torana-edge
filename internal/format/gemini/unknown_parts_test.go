package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// The gemini unknown-part contract (review finding 3): a provider-valid
// unmodelled part (inlineData, fileData, future arms) must round-trip
// LOSSLESSLY — raw members, lexemes, and wire order — through the ordered
// body and a marshal; a payload a plugin later poisons with a canonical
// member is rejected before marshal.

// TestUnknownPartsRoundTripLossless — real inline-data/file-data/future-arm
// rows with lexeme + order pins.
func TestUnknownPartsRoundTripLossless(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"user","parts":[
		{"text":"look at this"},
		{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}},
		{"text":"and this"},
		{"fileData":{"fileUri":"gs://bucket/x.pdf","mimeType":"application/pdf"}},
		{"customFutureArm":{"nested":[1,2,3],"raw":true}}
	]}]}`

	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(chat.Messages))
	}
	blocks := chat.Messages[0].Blocks
	if len(blocks) != 5 {
		t.Fatalf("blocks = %d, want 5 (text, inlineData, text, fileData, future)", len(blocks))
	}
	// The unknown arms carry the RAW payload minus the canonical members.
	checkUnknown := func(i int, want string) {
		t.Helper()
		u := blocks[i].Unknown
		if u == nil {
			t.Fatalf("block %d is not an unknown arm: %+v", i, blocks[i])
		}
		if !strings.Contains(string(u.Payload.Bytes()), want) {
			t.Errorf("block %d payload lost %q: %s", i, want, u.Payload.Bytes())
		}
	}
	checkUnknown(1, `"mimeType":"image/png"`)
	checkUnknown(1, `"data":"iVBORw0KGgo="`)
	checkUnknown(3, `"fileUri":"gs://bucket/x.pdf"`)
	checkUnknown(4, `"nested":[1,2,3]`)

	// The canonical members were stripped from the payloads.
	for _, i := range []int{1, 3, 4} {
		if strings.Contains(string(blocks[i].Unknown.Payload.Bytes()), `"text"`) {
			t.Errorf("block %d payload carries the canonical text member", i)
		}
	}

	// Marshal: the parts re-emit in the SAME order with the SAME raw
	// members (order pin: unknown arms ride at their exact positions).
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawFirstContentParts(t, out)
	if len(parts) != 5 {
		t.Fatalf("parts = %d, want 5: %s", len(parts), out)
	}
	rawText(t, parts[0], "look at this")
	if parts[1]["inlineData"] == nil {
		t.Errorf("parts[1] lost inlineData: %s", out)
	}
	if !strings.Contains(string(mustRaw(t, parts[1])), `"data":"iVBORw0KGgo="`) {
		t.Errorf("parts[1] inlineData data lost: %s", out)
	}
	rawText(t, parts[2], "and this")
	if parts[3]["fileData"] == nil {
		t.Errorf("parts[3] lost fileData: %s", out)
	}
	if parts[4]["customFutureArm"] == nil {
		t.Errorf("parts[4] lost the future arm: %s", out)
	}
	if !strings.Contains(string(mustRaw(t, parts[4])), `"nested":[1,2,3]`) {
		t.Errorf("parts[4] future-arm payload lost: %s", out)
	}

	// Re-parse: stable round trip (same block shape).
	again, err := (&Adapter{}).Unmarshal(out)
	if err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if reBlocks(again.Messages[0]) != reBlocks(chat.Messages[0]) {
		t.Errorf("round trip not stable:\n  got  %s\n  want %s",
			reBlocks(again.Messages[0]), reBlocks(chat.Messages[0]))
	}
}

// TestUnknownPartsAssistantRideTextContent — unknown arms in an assistant
// message ride the text/thinking content at their wire position.
func TestUnknownPartsAssistantRideTextContent(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"model","parts":[
		{"text":"answer"},
		{"customPart":{"x":1}},
		{"text":"","thoughtSignature":"TRAIL"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawFirstContentParts(t, out)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3: %s", len(parts), out)
	}
	rawText(t, parts[0], "answer")
	if parts[1]["customPart"] == nil {
		t.Errorf("parts[1] lost the custom arm: %s", out)
	}
	if parts[2]["thoughtSignature"] != "TRAIL" {
		t.Errorf("parts[2] trailing signature lost: %s", out)
	}
}

// TestUnknownPartProjectionInvariant — a plugin-poisoned payload carrying a
// canonical member is rejected before marshal.
func TestUnknownPartProjectionInvariant(t *testing.T) {
	chat := &engine.ChatRequest{
		Model: "m",
		Messages: []engine.Message{{
			Role: engine.RoleUser,
			Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "hi"}},
				{Unknown: &engine.UnknownBlock{Kind: "part", Payload: mustReqG(`{"inlineData":{"mimeType":"image/png"}}`)}},
			},
		}},
	}
	if _, err := (&Adapter{}).Marshal(chat); err != nil {
		t.Fatalf("a clean unknown payload must marshal: %v", err)
	}
	for _, canon := range []string{`"text"`, `"thought"`, `"functionCall"`} {
		poisoned := &engine.ChatRequest{
			Model: "m",
			Messages: []engine.Message{{
				Role: engine.RoleUser,
				Blocks: []engine.Block{
					{Unknown: &engine.UnknownBlock{Kind: "part", Payload: mustReqG(`{"inlineData":{},"` + strings.Trim(canon, `"`) + `":"evil"}`)}},
				},
			}},
		}
		if _, err := (&Adapter{}).Marshal(poisoned); err == nil {
			t.Fatalf("payload duplicating canonical member %s must be rejected", canon)
		}
	}
}

// TestSystemPartsTextOnly — a non-text system part is a fact drop, refused
// at parse rather than silently skipped.
func TestSystemPartsTextOnly(t *testing.T) {
	_, err := (&Adapter{}).Unmarshal([]byte(`{"model":"m","systemInstruction":{"parts":[{"functionCall":{"name":"r","args":{},"id":"c1"}}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err == nil {
		t.Fatal("a functionCall system part must be rejected")
	}
	if !strings.Contains(err.Error(), "system") {
		t.Fatalf("error = %q, want the system condition named", err)
	}
}

func mustReqG(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

// rawFirstContentParts extracts the first content's parts from a marshaled
// body (role-agnostic; rawModelParts assumes a single model content).
func rawFirstContentParts(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("marshal output invalid: %v", err)
	}
	inner := top
	if req, ok := top["request"].(map[string]any); ok {
		inner = req
	}
	contents, _ := inner["contents"].([]any)
	if len(contents) == 0 {
		t.Fatalf("no contents: %s", out)
	}
	cm := contents[0].(map[string]any)
	parts, _ := cm["parts"].([]any)
	outParts := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		outParts = append(outParts, p.(map[string]any))
	}
	return outParts
}

func mustRaw(t *testing.T, part map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
