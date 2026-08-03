package gemini

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
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

	if ca, _ := chat.ProviderExtensions[extCodeAssist].(bool); !ca {
		t.Fatal("expected Code Assist marker to be set")
	}
	if chat.Model != "gemini-3.5-flash-extra-low" {
		t.Errorf("model from wrapper not lifted: %q", chat.Model)
	}

	// Messages: system, user, assistant(tool call), tool(result), assistant(text).
	var toolCall *engine.ToolCall
	var toolResult *engine.Message
	var textMsg *engine.Message
	for i := range chat.Messages {
		m := &chat.Messages[i]
		if len(m.ToolCalls) > 0 {
			toolCall = &m.ToolCalls[0]
		}
		if m.Role == engine.RoleTool {
			toolResult = m
		}
		if m.Role == engine.RoleAssistant && m.Content != "" {
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
	if textMsg.Content != "The directory contains two files." {
		t.Errorf("assistant text content = %q, want the fixture's text", textMsg.Content)
	}
	if textMsg.ContentSignature != "SIG_TEXT" {
		t.Errorf("ContentSignature = %q, want SIG_TEXT (signature beside non-thought text)", textMsg.ContentSignature)
	}
	if textMsg.ThinkingSignature != "" {
		t.Errorf("ThinkingSignature = %q, want empty — text-part signature must not use the thinking slot", textMsg.ThinkingSignature)
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

// TestCodeAssistToolResultUnwrapsForCompaction verifies that a Code Assist
// {"output": "<multiline text>"} tool result is stored as raw text (real
// newlines) so line-based compactor plugins can split it, and is re-wrapped as
// {"output": …} on Marshal.
func TestCodeAssistToolResultUnwrapsForCompaction(t *testing.T) {
	body := `{"model":"m","request":{"contents":[
		{"role":"model","parts":[{"functionResponse":{"id":"c1","name":"view_file","response":{"output":"line1\nline2\nline3"}}}]}
	]}}`
	a := &Adapter{}
	chat, err := a.Unmarshal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var tr *engine.Message
	for i := range chat.Messages {
		if chat.Messages[i].Role == engine.RoleTool {
			tr = &chat.Messages[i]
		}
	}
	if tr == nil {
		t.Fatal("no tool result")
	}
	if tr.Content != "line1\nline2\nline3" {
		t.Errorf("tool result not unwrapped to raw text: %q", tr.Content)
	}
	if strings.Count(tr.Content, "\n") != 2 {
		t.Errorf("newlines not preserved for line-splitting: %q", tr.Content)
	}

	// Simulate a compactor rewriting the content, then re-marshal.
	tr.Content = "line2"
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
		ProviderExtensions: map[string]any{
			extCodeAssist:   true,
			extWrapper:      map[string]any{"model": "gemini-3.5-flash"},
			extRequestExtra: map[string]any{},
		},
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "do two things"},
			{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{
				{ID: "a1", Name: "list_dir", Arguments: mustReqArgs(t, `{"p": "/x"}`), Signature: "SIG_FIRST"},
				{ID: "a2", Name: "read_file", Arguments: mustReqArgs(t, `{"f": "y"}`)}, // no signature
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
	if msg.Content != "answer" {
		t.Errorf("content = %q, want the preceding text", msg.Content)
	}
	if msg.TrailingSignature != "SIG" {
		t.Errorf("TrailingSignature = %q, want SIG", msg.TrailingSignature)
	}
	if msg.ThinkingSignature != "" {
		t.Errorf("ThinkingSignature = %q, want empty — trailing signature must not merge into the current block", msg.ThinkingSignature)
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
	if msg.Thinking != "think" {
		t.Errorf("Thinking = %q, want the thought part's text", msg.Thinking)
	}
	if msg.ThinkingSignature != "SIG_A" {
		t.Errorf("ThinkingSignature = %q, want SIG_A (current block)", msg.ThinkingSignature)
	}
	if msg.Content != "answer" {
		t.Errorf("content = %q, want ONLY the text part — thinking must not leak into Content", msg.Content)
	}
	if msg.TrailingSignature != "SIG_B" {
		t.Errorf("TrailingSignature = %q, want SIG_B (trailing standalone)", msg.TrailingSignature)
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
	if re.Content != msg.Content || re.Thinking != msg.Thinking || re.ThinkingSignature != msg.ThinkingSignature || re.TrailingSignature != msg.TrailingSignature {
		t.Errorf("round trip not stable: got {%q, %q, %q, %q}, want {%q, %q, %q, %q}",
			re.Content, re.Thinking, re.ThinkingSignature, re.TrailingSignature, msg.Content, msg.Thinking, msg.ThinkingSignature, msg.TrailingSignature)
	}
}

// TestCodeAssistTrailingSignatureRejectedLeadingStandalone: a signature-only
// part with NO preceding text/thinking in the same content is malformed — it
// would bind nothing (the scope covers only preceding closed content) — so
// Unmarshal rejects it instead of guessing at a binding.
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

// TestCodeAssistRejectedMultipleTextPartsWithSignature: two text parts plus
// any signature in the content are unrepresentable — merging the parts into
// one Content string would replay the signature over text the provider did
// not sign (Google contract: a signature lives on its original Part). This
// covers both a signature on one text part and a trailing standalone part.
func TestCodeAssistRejectedMultipleTextPartsWithSignature(t *testing.T) {
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
			_, err := (&Adapter{}).Unmarshal([]byte(body))
			if err == nil {
				t.Fatal("Unmarshal succeeded, want parse error")
			}
			if !strings.Contains(err.Error(), "gemini: multiple text parts with a signature are not representable") {
				t.Errorf("error = %q, want the multi-text-with-signature condition named", err.Error())
			}
		})
	}
}

// TestCodeAssistRejectedMultipleThinkingPartsWithSignature: two thinking parts
// plus any signature in the content are unrepresentable — the same class of
// error as the multi-text case, for the merged Thinking string.
func TestCodeAssistRejectedMultipleThinkingPartsWithSignature(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"thought":true,"text":"a","thoughtSignature":"S"},
		{"thought":true,"text":"b"}
	]}]}`
	_, err := (&Adapter{}).Unmarshal([]byte(body))
	if err == nil {
		t.Fatal("Unmarshal succeeded, want parse error")
	}
	if !strings.Contains(err.Error(), "gemini: multiple thinking parts with a signature are not representable") {
		t.Errorf("error = %q, want the multi-thinking-with-signature condition named", err.Error())
	}
}

// TestCodeAssistUnsignedTextPartsMerge: multiple text parts with NO signature
// anywhere still merge into Content — the existing unsigned-merge contract.
func TestCodeAssistUnsignedTextPartsMerge(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"text":"a"},
		{"text":"b"}
	]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Content != "a\nb" {
		t.Fatalf("unsigned text parts must merge into one Content: %+v", chat.Messages)
	}
	if chat.Messages[0].ThinkingSignature != "" || chat.Messages[0].TrailingSignature != "" {
		t.Errorf("unsigned merge must not fabricate signatures: %+v", chat.Messages[0])
	}

	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parts := rawModelParts(t, out)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want the single merged text part", len(parts))
	}
	rawText(t, parts[0], "a\nb")
	rawAbsent(t, parts[0], "thoughtSignature")
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
	if msg.Content != "answer" {
		t.Errorf("Content = %q, want the text part's text", msg.Content)
	}
	if msg.ContentSignature != "SIG" {
		t.Errorf("ContentSignature = %q, want SIG (SameMessage scope over content)", msg.ContentSignature)
	}
	if msg.ThinkingSignature != "" {
		t.Errorf("ThinkingSignature = %q, want empty — text-part signature must not use the thinking slot", msg.ThinkingSignature)
	}
	if msg.TrailingSignature != "" {
		t.Errorf("TrailingSignature = %q, want empty — not a standalone part", msg.TrailingSignature)
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
	if msg.Thinking != "think" || msg.ThinkingSignature != "A" {
		t.Errorf("Thinking/ThinkingSignature = {%q, %q}, want {think, A}", msg.Thinking, msg.ThinkingSignature)
	}
	if msg.Content != "answer" || msg.ContentSignature != "B" {
		t.Errorf("Content/ContentSignature = {%q, %q}, want {answer, B}", msg.Content, msg.ContentSignature)
	}
	if msg.TrailingSignature != "C" {
		t.Errorf("TrailingSignature = %q, want C", msg.TrailingSignature)
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
	if re.Content != msg.Content || re.ContentSignature != msg.ContentSignature || re.Thinking != msg.Thinking || re.ThinkingSignature != msg.ThinkingSignature || re.TrailingSignature != msg.TrailingSignature {
		t.Errorf("round trip not stable: got {%q,%q,%q,%q,%q}, want {%q,%q,%q,%q,%q}",
			re.Content, re.ContentSignature, re.Thinking, re.ThinkingSignature, re.TrailingSignature,
			msg.Content, msg.ContentSignature, msg.Thinking, msg.ThinkingSignature, msg.TrailingSignature)
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
			if !strings.Contains(err.Error(), "gemini: standalone signature part must carry explicit empty text") {
				t.Errorf("error = %q, want the explicit-empty-text condition named", err.Error())
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
	if msg.Thinking != "r" || msg.ThinkingSignature != "S" || msg.Content != "" || msg.ContentSignature != "" {
		t.Errorf("got {%q,%q,%q,%q}, want {r,S,<empty>,<empty>}", msg.Thinking, msg.ThinkingSignature, msg.Content, msg.ContentSignature)
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
	if msg.Thinking != "r" || msg.ThinkingSignature != "" || msg.Content != "" || msg.TrailingSignature != "S" {
		t.Errorf("got {%q,%q,%q,%q}, want {r,<empty>,<empty>,S}", msg.Thinking, msg.ThinkingSignature, msg.Content, msg.TrailingSignature)
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
