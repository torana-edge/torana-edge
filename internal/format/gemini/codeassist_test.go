package gemini

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestCodeAssistRoundTrip parses a real Antigravity CLI Code Assist request and
// verifies the wrapper, tool IDs, thoughtSignatures, and untouched fields
// (toolConfig, thinkingConfig, sessionId) survive Unmarshal→Marshal.
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
	for i := range chat.Messages {
		m := &chat.Messages[i]
		if len(m.ToolCalls) > 0 {
			toolCall = &m.ToolCalls[0]
		}
		if m.Role == engine.RoleTool {
			toolResult = m
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
	var sawModelCall, sawModelResult bool
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
	}
	if !sawModelCall {
		t.Error("marshaled functionCall missing role:model / id / thoughtSignature")
	}
	if !sawModelResult {
		t.Error("marshaled functionResponse missing role:model / id")
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
	if err := s.SerializeStream(&buf, replay(events)); err != nil {
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
				{ID: "a1", Name: "list_dir", Arguments: map[string]any{"p": "/x"}, Signature: "SIG_FIRST"},
				{ID: "a2", Name: "read_file", Arguments: map[string]any{"f": "y"}}, // no signature
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
	if err := (&StreamAdapter{Wrapped: false}).SerializeStream(&bare, mk()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bare.String(), `"response"`) {
		t.Errorf("bare gemini frames must NOT wrap in \"response\":\n%s", bare.String())
	}
	if !strings.Contains(bare.String(), `"candidates"`) {
		t.Errorf("bare gemini frames must carry candidates at top level:\n%s", bare.String())
	}

	var wrapped strings.Builder
	if err := (&StreamAdapter{Wrapped: true}).SerializeStream(&wrapped, mk()); err != nil {
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
