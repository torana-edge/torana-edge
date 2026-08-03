package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
)

// Ordered-body test helpers (engine.Message).

func textBlock(s string) engine.Block {
	return engine.Block{Text: &engine.TextBlock{Text: s}}
}

func msgBlock(role engine.Role, text string) engine.Message {
	return engine.Message{Role: role, Blocks: []engine.Block{textBlock(text)}}
}

func textOf(m engine.Message) string { return m.Text() }

type tcView struct {
	ID, Name  string
	Args      engine.RequiredJSONObject
	Signature string
}

func toolCalls(m engine.Message) []tcView {
	var out []tcView
	for _, b := range m.Blocks {
		if b.ToolUse != nil {
			out = append(out, tcView{ID: b.ToolUse.ID, Name: b.ToolUse.Name, Args: b.ToolUse.Arguments, Signature: b.ToolUse.Signature})
		}
	}
	return out
}

func toolResults(m engine.Message) []*engine.ToolResultBlock {
	var out []*engine.ToolResultBlock
	for _, b := range m.Blocks {
		if b.ToolResult != nil {
			out = append(out, b.ToolResult)
		}
	}
	return out
}
func TestRoundTrip(t *testing.T) {
	a := &Adapter{}

	// Sample Gemini request with system instruction, text, and a function call.
	input := `{
		"systemInstruction": {
			"parts": [{"text": "You are a helpful assistant."}]
		},
		"contents": [
			{"role": "user", "parts": [{"text": "What is the weather in Paris?"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "get_weather", "args": {"location": "Paris", "unit": "celsius"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "get_weather", "response": {"temperature": 22, "condition": "sunny"}}}]},
			{"role": "model", "parts": [{"text": "The weather in Paris is 22C and sunny."}]}
		],
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "Get current weather", "parameters": {"type": "object", "properties": {"location": {"type": "string"}}}}]}]
	}`

	chat, err := a.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if len(chat.Messages) != 5 {
		t.Fatalf("Expected 5 messages, got %d", len(chat.Messages))
	}

	if chat.Messages[0].Role != engine.RoleSystem {
		t.Errorf("Message 0: expected system, got %s", chat.Messages[0].Role)
	}
	if textOf(chat.Messages[0]) != "You are a helpful assistant." {
		t.Errorf("Message 0: wrong content: %s", textOf(chat.Messages[0]))
	}

	if chat.Messages[1].Role != engine.RoleUser {
		t.Errorf("Message 1: expected user, got %s", chat.Messages[1].Role)
	}

	if len(toolCalls(chat.Messages[2])) != 1 {
		t.Fatalf("Message 2: expected 1 tool call, got %d", len(toolCalls(chat.Messages[2])))
	}
	tc := toolCalls(chat.Messages[2])[0]
	if tc.Name != "get_weather" {
		t.Errorf("ToolCall name: expected get_weather, got %s", tc.Name)
	}
	vals, _, err := tc.Args.DecodeObject()
	if err != nil {
		t.Fatalf("ToolCall args decode: %v", err)
	}
	if string(vals["location"]) != `"Paris"` {
		t.Errorf("ToolCall args: location = %v", vals["location"])
	}

	if len(chat.Tools) != 1 {
		t.Fatalf("Expected 1 tool, got %d", len(chat.Tools))
	}
	if chat.Tools[0].Name != "get_weather" {
		t.Errorf("Tool name: %s", chat.Tools[0].Name)
	}

	// Marshal back.
	output, err := a.Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("Marshal output is not valid JSON: %v\noutput: %s", err, string(output))
	}

	// Verify round-trip produces valid structure.
	contents, ok := result["contents"].([]any)
	if !ok {
		t.Fatalf("Marshal output missing contents array")
	}
	if len(contents) < 3 {
		t.Fatalf("Expected at least 3 contents, got %d", len(contents))
	}
}

func TestUnmarshalNoSystem(t *testing.T) {
	a := &Adapter{}
	input := `{"contents": [{"role": "user", "parts": [{"text": "Hello"}]}]}`

	chat, err := a.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != engine.RoleUser {
		t.Errorf("Expected user role, got %s", chat.Messages[0].Role)
	}
}

func TestStreamParse(t *testing.T) {
	s := &StreamAdapter{}

	lines := `{"candidates": [{"content": {"parts": [{"text": "Let me check the weather."}], "role": "model"}}]}
{"candidates": [{"content": {"parts": [{"functionCall": {"name": "get_weather", "args": {"location": "Paris"}}}], "role": "model"}}]}
{"candidates": [{"finishReason": "STOP"}]}
`
	r := strings.NewReader(lines)
	ch := s.ParseStream(r)

	var events []engine.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}

	// The text part opens and closes an explicit text block (the v2 topology
	// requires every content block to open with a start event), consuming
	// block index 0; the tool call that follows takes index 1.
	if len(events) != 7 {
		t.Fatalf("Expected 7 events, got %d: %+v", len(events), events)
	}

	if events[0].BlockStart == nil || events[0].BlockStart.Index != 0 || events[0].BlockStart.Kind != engine.BlockKindText {
		t.Errorf("Event 0: expected BlockStart(text, 0), got %+v", events[0])
	}

	if events[1].TextDelta == nil || *events[1].TextDelta != "Let me check the weather." {
		t.Errorf("Event 1: expected TextDelta, got %+v", events[1])
	}

	if events[2].BlockStop == nil || events[2].BlockStop.Index != 0 {
		t.Errorf("Event 2: expected BlockStop(0), got %+v", events[2])
	}

	if events[3].ToolCallStart == nil || events[3].ToolCallStart.Index != 1 || events[3].ToolCallStart.Name != "get_weather" {
		t.Errorf("Event 3: expected ToolCallStart(1) get_weather, got %+v", events[3])
	}

	if events[4].ToolCallDelta == nil {
		t.Errorf("Event 4: expected ToolCallDelta, got %+v", events[4])
	} else {
		delta := events[4].ToolCallDelta.ArgumentsDelta
		var args map[string]any
		if err := json.Unmarshal([]byte(delta), &args); err != nil {
			t.Errorf("ToolCallDelta args not valid JSON: %s (error: %v)", delta, err)
		}
		if args["location"] != "Paris" {
			t.Errorf("ToolCallDelta args: location = %v", args["location"])
		}
	}

	if events[5].ToolCallEnd == nil || events[5].ToolCallEnd.Index != 1 {
		t.Errorf("Event 5: expected ToolCallEnd(1), got %+v", events[5])
	}

	if events[6].FinishReason != "stop" {
		t.Errorf("Event 6: expected FinishReason 'stop', got %s", events[6].FinishReason)
	}
}

func TestStreamSerialize(t *testing.T) {
	s := &StreamAdapter{}

	events := make(chan engine.StreamEvent, 5)
	text := "Hello"
	events <- engine.StreamEvent{TextDelta: &text}
	events <- engine.StreamEvent{
		ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "get_weather", Name: "get_weather"},
	}
	events <- engine.StreamEvent{
		ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"location":"Paris"}`},
	}
	events <- engine.StreamEvent{
		ToolCallEnd: &engine.ToolCallEnd{Index: 0},
	}
	events <- engine.StreamEvent{FinishReason: "stop"}
	close(events)

	var buf strings.Builder
	if err := s.SerializeStream(context.Background(), &buf, events); err != nil {
		t.Fatalf("SerializeStream error: %v", err)
	}

	frames := parseSSEFrames(t, buf.String())
	if len(frames) < 3 {
		t.Fatalf("Expected at least 3 SSE frames, got %d:\n%s", len(frames), buf.String())
	}

	// First frame should have text.
	if len(frames[0].Candidates) == 0 || frames[0].Candidates[0].Content == nil {
		t.Fatalf("Frame 1 missing content")
	}
	if len(frames[0].Candidates[0].Content.Parts) == 0 || partText(frames[0].Candidates[0].Content.Parts[0]) != "Hello" {
		t.Errorf("Frame 1 text mismatch")
	}

	// Second frame should have functionCall.
	if len(frames[1].Candidates) == 0 || frames[1].Candidates[0].Content == nil {
		t.Fatalf("Frame 2 missing content")
	}
	if len(frames[1].Candidates[0].Content.Parts) == 0 || frames[1].Candidates[0].Content.Parts[0].FunctionCall == nil {
		t.Fatalf("Frame 2 missing functionCall")
	}
	if fc := frames[1].Candidates[0].Content.Parts[0].FunctionCall; fc.Name != "get_weather" {
		t.Errorf("Frame 2 functionCall name: %s", fc.Name)
	}

	// Last frame should have finishReason.
	last := frames[len(frames)-1]
	if len(last.Candidates) == 0 || last.Candidates[0].FinishReason != "STOP" {
		t.Errorf("Last frame: expected finishReason STOP, got %+v", last)
	}
}

// parseSSEFrames splits serialized output into frames, tolerating both the
// bare (`data: {<chunk>}`) and Code Assist wrapped (`data: {"response":{…}}`)
// shapes — mirroring ParseStream.
func parseSSEFrames(t *testing.T, output string) []geminiStreamChunk {
	t.Helper()
	var chunks []geminiStreamChunk
	for _, block := range strings.Split(output, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		data, ok := strings.CutPrefix(block, "data:")
		if !ok {
			t.Fatalf("frame missing data: prefix: %q", block)
		}
		raw := strings.TrimSpace(data)
		var frame streamFrame
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("frame not valid JSON: %v (%q)", err, block)
		}
		if frame.Response != nil {
			chunks = append(chunks, *frame.Response)
			continue
		}
		var bare geminiStreamChunk
		if err := json.Unmarshal([]byte(raw), &bare); err != nil {
			t.Fatalf("bare frame not valid JSON: %v (%q)", err, block)
		}
		chunks = append(chunks, bare)
	}
	return chunks
}

// TestPartMetadataCarrierTable — the SIX part_metadata_json carriers are
// absent-OR-strict-object at every boundary: parse -> Engine -> PB ->
// Engine -> final Gemini wire. Absent stays absent (never an invented
// {}), present-empty stays present-empty, populated stays byte-exact.
// The final wire is re-parsed with the same grammar so the assertion is
// on the provider wire, not the internal structs.
func TestPartMetadataCarrierTable(t *testing.T) {
	rows := []struct {
		name    string
		body    string
		carrier func(*engine.ChatRequest) *engine.OptionalJSONObject
		absent  bool
	}{
		{
			"text absent", `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].Text.PartMetadataJson
			},
			true,
		},
		{
			"text present-empty", `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi","partMetadata":{}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].Text.PartMetadataJson
			},
			false,
		},
		{
			"text populated", `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi","partMetadata":{"src":"x","n":1}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].Text.PartMetadataJson
			},
			false,
		},
		{
			"thinking", `{"model":"m","contents":[{"role":"model","parts":[{"thought":true,"text":"r","partMetadata":{"k":"v"}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].Thinking.PartMetadataJson
			},
			false,
		},
		{
			"tool use", `{"model":"m","contents":[{"role":"model","parts":[{"functionCall":{"name":"r","args":{},"id":"c1"},"partMetadata":{"k":"v"}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].ToolUse.PartMetadataJson
			},
			false,
		},
		{
			"tool result", `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"},"partMetadata":{"k":"v"}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].ToolResult.PartMetadataJson
			},
			false,
		},
		{
			"unknown media", `{"model":"m","contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBOR"},"partMetadata":{"k":"v"}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].Unknown.PartMetadataJson
			},
			false,
		},
		{
			"trailing standalone", `{"model":"m","contents":[{"role":"model","parts":[{"text":"a"},{"text":"","thoughtSignature":"S","partMetadata":{"k":"v"}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[1].TrailingSignature.PartMetadataJson
			},
			false,
		},
		{
			"system text", `{"model":"m","systemInstruction":{"parts":[{"text":"sys","partMetadata":{"k":"v"}}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].Text.PartMetadataJson
			},
			false,
		},
		{
			"assistant/model unknown", `{"model":"m","contents":[{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBOR"},"thoughtSignature":"MODEL_SIG","partMetadata":{"k":"v"}}]}]}`,
			func(c *engine.ChatRequest) *engine.OptionalJSONObject {
				return &c.Messages[0].Blocks[0].Unknown.PartMetadataJson
			},
			false,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			chat, err := (&Adapter{}).Unmarshal([]byte(row.body))
			if err != nil {
				t.Fatal(err)
			}
			carrier := row.carrier(chat)
			if row.absent {
				if !carrier.IsAbsent() || len(carrier.Bytes()) != 0 {
					t.Fatalf("absent carrier became %q (invented {}?)", carrier.Bytes())
				}
			} else if carrier.IsAbsent() {
				t.Fatalf("present metadata lost at parse")
			}

			// Engine -> PB -> Engine: absence must survive (nil stays nil).
			pbReq, err := pbconv.ToPBChatRequestChecked(chat)
			if err != nil {
				t.Fatal(err)
			}
			back, err := pbconv.FromPBChatRequest(pbReq)
			if err != nil {
				t.Fatal(err)
			}
			carrier2 := row.carrier(back)
			if row.absent {
				if !carrier2.IsAbsent() || len(carrier2.Bytes()) != 0 {
					t.Fatalf("absent carrier invented at the PB boundary: %q", carrier2.Bytes())
				}
			} else if carrier2.IsAbsent() {
				t.Fatalf("metadata lost through PB")
			} else if string(carrier2.Bytes()) != string(carrier.Bytes()) {
				t.Fatalf("metadata changed through PB: %q -> %q", carrier.Bytes(), carrier2.Bytes())
			}

			// Engine -> final Gemini wire -> re-parse: the wire carries the
			// fact exactly.
			out, err := (&Adapter{}).Marshal(back)
			if err != nil {
				t.Fatal(err)
			}
			reparsed, err := (&Adapter{}).Unmarshal(out)
			if err != nil {
				t.Fatal(err)
			}
			carrier3 := row.carrier(reparsed)
			if row.absent {
				if !carrier3.IsAbsent() {
					t.Fatalf("absent carrier invented on the wire: %q", carrier3.Bytes())
				}
			} else if string(carrier3.Bytes()) != string(carrier.Bytes()) {
				t.Fatalf("metadata changed through the wire: %q -> %q", carrier.Bytes(), carrier3.Bytes())
			}
			// The ASSISTANT/MODEL unknown row additionally pins the exact
			// final-wire signature.
			if row.name == "assistant/model unknown" {
				if reparsed.Messages[0].Blocks[0].Unknown.Signature != "MODEL_SIG" {
					t.Fatalf("model unknown signature lost on the wire: %+v", reparsed.Messages[0].Blocks[0].Unknown)
				}
			}
		})
	}
}

// TestSystemInstructionSingleEmission — system text is emitted EXACTLY
// ONCE: only through systemInstruction, never duplicated into contents.
// Explicit-empty system text survives.
func TestSystemInstructionSingleEmission(t *testing.T) {
	body := `{"model":"m","systemInstruction":{"parts":[{"text":"sys","partMetadata":{"k":"v"}},{"text":""}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		System struct {
			Parts []map[string]any `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Role  string           `json:"role"`
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if len(top.System.Parts) != 2 {
		t.Fatalf("system parts = %d, want exactly 2 (non-empty + explicit empty): %s", len(top.System.Parts), out)
	}
	if top.System.Parts[0]["text"] != "sys" || top.System.Parts[0]["partMetadata"] == nil {
		t.Fatalf("system text/metadata lost: %v", top.System.Parts[0])
	}
	if txt, ok := top.System.Parts[1]["text"].(string); !ok || txt != "" {
		t.Fatalf("explicit empty system text lost: %v", top.System.Parts[1])
	}
	if len(top.Contents) != 1 || top.Contents[0].Role != "user" {
		t.Fatalf("contents must contain only the non-system messages: %+v", top.Contents)
	}
	if len(top.Contents[0].Parts) != 1 {
		t.Fatalf("system text duplicated into contents: %+v", top.Contents)
	}
}

// TestEmptyToolResultTextWraps — EMPTY tool output is a normal user case:
// it must marshal through the REAL adapter as the semantic wrap, not fail
// as a misclassified absent object. Bare Gemini and Code Assist rows.
func TestEmptyToolResultTextWraps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		codeAssist bool
		wantKey    string
	}{
		{"bare", false, "content"},
		{"code assist", true, "output"},
	} {
		for _, text := range []string{"", "   "} {
			t.Run(tc.name+"/"+fmt.Sprintf("%q", text), func(t *testing.T) {
				chat := &engine.ChatRequest{
					Model: "m",
					Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{
						ToolResult: &engine.ToolResultBlock{
							ToolCallID: "c1", ToolName: "read",
							Content: []engine.ToolResultContentBlock{{Text: text}},
						},
					}}}},
				}
				if tc.codeAssist {
					chat.CodeAssist = true
				}
				out, err := (&Adapter{}).Marshal(chat)
				if err != nil {
					t.Fatalf("empty tool output must marshal: %v", err)
				}
				var top map[string]any
				if err := json.Unmarshal(out, &top); err != nil {
					t.Fatal(err)
				}
				var doc map[string]any
				if tc.codeAssist {
					req, ok := top["request"].(map[string]any)
					if !ok {
						t.Fatalf("no request envelope: %s", out)
					}
					doc = req
				} else {
					doc = top
				}
				contents := doc["contents"].([]any)
				msg := contents[0].(map[string]any)
				part := msg["parts"].([]any)[0].(map[string]any)
				fr := part["functionResponse"].(map[string]any)
				resp := fr["response"].(map[string]any)
				// EXACT value under exactly the expected key.
				if got, ok := resp[tc.wantKey].(string); !ok || got != text {
					t.Fatalf("expected %q under %q, got %v", text, tc.wantKey, resp)
				}
				other := "output"
				if tc.wantKey == "output" {
					other = "content"
				}
				if _, ok := resp[other]; ok {
					t.Fatalf("opposite key %q must be absent: %v", other, resp)
				}
				if len(resp) != 1 {
					t.Fatalf("response must be exactly the wrap: %v", resp)
				}
			})
		}
	}
}

// TestFunctionResponsePartRoundTrip — the sealed union is the exact
// one-arm WRAPPER object at every boundary: parse -> nested Unknown
// payload -> final wire, for BOTH arms incl. displayName.
func TestFunctionResponsePartRoundTrip(t *testing.T) {
	rows := map[string]string{
		"inlineData": `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBOR","displayName":"pic.png"}}]}}]}]}`,
		"fileData":   `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1","parts":[{"fileData":{"mimeType":"video/mp4","fileUri":"gs://b/x.mp4","displayName":"clip"}}]}}]}]}`,
	}
	for name, body := range rows {
		t.Run(name, func(t *testing.T) {
			chat, err := (&Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			tr := chat.Messages[0].Blocks[0].ToolResult
			if tr == nil || len(tr.Content) != 2 || tr.Content[1].Unknown == nil {
				t.Fatalf("nested media element missing: %+v", tr)
			}
			// The payload is the exact one-arm WRAPPER object.
			payload := string(tr.Content[1].Unknown.Payload.Bytes())
			if !strings.Contains(payload, name) {
				t.Fatalf("payload %q is not the wrapper object", payload)
			}
			out, err := (&Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatal(err)
			}
			// The final wire carries the sealed part byte-exactly.
			if !strings.Contains(string(out), `"displayName"`) {
				t.Fatalf("displayName lost on the wire: %s", out)
			}
			reparsed, err := (&Adapter{}).Unmarshal(out)
			if err != nil {
				t.Fatalf("final wire re-parse: %v (%s)", err, out)
			}
			tr2 := reparsed.Messages[0].Blocks[0].ToolResult
			if string(tr2.Content[1].Unknown.Payload.Bytes()) != payload {
				t.Fatalf("payload changed through the wire: %q -> %q", payload, tr2.Content[1].Unknown.Payload.Bytes())
			}
		})
	}
}

// TestFunctionResponsePartNegatives — the sealed union refuses every
// escape: missing/wrong-typed required members, two arms, extra outer
// members, extra inner members, and no-arm parts.
func TestFunctionResponsePartNegatives(t *testing.T) {
	rows := map[string]string{
		"missing mimeType":   `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{"inlineData":{"data":"x"}}]}}]}]}`,
		"missing data":       `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{"inlineData":{"mimeType":"image/png"}}]}}]}]}`,
		"wrong-typed data":   `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{"inlineData":{"mimeType":"image/png","data":5}}]}}]}]}`,
		"missing fileUri":    `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{"fileData":{"mimeType":"video/mp4"}}]}}]}]}`,
		"two arms":           `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{"inlineData":{"mimeType":"i","data":"d"},"fileData":{"mimeType":"v","fileUri":"u"}}]}}]}]}`,
		"extra outer member": `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{"inlineData":{"mimeType":"i","data":"d"},"thought":true}]}}]}]}`,
		"extra inner member": `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{"inlineData":{"mimeType":"i","data":"d","videoMetadata":{}}}]}}]}]}`,
		"no arm":             `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","parts":[{}]}}]}]}`,
	}
	for name, body := range rows {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Adapter{}).Unmarshal([]byte(body)); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}

// TestFunctionResponseUnknownMembersRefused — the six-field inventory is
// exact: any additional functionResponse member is the value-free 400,
// never a silently dropped fact.
func TestFunctionResponseUnknownMembersRefused(t *testing.T) {
	rows := map[string]string{
		"extra member":    `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","extraField":1}}]}]}`,
		"provider future": `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{},"id":"c1","toolUses":[]}}]}]}`,
	}
	for name, body := range rows {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Adapter{}).Unmarshal([]byte(body)); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}

// TestResponseObjectStrictDetection — the §5 verbatim path is decided by
// the SHARED strict object authority: duplicate keys, escape-equivalent
// duplicates, lone surrogates, invalid UTF-8, trailing values, and
// non-object top-level shapes are NOT verbatim (they get the documented
// semantic wrap), while a valid strict object survives lexeme-exact
// (numeric lexemes + member order).
func TestResponseObjectStrictDetection(t *testing.T) {
	// A valid strict object is verbatim: lexemes and order survive.
	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{"z":1e999,"a":{"b":2},"n":-0.5},"id":"c1"}}]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := chat.Messages[0].Blocks[0].ToolResult
	if tr.Content[0].Text != `{"z":1e999,"a":{"b":2},"n":-0.5}` {
		t.Fatalf("strict object not verbatim: %q", tr.Content[0].Text)
	}
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"response":{"z":1e999,"a":{"b":2},"n":-0.5}`) {
		t.Fatalf("strict object not re-emitted verbatim: %s", out)
	}

	// Non-strict texts get the semantic wrap (never verbatim).
	wrapped := map[string]string{
		"duplicate keys":        `{"a":1,"a":2}`,
		"escape-equivalent dup": `{"a":1,"\u0061":2}`,
		"lone surrogate":        `{"a":"\ud800"}`,
		"invalid utf8":          "{\"a\":\"\xff\xfe\"}",
		"trailing value":        `{"a":1} true`,
		"array top-level":       `[1,2]`,
		"string top-level":      `"x"`,
		"null top-level":        `null`,
		"not json at all":       `not json`,
	}
	for name, text := range wrapped {
		t.Run(name, func(t *testing.T) {
			got := geminiResponseObject(text, false)
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("wrap output not an object: %s", got)
			}
			if _, ok := m["content"]; !ok {
				t.Fatalf("expected the semantic content wrap, got %s", got)
			}
			if string(got) == text {
				t.Fatal("non-strict text emitted verbatim")
			}
		})
	}
}

// TestFunctionResponseNullSemantics — per the pinned provider JSON
// contract, an explicit JSON null for an OPTIONAL functionResponse member
// (id, willContinue, scheduling, parts) is ABSENT: pointer decoding must
// not give null accidental semantics (a null scheduling is NOT an invalid
// vocabulary value; a null willContinue is NOT an explicit false).
func TestFunctionResponseNullSemantics(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{"output":"x"},"id":null,"willContinue":null,"scheduling":null,"parts":null}}]}]}`
	chat, err := (&Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := chat.Messages[0].Blocks[0].ToolResult
	// null id == ABSENT id at the wire boundary: both take the documented
	// bare-Gemini synthesis (never an error, never a distinct value).
	noID := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"r","response":{"output":"x"}}}]}]}`
	absentChat, err := (&Adapter{}).Unmarshal([]byte(noID))
	if err != nil {
		t.Fatal(err)
	}
	if tr.ToolCallID != absentChat.Messages[0].Blocks[0].ToolResult.ToolCallID {
		t.Fatalf("null id diverged from absent: %q vs %q", tr.ToolCallID, absentChat.Messages[0].Blocks[0].ToolResult.ToolCallID)
	}
	if tr.WillContinue != nil {
		t.Fatalf("null willContinue gained a value: %v", *tr.WillContinue)
	}
	if tr.Scheduling != nil {
		t.Fatalf("null scheduling gained a value: %q", *tr.Scheduling)
	}
	if len(tr.Content) != 1 {
		t.Fatalf("null parts materialized: %d content elements", len(tr.Content))
	}
	// And the marshal round-trips: the nulls are gone (absent), the
	// response survives.
	out, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "null") {
		t.Fatalf("marshal emitted null members: %s", out)
	}
}

// TestSystemUnknownBlockFailsClosed — the provider-independent contract
// permits a system Unknown block (ValidateFullRequest accepts it), but
// Gemini system parts are TEXT-ONLY: marshal must REJECT the request
// rather than silently drop the block.
func TestSystemUnknownBlockFailsClosed(t *testing.T) {
	chat := &engine.ChatRequest{
		Model: "m",
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Blocks: []engine.Block{{
				Unknown: &engine.UnknownBlock{Kind: "part", Payload: mustReqObj(`{"inlineData":{"mimeType":"image/png","data":"iVBOR"}}`)},
			}}},
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}},
		},
	}
	// The common contract accepts it…
	if err := pbconv.ValidateFullRequest(chat); err != nil {
		t.Fatalf("the engine contract must accept a system unknown block: %v", err)
	}
	// …but the Gemini adapter rejects it instead of dropping it.
	if _, err := (&Adapter{}).Marshal(chat); err == nil {
		t.Fatal("gemini marshal must fail closed on a non-text system block")
	}
}

// TestSystemToolBlockFailsClosed — same boundary for a system tool-use
// block.
func TestSystemToolBlockFailsClosed(t *testing.T) {
	chat := &engine.ChatRequest{
		Model: "m",
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Blocks: []engine.Block{{
				ToolUse: &engine.ToolUseBlock{ID: "c1", Name: "read", Arguments: mustReqObj(`{"p":1}`)},
			}}},
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}},
		},
	}
	if err := pbconv.ValidateFullRequest(chat); err != nil {
		t.Fatalf("the engine contract must accept a system tool-use block: %v", err)
	}
	if _, err := (&Adapter{}).Marshal(chat); err == nil {
		t.Fatal("gemini marshal must fail closed on a non-text system block")
	}
}

func mustReqObj(raw string) engine.RequiredJSONObject {
	obj, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return obj
}
