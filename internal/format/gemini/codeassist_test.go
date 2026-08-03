package gemini

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"google.golang.org/protobuf/proto"
)

// TestCodeAssistRoundTrip parses a real Antigravity CLI Code Assist request and
// verifies the wrapper, tool IDs, thoughtSignatures, and untouched fields
// (toolConfig, thinkingConfig, sessionId) survive Unmarshal→Marshal.
func mustReqArgs(t *testing.T, raw string) engine.RequiredJSONObject {
	t.Helper()
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustExtCA(m map[string]any) engine.OptionalJSONObject {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	r, err := engine.ParseOptionalJSONObject(b)
	if err != nil {
		panic(err)
	}
	return r
}

func TestCodeAssistRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/codeassist-request.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	a := &Adapter{}
	chat, err := a.Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !chat.CodeAssist {
		t.Fatal("expected the typed Code Assist variant to be set")
	}
	if chat.Model != "gemini-3.5-flash-extra-low" {
		t.Errorf("model from wrapper not lifted: %q", chat.Model)
	}

	// Messages: system, user, assistant(tool call), tool(result), assistant(text).
	var toolCall *engine.ToolUseBlock
	var toolResult *engine.ToolResultBlock
	var textMsg *engine.Message
	for i := range chat.Messages {
		m := &chat.Messages[i]
		for _, b := range m.Blocks {
			if b.ToolUse != nil {
				toolCall = b.ToolUse
				break
			}
		}
		if rs := toolResults(*m); len(rs) > 0 {
			toolResult = rs[0]
		}
		if m.Role == engine.RoleAssistant && textOf(*m) != "" {
			textMsg = m
		}
	}
	if toolCall == nil {
		t.Fatal("no tool call parsed")
	}
	if toolCall.ID != "3llkhajj" {
		t.Errorf("tool call id: want 3llkhajj, got %q", toolCall.ID)
	}
	if toolCall.Signature != "SIG_CALL_1" {
		t.Errorf("tool call thoughtSignature lost: %q", toolCall.Signature)
	}
	if toolResult == nil || toolResult.ToolCallID != "3llkhajj" {
		t.Errorf("tool result id not matched to call: %+v", toolResult)
	}
	// contents[3] is a text part carrying a thoughtSignature beside non-thought
	// text — the SameMessage content-bound shape. Round 4 routes it to
	// ContentSignature, never the thinking slot.
	if textMsg == nil {
		t.Fatal("no assistant text message parsed")
	}
	if textOf(*textMsg) != "The directory contains two files." {
		t.Errorf("assistant text content = %q, want the fixture's text", textOf(*textMsg))
	}
	var sig string
	for _, b := range textMsg.Blocks {
		if b.Text != nil {
			sig = b.Text.Signature
		}
	}
	if sig != "SIG_TEXT" {
		t.Errorf("text signature = %q, want SIG_TEXT (signature beside non-thought text)", sig)
	}
	for _, b := range textMsg.Blocks {
		if b.Thinking != nil && b.Thinking.Signature != "" {
			t.Errorf("thinking signature = %q, want empty — text-part signature must not use the thinking slot", b.Thinking.Signature)
		}
	}

	out, err := a.Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("marshal output invalid: %v", err)
	}
	// Wrapper siblings preserved.
	for _, k := range []string{"project", "requestId", "model", "userAgent", "requestType", "request"} {
		if _, ok := top[k]; !ok {
			t.Errorf("wrapper lost key %q", k)
		}
	}
	req, _ := top["request"].(map[string]any)
	if req == nil {
		t.Fatal("request wrapper missing")
	}
	// Untouched inner fields preserved verbatim.
	if _, ok := req["toolConfig"]; !ok {
		t.Error("toolConfig not preserved")
	}
	if _, ok := req["sessionId"]; !ok {
		t.Error("sessionId not preserved")
	}
	gc, _ := req["generationConfig"].(map[string]any)
	if gc == nil || gc["thinkingConfig"] == nil {
		t.Error("generationConfig.thinkingConfig not preserved")
	}

	// Rebuilt contents keep role:model for both call and result, plus id/signature.
	contents, _ := req["contents"].([]any)
	var sawModelCall, sawModelResult, sawTextSig bool
	for _, c := range contents {
		cm := c.(map[string]any)
		parts, _ := cm["parts"].([]any)
		if len(parts) == 0 {
			continue
		}
		p := parts[0].(map[string]any)
		if fc, ok := p["functionCall"].(map[string]any); ok {
			if cm["role"] == "model" && fc["id"] == "3llkhajj" && p["thoughtSignature"] == "SIG_CALL_1" {
				sawModelCall = true
			}
		}
		if fr, ok := p["functionResponse"].(map[string]any); ok {
			if cm["role"] == "model" && fr["id"] == "3llkhajj" {
				sawModelResult = true
			}
		}
		if tv, ok := p["text"].(string); ok && tv == "The directory contains two files." && p["thoughtSignature"] == "SIG_TEXT" {
			sawTextSig = true
		}
	}
	if !sawModelCall {
		t.Error("marshaled functionCall missing role:model / id / thoughtSignature")
	}
	if !sawModelResult {
		t.Error("marshaled functionResponse missing role:model / id")
	}
	if !sawTextSig {
		t.Error("marshaled text part missing thoughtSignature SIG_TEXT beside its text")
	}
}

// TestCodeAssistStreamToolCall parses a real SSE response containing a
// functionCall and asserts id + thoughtSignature capture, then re-serializes.
func TestCodeAssistStreamToolCall(t *testing.T) {
	raw, err := os.ReadFile("testdata/codeassist-stream-toolcall.sse")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := &StreamAdapter{Wrapped: true}
	events := drain(s.ParseStream(strings.NewReader(string(raw))))

	var start *engine.ToolCallStart
	var argsDelta string
	var finish string
	for _, ev := range events {
		switch {
		case ev.ToolCallStart != nil:
			start = ev.ToolCallStart
		case ev.ToolCallDelta != nil:
			argsDelta += ev.ToolCallDelta.ArgumentsDelta
		case ev.FinishReason != "":
			finish = ev.FinishReason
		case ev.Error != nil:
			t.Fatalf("stream error: %s", ev.Error.Message)
		}
	}
	if start == nil {
		t.Fatal("no tool call parsed from SSE")
	}
	if start.ID == "" || start.ID == start.Name {
		t.Errorf("expected real tool-call id, got %q (name %q)", start.ID, start.Name)
	}
	if start.Signature == "" {
		t.Error("thoughtSignature not captured on tool call")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsDelta), &args); err != nil {
		t.Errorf("tool args not valid JSON: %v (%q)", err, argsDelta)
	}
	if finish != "stop" {
		t.Errorf("expected finish stop, got %q", finish)
	}

	// Re-serialize and confirm it round-trips through ParseStream with id+sig intact.
	var buf strings.Builder
	if err := s.SerializeStream(context.Background(), &buf, replay(events)); err != nil {
		t.Fatalf("SerializeStream: %v", err)
	}
	reparsed := drain(s.ParseStream(strings.NewReader(buf.String())))
	var rstart *engine.ToolCallStart
	for _, ev := range reparsed {
		if ev.ToolCallStart != nil {
			rstart = ev.ToolCallStart
		}
	}
	if rstart == nil || rstart.ID != start.ID || rstart.Signature != start.Signature {
		t.Errorf("re-serialized tool call lost id/sig: %+v", rstart)
	}
}

// TestCodeAssistStreamText parses a real text+usage+finish SSE response.
func TestCodeAssistStreamText(t *testing.T) {
	raw, err := os.ReadFile("testdata/codeassist-stream-text.sse")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s := &StreamAdapter{Wrapped: true}
	events := drain(s.ParseStream(strings.NewReader(string(raw))))

	var text strings.Builder
	var usage *engine.StreamUsage
	var finish string
	var sawSig bool
	for _, ev := range events {
		switch {
		case ev.TextDelta != nil:
			text.WriteString(*ev.TextDelta)
		case ev.Usage != nil:
			usage = ev.Usage
		case ev.SignatureDelta != nil:
			sawSig = true
		case ev.FinishReason != "":
			finish = ev.FinishReason
		}
	}
	if text.Len() == 0 {
		t.Error("no text parsed")
	}
	if usage == nil || usage.InputTokens == 0 {
		t.Errorf("usage not parsed: %+v", usage)
	}
	if finish != "stop" {
		t.Errorf("expected finish stop, got %q", finish)
	}
	if !sawSig {
		t.Error("trailing thoughtSignature not captured")
	}
}

// TestCodeAssistToolResultUnwrapsForCompaction verifies the REV 4 §5
// SINGLE-AUTHORITY model on the Code Assist shape: an exact
// {"output": "<multiline>"} response is stored as the RAW response object
// TEXT (the JSON-encoded form, backslash-n escapes intact) — the
// compactor's length/economic gates use the raw JSON baseline (the
// documented size delta vs the old unwrap semantic), and a compactor
// rewrite produces NON-OBJECT text that Marshal re-wraps as
// {"output": …} — a rewrite therefore updates the wire; no stale raw copy
// can win because the text IS the raw.
func TestCodeAssistToolResultUnwrapsForCompaction(t *testing.T) {
	body := `{"model":"m","request":{"contents":[
		{"role":"model","parts":[{"functionResponse":{"id":"c1","name":"view_file","response":{"output":"line1\nline2\nline3"}}}]}
	]}}`
	a := &Adapter{}
	chat, err := a.Unmarshal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var tr *engine.ToolResultBlock
	for i := range chat.Messages {
		if rs := toolResults(chat.Messages[i]); len(rs) > 0 {
			tr = rs[0]
		}
	}
	if tr == nil {
		t.Fatal("no tool result")
	}
	var text string
	for _, c := range tr.Content {
		text += c.Text
	}
	// The raw object text is the SINGLE authority (verbatim, lexeme-exact;
	// JSON-encoded newlines, NOT decoded).
	if text != "{\"output\":\"line1\\nline2\\nline3\"}" {
		t.Errorf("tool result not stored as the raw response object text: %q", text)
	}

	// A compactor rewrite (non-object text) re-wraps on Marshal.
	tr.Content = []engine.ToolResultContentBlock{{Text: "line2"}}
	out, err := a.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	json.Unmarshal(out, &top)
	fr := top["request"].(map[string]any)["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	resp := fr["response"].(map[string]any)
	if resp["output"] != "line2" {
		t.Errorf("tool result not re-wrapped as {output}: %v", resp)
	}
}

// TestCodeAssistParallelCallsShareOneBlock verifies that a turn's parallel tool
// calls marshal into ONE role:model content block (first call keeps its
// signature, the rest legitimately have none) — matching how Gemini emits them.
// Splitting them makes the server 400 with "missing thought_signature".
func TestCodeAssistParallelCallsShareOneBlock(t *testing.T) {
	chat := &engine.ChatRequest{
		CodeAssist:         true,
		ProviderExtensions: mustExtCA(map[string]any{"request": map[string]any{}}),
		Messages: []engine.Message{
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "do two things"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{
				{ToolUse: &engine.ToolUseBlock{ID: "a1", Name: "list_dir", Arguments: mustReqArgs(t, `{"p": "/x"}`), Signature: "SIG_FIRST"}},
				{ToolUse: &engine.ToolUseBlock{ID: "a2", Name: "read_file", Arguments: mustReqArgs(t, `{"f": "y"}`)}}, // no signature
			}},
		},
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	json.Unmarshal(out, &top)
	contents := top["request"].(map[string]any)["contents"].([]any)

	// Find the model content holding the calls; there must be exactly one.
	var callBlocks []map[string]any
	for _, c := range contents {
		cm := c.(map[string]any)
		parts, _ := cm["parts"].([]any)
		hasCall := false
		for _, p := range parts {
			if _, ok := p.(map[string]any)["functionCall"]; ok {
				hasCall = true
			}
		}
		if hasCall {
			callBlocks = append(callBlocks, cm)
		}
	}
	if len(callBlocks) != 1 {
		t.Fatalf("parallel calls must share one content block, got %d blocks", len(callBlocks))
	}
	parts := callBlocks[0]["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("expected 2 call parts in the block, got %d", len(parts))
	}
	p0 := parts[0].(map[string]any)
	p1 := parts[1].(map[string]any)
	if p0["thoughtSignature"] != "SIG_FIRST" {
		t.Errorf("first parallel call lost its signature: %v", p0["thoughtSignature"])
	}
	if _, present := p1["thoughtSignature"]; present {
		t.Errorf("second parallel call should have no signature, got %v", p1["thoughtSignature"])
	}
}

// TestStreamFramingBareVsWrapped locks the one difference between the two
// formats: bare Gemini emits `data: {<chunk>}`, Code Assist wraps in "response".
func TestStreamFramingBareVsWrapped(t *testing.T) {
	mk := func() <-chan engine.StreamEvent {
		ch := make(chan engine.StreamEvent, 2)
		txt := "hi"
		ch <- engine.StreamEvent{TextDelta: &txt}
		ch <- engine.StreamEvent{FinishReason: "stop"}
		close(ch)
		return ch
	}

	var bare strings.Builder
	if err := (&StreamAdapter{Wrapped: false}).SerializeStream(context.Background(), &bare, mk()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bare.String(), `"response"`) {
		t.Errorf("bare gemini frames must NOT wrap in \"response\":\n%s", bare.String())
	}
	if !strings.Contains(bare.String(), `"candidates"`) {
		t.Errorf("bare gemini frames must carry candidates at top level:\n%s", bare.String())
	}

	var wrapped strings.Builder
	if err := (&StreamAdapter{Wrapped: true}).SerializeStream(context.Background(), &wrapped, mk()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped.String(), `"response"`) {
		t.Errorf("code assist frames must wrap in \"response\":\n%s", wrapped.String())
	}

	// Both must re-parse cleanly through the tolerant parser.
	for _, out := range []string{bare.String(), wrapped.String()} {
		evs := drain((&StreamAdapter{}).ParseStream(strings.NewReader(out)))
		var sawText, sawFinish bool
		for _, e := range evs {
			if e.TextDelta != nil && *e.TextDelta == "hi" {
				sawText = true
			}
			if e.FinishReason == "stop" {
				sawFinish = true
			}
		}
		if !sawText || !sawFinish {
			t.Errorf("re-parse lost text/finish for output:\n%s", out)
		}
	}
}

// rawModelParts re-parses marshal output and returns the parts of the first
// role:model content as raw maps. Regressions assert on the RAW WIRE SHAPE
// (key presence included) — never decoded through the lossy geminiPart struct,
// which cannot distinguish an explicit "text":"" from an absent text member.
func rawModelParts(t *testing.T, out []byte) []map[string]any {
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
	if len(contents) != 1 {
		t.Fatalf("contents = %d, want 1 (all parts in the same content)", len(contents))
	}
	cm := contents[0].(map[string]any)
	if cm["role"] != "model" {
		t.Fatalf("role = %v, want model", cm["role"])
	}
	parts, _ := cm["parts"].([]any)
	outParts := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		outParts = append(outParts, p.(map[string]any))
	}
	return outParts
}

// rawText asserts that a raw part carries a text member with the exact value.
func rawText(t *testing.T, p map[string]any, want string) {
	t.Helper()
	v, ok := p["text"]
	if !ok {
		t.Errorf("part %v: missing text member, want %q present", p, want)
		return
	}
	if v != want {
		t.Errorf("part %v: text = %v, want %q", p, v, want)
	}
}

// rawAbsent asserts that a raw part does NOT carry the given key.
func rawAbsent(t *testing.T, p map[string]any, key string) {
	t.Helper()
	if v, ok := p[key]; ok {
		t.Errorf("part %v: unexpected %q = %v, want absent", p, key, v)
	}
}

// TestCodeAssistTrailingSignatureRoundTrip: a model turn whose text is
// followed by Code Assist's trailing signature-only part
// ({"thoughtSignature":<sig>,"text":""}) must land in the message's explicit
// TrailingSignature slot (SignatureScopeTrailingStandalone — it binds the
// preceding closed text content, never the current-block ThinkingSignature),
// and re-marshal must reproduce the exact two-part topology in one content:
// the signature stays its own final empty-text part with the EXPLICIT "text":""
// arm the provider emits, never merged into text and never a bare
// {"thoughtSignature":…}.
func TestCodeAssistTrailingSignatureRoundTrip(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"text":"answer"},
		{"text":"","thoughtSignature":"SIG"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(chat.Messages))
	}
	msg := chat.Messages[0]
	if msg.Role != engine.RoleAssistant {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if textOf(msg) != "answer" {
		t.Errorf("content = %q, want the preceding text", textOf(msg))
	}
	var trail string
	for _, b := range msg.Blocks {
		if b.TrailingSignature != nil {
			trail = b.TrailingSignature.Signature
		}
	}
	if trail != "SIG" {
		t.Errorf("TrailingSignature = %q, want SIG", trail)
	}
	for _, b := range msg.Blocks {
		if b.Thinking != nil && b.Thinking.Signature != "" {
			t.Errorf("thinking signature = %q, want empty — trailing signature must not merge into the current block", b.Thinking.Signature)
		}
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (text + explicit empty-text trailing signature)", len(parts))
	}
	rawText(t, parts[0], "answer")
	rawAbsent(t, parts[0], "thoughtSignature")
	rawText(t, parts[1], "") // EXPLICIT empty text member, not an absent one
	if parts[1]["thoughtSignature"] != "SIG" {
		t.Errorf("trailing part thoughtSignature = %v, want SIG", parts[1]["thoughtSignature"])
	}
}

// TestCodeAssistTrailingSignatureRoundTripWithThinking: the realistic Code
// Assist thinking shape — a thought part (thought:true) carrying the
// current-block signature, a plain text part, and the trailing standalone
// signature — maps to distinct message slots (Thinking/ThinkingSignature,
// Content, TrailingSignature) and re-marshals to the SAME three-part topology
// in the pinned order thinking, text, trailing, with the trailing part's
// explicit "text":"". The re-parse of that output reproduces the exact same
// message (stable round trip).
func TestCodeAssistTrailingSignatureRoundTripWithThinking(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"thought":true,"text":"think","thoughtSignature":"SIG_A"},
		{"text":"answer"},
		{"text":"","thoughtSignature":"SIG_B"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(chat.Messages))
	}
	msg := chat.Messages[0]
	var thinking, thinkSig, text string
	for _, b := range msg.Blocks {
		switch {
		case b.Thinking != nil:
			thinking = b.Thinking.Text
			thinkSig = b.Thinking.Signature
		case b.Text != nil:
			text = b.Text.Text
		}
	}
	if thinking != "think" {
		t.Errorf("thinking = %q, want the thought part's text", thinking)
	}
	if thinkSig != "SIG_A" {
		t.Errorf("ThinkingSignature = %q, want SIG_A (current block)", thinkSig)
	}
	if text != "answer" {
		t.Errorf("content = %q, want ONLY the text part — thinking must not leak into Content", text)
	}
	var trail string
	for _, b := range msg.Blocks {
		if b.TrailingSignature != nil {
			trail = b.TrailingSignature.Signature
		}
	}
	if trail != "SIG_B" {
		t.Errorf("TrailingSignature = %q, want SIG_B (trailing standalone)", trail)
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (thinking, text, trailing)", len(parts))
	}
	// Part 0: thinking, first, with its current-block signature.
	if parts[0]["thought"] != true {
		t.Errorf("parts[0] thought = %v, want true", parts[0]["thought"])
	}
	rawText(t, parts[0], "think")
	if parts[0]["thoughtSignature"] != "SIG_A" {
		t.Errorf("parts[0] thoughtSignature = %v, want SIG_A", parts[0]["thoughtSignature"])
	}
	// Part 1: the plain text part, carrying NO signature.
	rawText(t, parts[1], "answer")
	rawAbsent(t, parts[1], "thoughtSignature")
	rawAbsent(t, parts[1], "thought")
	// Part 2: the trailing standalone with its explicit empty text arm.
	rawText(t, parts[2], "")
	if parts[2]["thoughtSignature"] != "SIG_B" {
		t.Errorf("parts[2] thoughtSignature = %v, want SIG_B", parts[2]["thoughtSignature"])
	}

	again, err := (&Adapter{}).Unmarshal(out)
	if err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if len(again.Messages) != 1 {
		t.Fatalf("re-unmarshal messages = %d, want 1", len(again.Messages))
	}
	re := again.Messages[0]
	if textOf(re) != textOf(msg) || reBlocks(re) != reBlocks(msg) {
		t.Errorf("round trip not stable: %s vs %s", reBlocks(re), reBlocks(msg))
	}
}

// TestCodeAssistTrailingSignatureRejectedLeadingStandalone: a signature-only
// part with NO preceding text/thinking in the same content is malformed — it
// would bind nothing (the scope covers only preceding closed content) — so
// Unmarshal rejects it instead of guessing at a binding.
func reBlocks(m engine.Message) string {
	var out []string
	for _, b := range m.Blocks {
		switch {
		case b.Text != nil:
			out = append(out, "text:"+b.Text.Text+":"+b.Text.Signature)
		case b.Thinking != nil:
			out = append(out, "think:"+b.Thinking.Text+":"+b.Thinking.Signature)
		case b.TrailingSignature != nil:
			out = append(out, "trail:"+b.TrailingSignature.Signature)
		case b.ToolUse != nil:
			out = append(out, "call:"+b.ToolUse.ID)
		case b.ToolResult != nil:
			out = append(out, "result:"+b.ToolResult.ToolCallID)
		}
	}
	return strings.Join(out, "|")
}

func TestCodeAssistTrailingSignatureRejectedLeadingStandalone(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"thoughtSignature":"SIG_LEAD","text":""},
		{"text":"the actual reply"}
	]}]}`
	_, err := (&Adapter{}).Unmarshal([]byte(body))
	if err == nil {
		t.Fatal("Unmarshal succeeded, want parse error")
	}
	if !strings.Contains(err.Error(), "gemini: trailing signature without preceding text/thinking content") {
		t.Errorf("error = %q, want the leading-standalone condition named", err.Error())
	}
}

// TestCodeAssistTrailingSignatureRejectedOnToolCallOnlyTurn: a signature-only
// part in a content with NO text/thinking at all (a tool-call-only turn) is
// malformed — SignatureScopeTrailingStandalone does not bind tool-call blocks
// — so Unmarshal rejects it rather than dropping or misbinding it.
func TestCodeAssistTrailingSignatureRejectedOnToolCallOnlyTurn(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"functionCall":{"name":"list_dir","args":{"p":"/x"},"id":"c1"}},
		{"thoughtSignature":"SIG_ORPHAN","text":""}
	]}]}`
	_, err := (&Adapter{}).Unmarshal([]byte(body))
	if err == nil {
		t.Fatal("Unmarshal succeeded, want parse error")
	}
	if !strings.Contains(err.Error(), "gemini: standalone signature on a tool-call-only turn") {
		t.Errorf("error = %q, want the tool-call-only-turn condition named", err.Error())
	}
}

// TestCodeAssistTrailingSignatureRejectedNotFinal: a signature-only part with
// parts AFTER it (text,sig,text) can bind nothing — the trailing slot is final
// by contract — so Unmarshal rejects it as non-final instead of silently
// moving the signature over the following text.
func TestCodeAssistTrailingSignatureRejectedNotFinal(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"text":"a"},
		{"text":"","thoughtSignature":"S"},
		{"text":"b"}
	]}]}`
	_, err := (&Adapter{}).Unmarshal([]byte(body))
	if err == nil {
		t.Fatal("Unmarshal succeeded, want parse error")
	}
	if !strings.Contains(err.Error(), "gemini: standalone signature is not the final part") {
		t.Errorf("error = %q, want the non-final condition named", err.Error())
	}
}

// TestCodeAssistTrailingSignatureRejectedDuplicate: a second signature-only
// part is never representable — the trailing slot is singular — whether it
// follows text (text,sigA,sigB) or another standalone (sig-only,sig-only).
func TestCodeAssistTrailingSignatureRejectedDuplicate(t *testing.T) {
	for name, body := range map[string]string{
		"after text": `{"contents":[{"role":"model","parts":[
			{"text":"a"},
			{"text":"","thoughtSignature":"SIG_A"},
			{"text":"","thoughtSignature":"SIG_B"}
		]}]}`,
		"sig-only pair": `{"contents":[{"role":"model","parts":[
			{"text":"","thoughtSignature":"SIG_A"},
			{"text":"","thoughtSignature":"SIG_B"}
		]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&Adapter{}).Unmarshal([]byte(body))
			if err == nil {
				t.Fatal("Unmarshal succeeded, want parse error")
			}
			if !strings.Contains(err.Error(), "gemini: duplicate standalone signature") {
				t.Errorf("error = %q, want the duplicate-standalone condition named", err.Error())
			}
		})
	}
}

// TestCodeAssistMultipleTextPartsWithSignature: per-block signatures are
// first-class in the ordered body — a signature lives on ITS text part, so
// multiple text parts with a signature on one of them round-trip exactly
// (the ordered ABI replaced the single flat Content slot these cases were
// rejected under). A trailing standalone after several text parts binds the
// whole preceding covered content.
func TestCodeAssistMultipleTextPartsWithSignature(t *testing.T) {
	for name, body := range map[string]string{
		"sig on text part": `{"contents":[{"role":"model","parts":[
			{"text":"a","thoughtSignature":"S"},
			{"text":"b"}
		]}]}`,
		"trailing standalone": `{"contents":[{"role":"model","parts":[
			{"text":"a"},
			{"text":"b"},
			{"text":"","thoughtSignature":"S"}
		]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			chat, err := (&Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			out, err := (&Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			again, err := (&Adapter{}).Unmarshal(out)
			if err != nil {
				t.Fatalf("re-Unmarshal: %v", err)
			}
			if len(again.Messages) != 1 || reBlocks(again.Messages[0]) != reBlocks(chat.Messages[0]) {
				t.Fatalf("round trip not stable: %s", out)
			}
		})
	}
}

// TestCodeAssistMultipleThinkingPartsWithSignature: per-block signatures make
// two signed thinking parts representable — each Thinking block keeps its own
// current-block token and the parts round-trip in order.
func TestCodeAssistMultipleThinkingPartsWithSignature(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"thought":true,"text":"a","thoughtSignature":"S"},
		{"thought":true,"text":"b"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	again, err := (&Adapter{}).Unmarshal(out)
	if err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if len(again.Messages) != 1 || reBlocks(again.Messages[0]) != reBlocks(chat.Messages[0]) {
		t.Fatalf("round trip not stable: %s", out)
	}
}

// TestCodeAssistUnsignedTextParts: multiple unsigned text parts stay
// SEPARATE ordered blocks — the ordered body IS the wire order, and merging
// would invent a part boundary the provider never sent. The marshal re-emits
// one part per block, byte-stable.
func TestCodeAssistUnsignedTextParts(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"text":"a"},
		{"text":"b"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 || len(chat.Messages[0].Blocks) != 2 {
		t.Fatalf("unsigned text parts must keep their part boundaries: %+v", chat.Messages)
	}
	for _, b := range chat.Messages[0].Blocks {
		if (b.Text != nil && b.Text.Signature != "") || b.TrailingSignature != nil {
			t.Errorf("unsigned parts must not fabricate signatures: %+v", chat.Messages[0])
		}
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want the two original text parts", len(parts))
	}
	rawText(t, parts[0], "a")
	rawText(t, parts[1], "b")
	rawAbsent(t, parts[0], "thoughtSignature")
	rawAbsent(t, parts[1], "thoughtSignature")
}

// TestCodeAssistRejectedMixedTextThinkingWithToolCall: a role:"model" content
// mixing text/thinking parts with functionCall/functionResponse parts is
// rejected outright — buildContents splits tool calls into their OWN model
// contents for Code Assist, so the ordered mixed shape (text, functionCall,
// trailingSig) cannot round-trip. Unconditional per reviewer ruling; no
// fixture relies on the mixed shape.
func TestCodeAssistRejectedMixedTextThinkingWithToolCall(t *testing.T) {
	for name, body := range map[string]string{
		"text + trailing + call": `{"contents":[{"role":"model","parts":[
			{"text":"answer"},
			{"functionCall":{"name":"list_dir","args":{"p":"/x"},"id":"c1"}},
			{"text":"","thoughtSignature":"SIG"}
		]}]}`,
		"thinking + call": `{"contents":[{"role":"model","parts":[
			{"thought":true,"text":"reason"},
			{"functionCall":{"name":"list_dir","args":{"p":"/x"},"id":"c1"}}
		]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&Adapter{}).Unmarshal([]byte(body))
			if err == nil {
				t.Fatal("Unmarshal succeeded, want parse error")
			}
			if !strings.Contains(err.Error(), "gemini: mixed text/thinking and tool-call parts in one content are not representable") {
				t.Errorf("error = %q, want the mixed-contents condition named", err.Error())
			}
		})
	}
}

// TestCodeAssistContentSignatureRoundTrip: a non-thought text part carrying a
// thoughtSignature maps to the ContentSignature slot (SameMessage scope over
// the merged Content — NOT ThinkingSignature, whose SDK binding covers only
// thinking/redacted_thinking blocks) and re-marshals to the exact same single
// part with the signature beside the text.
func textSigOf(m engine.Message) string {
	for _, b := range m.Blocks {
		if b.Text != nil {
			return b.Text.Signature
		}
	}
	return ""
}

func thinkOf(m engine.Message) string {
	for _, b := range m.Blocks {
		if b.Thinking != nil {
			return b.Thinking.Text
		}
	}
	return ""
}

func thinkSigOf(m engine.Message) string {
	for _, b := range m.Blocks {
		if b.Thinking != nil {
			return b.Thinking.Signature
		}
	}
	return ""
}

func trailOf(m engine.Message) string {
	for _, b := range m.Blocks {
		if b.TrailingSignature != nil {
			return b.TrailingSignature.Signature
		}
	}
	return ""
}

func TestCodeAssistContentSignatureRoundTrip(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"text":"answer","thoughtSignature":"SIG"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(chat.Messages))
	}
	msg := chat.Messages[0]
	if textOf(msg) != "answer" {
		t.Errorf("Content = %q, want the text part's text", textOf(msg))
	}
	var textSig, thinkSig, trailSig string
	for _, b := range msg.Blocks {
		switch {
		case b.Text != nil:
			textSig = b.Text.Signature
		case b.Thinking != nil:
			thinkSig = b.Thinking.Signature
		case b.TrailingSignature != nil:
			trailSig = b.TrailingSignature.Signature
		}
	}
	if textSig != "SIG" {
		t.Errorf("ContentSignature = %q, want SIG (SameMessage scope over content)", textSig)
	}
	if thinkSig != "" {
		t.Errorf("ThinkingSignature = %q, want empty — text-part signature must not use the thinking slot", thinkSig)
	}
	if trailSig != "" {
		t.Errorf("TrailingSignature = %q, want empty — not a standalone part", trailSig)
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	rawText(t, parts[0], "answer")
	if parts[0]["thoughtSignature"] != "SIG" {
		t.Errorf("thoughtSignature = %v, want SIG on the text part", parts[0]["thoughtSignature"])
	}
	rawAbsent(t, parts[0], "thought")
}

// TestCodeAssistContentSignatureCombinedShape: the full signed topology — a
// thought part with its current-block signature, a text part with its
// content-bound signature, and the trailing standalone — maps to three DISTINCT
// message slots (ThinkingSignature, ContentSignature, TrailingSignature) and
// re-marshals to the same three parts in the pinned order thinking, text,
// trailing. The re-parse of that output reproduces the exact same message.
func TestCodeAssistContentSignatureCombinedShape(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"thought":true,"text":"think","thoughtSignature":"A"},
		{"text":"answer","thoughtSignature":"B"},
		{"text":"","thoughtSignature":"C"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(chat.Messages))
	}
	msg := chat.Messages[0]
	var think, thinkSig, textSig, trailSig string
	for _, b := range msg.Blocks {
		switch {
		case b.Thinking != nil:
			think = b.Thinking.Text
			thinkSig = b.Thinking.Signature
		case b.Text != nil:
			textSig = b.Text.Signature
		case b.TrailingSignature != nil:
			trailSig = b.TrailingSignature.Signature
		}
	}
	if think != "think" || thinkSig != "A" {
		t.Errorf("Thinking/ThinkingSignature = {%q, %q}, want {think, A}", think, thinkSig)
	}
	if textOf(msg) != "answer" || textSig != "B" {
		t.Errorf("Content/ContentSignature = {%q, %q}, want {answer, B}", textOf(msg), textSig)
	}
	if trailSig != "C" {
		t.Errorf("TrailingSignature = %q, want C", trailSig)
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (thinking, text, trailing)", len(parts))
	}
	// Part 0: thinking, first, with its current-block signature.
	if parts[0]["thought"] != true {
		t.Errorf("parts[0] thought = %v, want true", parts[0]["thought"])
	}
	rawText(t, parts[0], "think")
	if parts[0]["thoughtSignature"] != "A" {
		t.Errorf("parts[0] thoughtSignature = %v, want A", parts[0]["thoughtSignature"])
	}
	// Part 1: the text part with its content-bound signature.
	rawText(t, parts[1], "answer")
	if parts[1]["thoughtSignature"] != "B" {
		t.Errorf("parts[1] thoughtSignature = %v, want B", parts[1]["thoughtSignature"])
	}
	rawAbsent(t, parts[1], "thought")
	// Part 2: the trailing standalone with its explicit empty text arm.
	rawText(t, parts[2], "")
	if parts[2]["thoughtSignature"] != "C" {
		t.Errorf("parts[2] thoughtSignature = %v, want C", parts[2]["thoughtSignature"])
	}

	again, err := (&Adapter{}).Unmarshal(out)
	if err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if len(again.Messages) != 1 {
		t.Fatalf("re-unmarshal messages = %d, want 1", len(again.Messages))
	}
	re := again.Messages[0]
	if reBlocks(re) != reBlocks(msg) {
		t.Errorf("round trip not stable: %s vs %s", reBlocks(re), reBlocks(msg))
	}
}

// TestCodeAssistRejectedBareSignaturePart: a part with a thoughtSignature but
// NO "text" key at all is not the supported trailing shape — the
// TrailingStandalone contract requires the EXPLICIT empty-text arm — so
// Unmarshal rejects it instead of guessing at a binding.
func TestCodeAssistRejectedBareSignaturePart(t *testing.T) {
	for name, body := range map[string]string{
		"lone bare": `{"contents":[{"role":"model","parts":[
			{"thoughtSignature":"S"}
		]}]}`,
		"bare after text": `{"contents":[{"role":"model","parts":[
			{"text":"answer"},
			{"thoughtSignature":"S"}
		]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&Adapter{}).Unmarshal([]byte(body))
			if err == nil {
				t.Fatal("Unmarshal succeeded, want parse error")
			}
			if !strings.Contains(err.Error(), "arm members") && !strings.Contains(err.Error(), "explicit empty text") {
				t.Errorf("error = %q, want the arm-grammar condition named", err.Error())
			}
		})
	}
}

// TestCodeAssistThinkingOnlyNoInventedTextPart: a thinking-only turn must
// marshal to EXACTLY ONE part (the thought part) — round 3 invented an empty
// {} text part for Content-less turns, which Code Assist rejects.
func TestCodeAssistThinkingOnlyNoInventedTextPart(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"thought":true,"text":"r","thoughtSignature":"S"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(chat.Messages))
	}
	msg := chat.Messages[0]
	if thinkOf(msg) != "r" || thinkSigOf(msg) != "S" || textOf(msg) != "" || textSigOf(msg) != "" {
		t.Errorf("got {%q,%q,%q,%q}, want {r,S,<empty>,<empty>}", thinkOf(msg), thinkSigOf(msg), textOf(msg), textSigOf(msg))
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want EXACTLY 1 (no invented empty text part)", len(parts))
	}
	if parts[0]["thought"] != true {
		t.Errorf("parts[0] thought = %v, want true", parts[0]["thought"])
	}
	rawText(t, parts[0], "r")
	if parts[0]["thoughtSignature"] != "S" {
		t.Errorf("parts[0] thoughtSignature = %v, want S", parts[0]["thoughtSignature"])
	}
}

// TestCodeAssistThinkingTrailingNoContent: a thinking part plus a trailing
// standalone with NO text content in between must marshal to exactly two parts
// (thinking, trailing) — the text part is not invented and the trailing
// signature stays its own explicit empty-text part.
func TestCodeAssistThinkingTrailingNoContent(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"thought":true,"text":"r"},
		{"text":"","thoughtSignature":"S"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(chat.Messages))
	}
	msg := chat.Messages[0]
	if thinkOf(msg) != "r" || thinkSigOf(msg) != "" || textOf(msg) != "" || trailOf(msg) != "S" {
		t.Errorf("got {%q,%q,%q,%q}, want {r,<empty>,<empty>,S}", thinkOf(msg), thinkSigOf(msg), textOf(msg), trailOf(msg))
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (thinking, trailing)", len(parts))
	}
	if parts[0]["thought"] != true {
		t.Errorf("parts[0] thought = %v, want true", parts[0]["thought"])
	}
	rawText(t, parts[0], "r")
	rawText(t, parts[1], "")
	if parts[1]["thoughtSignature"] != "S" {
		t.Errorf("parts[1] thoughtSignature = %v, want S", parts[1]["thoughtSignature"])
	}
}

// TestStreamSerializeSignatureDeltaExplicitEmptyText: the stream serializer's
// standalone SignatureDelta frame must carry the provider's EXPLICIT
// {"text":"","thoughtSignature":…} shape — a bare {"thoughtSignature":…}
// would drop the text arm. Asserted on the raw frame JSON.
func TestStreamSerializeSignatureDeltaExplicitEmptyText(t *testing.T) {
	s := &StreamAdapter{}
	ch := make(chan engine.StreamEvent, 2)
	sig := "SIG"
	ch <- engine.StreamEvent{SignatureDelta: &sig}
	ch <- engine.StreamEvent{FinishReason: "stop"}
	close(ch)

	var buf strings.Builder
	if err := s.SerializeStream(context.Background(), &buf, ch); err != nil {
		t.Fatalf("SerializeStream: %v", err)
	}

	var found bool
	for _, block := range strings.Split(buf.String(), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		data, ok := strings.CutPrefix(block, "data:")
		if !ok {
			t.Fatalf("frame missing data: prefix: %q", block)
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &frame); err != nil {
			t.Fatalf("frame not valid JSON: %v (%q)", err, block)
		}
		cands, _ := frame["candidates"].([]any)
		if len(cands) == 0 {
			continue
		}
		content, _ := cands[0].(map[string]any)["content"].(map[string]any)
		if content == nil {
			continue
		}
		parts, _ := content["parts"].([]any)
		for _, p := range parts {
			pm := p.(map[string]any)
			if pm["thoughtSignature"] != "SIG" {
				continue
			}
			found = true
			tv, tok := pm["text"]
			if !tok || tv != "" {
				t.Errorf("signature part missing explicit empty text arm: %v", pm)
			}
		}
	}
	if !found {
		t.Fatalf("no standalone signature part serialized:\n%s", buf.String())
	}
}

func drain(ch <-chan engine.StreamEvent) []engine.StreamEvent {
	var out []engine.StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func replay(events []engine.StreamEvent) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

// TestCodeAssistOverlayCanonicalWins — the 4b overlay: canonical fields
// control the emitted wire (outer model; inner generationConfig members,
// safetySettings, systemInstruction/contents/tools) while unknown
// wrapper/inner siblings pass through losslessly at their exact scopes.
func TestCodeAssistOverlayCanonicalWins(t *testing.T) {
	maxTok := 2048
	temp := 0.7
	topP := 0.9
	safety, _ := engine.ParseOptionalJSONArray([]byte(`[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]`))
	chat := &engine.ChatRequest{
		Model:       "gemini-3.5-flash-extra-low",
		MaxTokens:   &maxTok,
		Temperature: &temp,
		TopP:        &topP,
		CodeAssist:  true,
		ProviderExtensions: mustExtCA(map[string]any{
			"project": "my-project", // wrapper extra
			"request": map[string]any{
				"sessionId": "s-1", // inner extra
				"generationConfig": map[string]any{
					"candidateCount": 3, // unknown generation sibling (canonical members are stripped at parse)
				},
			},
		}),
		SafetySettings: safety,
		Messages: []engine.Message{
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}},
		},
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	// Canonical outer model wins.
	if top["model"] != "gemini-3.5-flash-extra-low" {
		t.Fatalf("canonical model lost: %v", top["model"])
	}
	// Wrapper extra survives at the top scope.
	if top["project"] != "my-project" {
		t.Fatalf("wrapper extra lost: %v", top["project"])
	}
	req := top["request"].(map[string]any)
	if req["sessionId"] != "s-1" {
		t.Fatalf("inner extra lost: %v", req["sessionId"])
	}
	gc := req["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(2048) {
		t.Fatalf("canonical maxOutputTokens not rebuilt: %v", gc["maxOutputTokens"])
	}
	if gc["temperature"] != 0.7 || gc["topP"] != 0.9 {
		t.Fatalf("canonical generation members not rebuilt: %v", gc)
	}
	if gc["candidateCount"] != float64(3) {
		t.Fatalf("unknown generation sibling lost: %v", gc["candidateCount"])
	}
	if len(gc) != 4 {
		t.Fatalf("generationConfig = %v, want exactly the 3 canonical + 1 sibling", gc)
	}
	if _, ok := req["safetySettings"]; !ok {
		t.Fatalf("canonical safetySettings not rebuilt: %v", req)
	}
}

// TestCodeAssistEnvelopeSmugglingRefused — a replacement envelope that
// smuggles canonical members through the extras path is REFUSED at
// marshal (normal plugin failure mode), never silently ignored.
func TestCodeAssistEnvelopeSmugglingRefused(t *testing.T) {
	rows := map[string]map[string]any{
		"outer model": {
			"model":   "evil-model",
			"request": map[string]any{},
		},
		"inner contents": {
			"request": map[string]any{"contents": []any{}},
		},
		"inner tools": {
			"request": map[string]any{"tools": []any{}},
		},
		"inner safetySettings": {
			"request": map[string]any{"safetySettings": []any{}},
		},
		"inner systemInstruction": {
			"request": map[string]any{"systemInstruction": map[string]any{}},
		},
		"generation canonical": {
			"request": map[string]any{"generationConfig": map[string]any{"maxOutputTokens": 1}},
		},
	}
	for name, env := range rows {
		t.Run(name, func(t *testing.T) {
			chat := &engine.ChatRequest{
				Model:              "m",
				CodeAssist:         true,
				ProviderExtensions: mustExtCA(env),
				Messages: []engine.Message{
					{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}},
				},
			}
			if _, err := (&Adapter{}).Marshal(chat); err == nil {
				t.Fatalf("smuggled canonical member %q accepted", name)
			}
		})
	}
}

// TestMarshalCodeAssistPure — marshal never mutates the input: the cache
// key before and after is identical, repeated marshal is byte-identical,
// and an ABSENT envelope marshals without materializing anything on the
// chat. A PRESENT envelope missing the structural `request` member is
// REFUSED.
func TestMarshalCodeAssistPure(t *testing.T) {
	chat := &engine.ChatRequest{
		Model:      "m",
		CodeAssist: true,
		Messages:   []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	out1, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	if !chat.ProviderExtensions.IsAbsent() {
		t.Fatalf("marshal materialized an envelope on the input: %q", chat.ProviderExtensions.Bytes())
	}
	out2, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out2) {
		t.Fatal("repeated marshal is not byte-identical")
	}

	// Present `{}` is refused (request is structural).
	empty, _ := engine.ParseOptionalJSONObject([]byte(`{}`))
	chat.ProviderExtensions = empty
	if _, err := (&Adapter{}).Marshal(chat); err == nil {
		t.Fatal("a present {} envelope must be refused")
	}

	// Cache identity: pb conversion before/after marshal is unchanged.
	chat2 := &engine.ChatRequest{
		Model: "m", CodeAssist: true,
		ProviderExtensions: mustExtCA(map[string]any{"project": "p", "request": map[string]any{}}),
		Messages:           []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	before, err := pbconv.ToPBChatRequestChecked(chat2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Adapter{}).Marshal(chat2); err != nil {
		t.Fatal(err)
	}
	after, err := pbconv.ToPBChatRequestChecked(chat2)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(before, after) {
		t.Fatal("marshal mutated the request (cache identity changed)")
	}
}

// TestCodeAssistEnvelopeInvalidShapes — null, arrays, scalars, malformed
// text, and empty objects for `request` and `generationConfig` are
// classified errors with NO panic, on accepted input and on a
// plugin-shaped replacement envelope.
func TestCodeAssistEnvelopeInvalidShapes(t *testing.T) {
	shapes := map[string]string{
		"request null":               `{"request":null}`,
		"request array":              `{"request":[1]}`,
		"request scalar":             `{"request":5}`,
		"request malformed":          `{"request":"{oops"}`,
		"generationConfig null":      `{"request":{"generationConfig":null}}`,
		"generationConfig array":     `{"request":{"generationConfig":[1]}}`,
		"generationConfig scalar":    `{"request":{"generationConfig":"x"}}`,
		"generationConfig malformed": `{"request":{"generationConfig":"{oops"}}`,
	}
	for name, raw := range shapes {
		t.Run(name, func(t *testing.T) {
			env, err := engine.ParseOptionalJSONObject([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			chat := &engine.ChatRequest{
				Model: "m", CodeAssist: true, ProviderExtensions: env,
				Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
			}
			// Must be a CLASSIFIED error, never a panic.
			if _, err := (&Adapter{}).Marshal(chat); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}

// TestGenerationSiblingLossless — unknown generationConfig siblings keep
// their EXACT lexemes (member order, whitespace, numeric spellings,
// escapes, nested bytes) across input projection and the final wire. The
// body is a VALID wrapped Code Assist request (contents inside request).
func TestGenerationSiblingLossless(t *testing.T) {
	// Nonalphabetical order, 1e999, 1.0, escaped Unicode, whitespace, a
	// nested object — inside a valid wrapped request.
	body := `{"model":"m","request":{"generationConfig":{"z":1e999,"a":1.0,"u":"\u0041bc","w": 42 ,"nested":{"k":[1,2]},"m":"x"},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !chat.CodeAssist {
		t.Fatal("variant lost")
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Blocks[0].Text.Text != "hi" {
		t.Fatalf("no message parsed from the wrapped request (contents must live INSIDE request): %+v", chat.Messages)
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	// The COMPLETE preserved generationConfig must appear EXACTLY
	// (members in their original order, whitespace and lexemes intact) —
	// a single substring assertion, not independent fragments.
	preserved := `{"z":1e999,"a":1.0,"u":"\u0041bc","w": 42 ,"nested":{"k":[1,2]},"m":"x"}`
	if !strings.Contains(string(out), preserved) {
		t.Fatalf("preserved generationConfig not lexeme-exact on the wire:\nwant %s\n got %s", preserved, out)
	}
}

// TestGenerationConfigAbsentVsEmpty — the settled rule: input projection
// REMOVES generationConfig when no unknown sibling remains (canonical-only
// input never leaks a derived {}); an EXPLICITLY EMPTY wire object is
// preserved; unknown-only and canonical+unknown keep the siblings; the
// marshal overlays canonical members without disturbing any of them.
func TestGenerationConfigAbsentVsEmpty(t *testing.T) {
	rows := map[string]struct {
		body     string
		wantKind string // "absent" | "empty" | "siblings"
	}{
		"canonical only": {
			`{"model":"m","request":{"generationConfig":{"maxOutputTokens":100,"temperature":0.5},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
			"absent",
		},
		"wire empty": {
			`{"model":"m","request":{"generationConfig":{},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
			"empty",
		},
		"unknown only": {
			`{"model":"m","request":{"generationConfig":{"candidateCount":3},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
			"siblings",
		},
		"canonical + unknown": {
			`{"model":"m","request":{"generationConfig":{"maxOutputTokens":100,"candidateCount":3},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
			"siblings",
		},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			chat, err := (&Adapter{}).Unmarshal([]byte(row.body))
			if err != nil {
				t.Fatal(err)
			}
			// The parsed envelope's inner scope decides the projection
			// outcome BEFORE any marshal.
			m, _, _ := chat.ProviderExtensions.DecodeObject()
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(m["request"], &inner); err != nil {
				t.Fatal(err)
			}
			gcRaw, present := inner["generationConfig"]
			switch row.wantKind {
			case "absent":
				if present {
					t.Fatalf("canonical-only input leaked generationConfig: %s", gcRaw)
				}
			case "empty":
				if !present || string(gcRaw) != "{}" {
					t.Fatalf("explicit empty must be preserved as {}: %s", gcRaw)
				}
			case "siblings":
				if !present || !strings.Contains(string(gcRaw), "candidateCount") {
					t.Fatalf("siblings lost: %s", gcRaw)
				}
				if strings.Contains(string(gcRaw), "maxOutputTokens") {
					t.Fatalf("canonical member survived the projection: %s", gcRaw)
				}
			}
			// Marshal round-trips without disturbing the outcome.
			out, err := (&Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatal(err)
			}
			if row.wantKind == "siblings" && !strings.Contains(string(out), "candidateCount") {
				t.Fatalf("sibling lost on the wire: %s", out)
			}
		})
	}
}
