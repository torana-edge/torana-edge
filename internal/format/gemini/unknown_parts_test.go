package gemini

import (
	"bytes"
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

// The executable Part grammar (review round 2 finding 3): exactly one arm
// member per part, only documented modifier combinations, deliberate
// future-arm policy. Every ambiguous row must be the value-free parse
// error; every legal combination must parse and round-trip.

func TestGeminiPartGrammar(t *testing.T) {
	unmarshal := func(body string) error {
		_, err := (&Adapter{}).Unmarshal([]byte(body))
		return err
	}
	refuse := map[string]string{
		"text+inlineData on one part":     `{"model":"m","contents":[{"role":"user","parts":[{"text":"x","inlineData":{"mimeType":"image/png"}}]}]}`,
		"inlineData+fileData on one part": `{"model":"m","contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png"},"fileData":{"fileUri":"gs://b/x"}}]}]}`,
		"text+functionCall on one part":   `{"model":"m","contents":[{"role":"model","parts":[{"text":"x","functionCall":{"name":"r","args":{},"id":"c1"}}]}]}`,
		"thought on a non-text arm":       `{"model":"m","contents":[{"role":"model","parts":[{"thought":true,"functionCall":{"name":"r","args":{},"id":"c1"}}]}]}`,

		"modifiers only (no arm)": `{"model":"m","contents":[{"role":"model","parts":[{"thought":true,"thoughtSignature":"S"}]}]}`,
		"system unknown arm":      `{"model":"m","systemInstruction":{"parts":[{"inlineData":{"mimeType":"image/png"}}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		"system future arm":       `{"model":"m","systemInstruction":{"parts":[{"customFuture":{"x":1}}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		"system thought":          `{"model":"m","systemInstruction":{"parts":[{"thought":true,"text":"r"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
	}
	for name, body := range refuse {
		t.Run("refuse/"+name, func(t *testing.T) {
			if err := unmarshal(body); err == nil {
				t.Fatal("ambiguous part accepted")
			}
		})
	}

	accept := map[string]string{
		"thinking arm":            `{"model":"m","contents":[{"role":"model","parts":[{"thought":true,"text":"r","thoughtSignature":"S"}]}]}`,
		"content-bound signature": `{"model":"m","contents":[{"role":"model","parts":[{"text":"a","thoughtSignature":"S"}]}]}`,
		"trailing standalone":     `{"model":"m","contents":[{"role":"model","parts":[{"text":"a"},{"text":"","thoughtSignature":"S"}]}]}`,
		"call-bound signature":    `{"model":"m","contents":[{"role":"model","parts":[{"functionCall":{"name":"r","args":{},"id":"c1"},"thoughtSignature":"S"}]}]}`,
		"future arm alone":        `{"model":"m","contents":[{"role":"user","parts":[{"customFutureArm":{"nested":[1,2,3]}}]}]}`,
		// REV 4: thoughtSignature is legal on ANY arm (provider guidance),
		// including media arms — the signature is a typed carrier.
		"thoughtSignature on a media arm": `{"model":"m","contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png"},"thoughtSignature":"S"}]}]}`,
	}
	for name, body := range accept {
		t.Run("accept/"+name, func(t *testing.T) {
			chat, err := (&Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("legal combination refused: %v", err)
			}
			if _, err := (&Adapter{}).Marshal(chat); err != nil {
				t.Fatalf("legal combination failed to marshal: %v", err)
			}
		})
	}
}

// Review round 3 finding 3 reproductions — the Gemini Part grammar is not
// Google's actual grammar, and the ABI has no signature carriers for
// media/future arms or tool results. These rows are RED pending the SDK
// signed-Part contract correction (design checkpoint submitted for
// approval); they pin the FAILING behaviors so the SDK re-pin must close
// them.

// TestGeminiMediaAncillaryRepro — videoMetadata and mediaResolution are
// DOCUMENTED ancillary members of inlineData/fileData parts, not arms: the
// current grammar rejects these valid requests.
func TestGeminiMediaAncillaryRepro(t *testing.T) {
	accept := map[string]string{
		"inlineData+videoMetadata": `{"model":"m","contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"video/mp4","data":"AAAA"},"videoMetadata":{"durationOffset":"1s","startOffset":"0s"}}]}]}`,
		// mediaResolution is the OBJECT wire shape pinned from the
		// Vertex-compatible content proto (level member, canonical
		// MEDIA_RESOLUTION_LOW/MEDIUM/HIGH/ULTRA_HIGH spellings) — the
		// earlier invented "720p"/"1080p" strings are NOT the wire grammar.
		"inlineData+mediaResolution":             `{"model":"m","contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBOR"},"mediaResolution":{"level":"MEDIA_RESOLUTION_LOW"}}]}]}`,
		"fileData+videoMetadata+mediaResolution": `{"model":"m","contents":[{"role":"user","parts":[{"fileData":{"fileUri":"gs://b/x.mp4","mimeType":"video/mp4"},"videoMetadata":{"durationOffset":"2s"},"mediaResolution":{"level":"MEDIA_RESOLUTION_ULTRA_HIGH"}}]}]}`,
	}
	for name, body := range accept {
		t.Run("accept/"+name, func(t *testing.T) {
			chat, err := (&Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("valid media part refused: %v", err)
			}
			if _, err := (&Adapter{}).Marshal(chat); err != nil {
				t.Fatalf("valid media part failed to marshal: %v", err)
			}
		})
	}
	refuse := map[string]string{
		"text+videoMetadata": `{"model":"m","contents":[{"role":"user","parts":[{"text":"x","videoMetadata":{"startOffset":"0s"}}]}]}`,
	}
	for name, body := range refuse {
		t.Run("refuse/"+name, func(t *testing.T) {
			if _, err := (&Adapter{}).Unmarshal([]byte(body)); err == nil {
				t.Fatal("ancillary member on a non-media arm accepted")
			}
		})
	}
}

// TestGeminiSchedulingRepro — functionResponse scheduling presence is
// meaningful and vocabulary-governed: absence is the provider default
// WHEN_IDLE (distinct from any explicit value), the three canonical
// spellings are accepted and preserved EXACTLY (structurally decoded, not
// string-matched), an explicit willContinue:false survives as present
// false, and SCHEDULING_UNSPECIFIED behaves EXACTLY like any other unknown
// value — the value-free 400 (never a silent default).
//
// These are ADAPTER-GRAMMAR rows (parse/marshal through the format
// adapter). The REAL-TRANSPORT rows (golden 400, zero hooks/buckets/
// upstream) are TestSchedulingValueFree400Transport in the proxy package;
// the adapter rows alone are not transport proof.
func TestGeminiSchedulingRepro(t *testing.T) {

	// frParts structurally decodes the marshaled wire and returns every
	// functionResponse member of the first message's parts.
	frParts := func(t *testing.T, out []byte) []map[string]any {
		t.Helper()
		var doc map[string]any
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("marshaled output is not decodable JSON: %v", err)
		}
		contents, _ := doc["contents"].([]any)
		if len(contents) == 0 {
			t.Fatalf("no contents in output: %s", out)
		}
		msg, _ := contents[0].(map[string]any)
		parts, _ := msg["parts"].([]any)
		var frs []map[string]any
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if fr, ok := pm["functionResponse"].(map[string]any); ok {
				frs = append(frs, fr)
			}
		}
		if len(frs) == 0 {
			t.Fatalf("no functionResponse parts in output: %s", out)
		}
		return frs
	}
	hasKey := func(m map[string]any, k string) bool {
		_, ok := m[k]
		return ok
	}

	accept := map[string]string{
		"absent (provider default)": `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`,
		"scheduling SILENT":         `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","scheduling":"SILENT"}}]}]}`,
		"scheduling WHEN_IDLE":      `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","scheduling":"WHEN_IDLE"}}]}]}`,
		"scheduling INTERRUPT":      `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","scheduling":"INTERRUPT"}}]}]}`,
		"willContinue explicit":     `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","willContinue":false}}]}]}`,
	}
	for name, body := range accept {
		t.Run("accept/"+name, func(t *testing.T) {
			chat, err := (&Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("valid scheduling combination refused: %v", err)
			}
			out, err := (&Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatalf("valid scheduling combination failed to marshal: %v", err)
			}
			frs := frParts(t, out)
			switch {
			case name == "absent (provider default)":
				// Absence must REMAIN absent: no scheduling/willContinue
				// key anywhere in the marshaled functionResponses.
				for i, fr := range frs {
					if hasKey(fr, "scheduling") || hasKey(fr, "willContinue") {
						t.Fatalf("absent row: presence materialized in output part %d: %v", i, fr)
					}
				}
			case name == "willContinue explicit":
				// Explicit false must remain PRESENT and false.
				found := false
				for _, fr := range frs {
					if v, ok := fr["willContinue"]; ok {
						b, isBool := v.(bool)
						if !isBool || b {
							t.Fatalf("willContinue = %v (%T), want present false", v, v)
						}
						found = true
					}
				}
				if !found {
					t.Fatalf("explicit willContinue:false was dropped: %s", out)
				}
			default:
				// Each explicit scheduling value must remain present and
				// EXACT.
				want := strings.TrimPrefix(name, "scheduling ")
				found := false
				for _, fr := range frs {
					if v, ok := fr["scheduling"]; ok {
						str, isStr := v.(string)
						if !isStr || str != want {
							t.Fatalf("scheduling = %v, want exact %q", v, want)
						}
						found = true
					}
				}
				if !found {
					t.Fatalf("explicit scheduling %q was dropped: %s", want, out)
				}
			}
		})
	}
	refuse := map[string]string{
		"scheduling UNSPECIFIED":   `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","scheduling":"SCHEDULING_UNSPECIFIED"}}]}]}`,
		"scheduling unknown value": `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","scheduling":"WHATEVER"}}]}]}`,
	}
	for name, body := range refuse {
		t.Run("refuse/"+name, func(t *testing.T) {
			if _, err := (&Adapter{}).Unmarshal([]byte(body)); err == nil {
				t.Fatal("unknown scheduling value accepted")
			}
		})
	}
}

// TestGeminiSignedMediaRepro — Google's thought-signature guidance permits
// thoughtSignature on ANY content part, including inlineData; the current
// grammar rejects it (RED) and even when accepted there is no ABI carrier:
// the signature would be stripped from the raw payload and lost.
func TestGeminiSignedMediaRepro(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBOR"},"thoughtSignature":"MEDIA_SIG"}]}]}`
	// FUTURE CONTRACT: thoughtSignature is legal on ANY content part per
	// Google's guidance — a signed media part must be accepted, with the
	// signature projected into the first-class ABI carrier (the SDK
	// correction) and reattached on marshal.
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("signed media part refused: %v", err)
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "MEDIA_SIG") {
		t.Fatalf("the media thoughtSignature was stripped: %s", out)
	}
}

// TestGeminiSignedFunctionResponseRepro — geminiToolResultBlock ignores the
// part's thoughtSignature, so a signed functionResponse is silently
// stripped on the way out: the wire signature never survives a pass.
func TestGeminiSignedFunctionResponseRepro(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"out"},"id":"c1"},"thoughtSignature":"RESP_SIG"}]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("signed functionResponse refused: %v", err)
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "RESP_SIG") {
		t.Log("signed functionResponse currently round-trips (carrier exists?)")
		return
	}
	t.Fatalf("the functionResponse thoughtSignature was stripped: %s", out)
}
