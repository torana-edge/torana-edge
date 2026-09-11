package openai

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func TestResponsesOpaqueInputItemsRoundTripInOrder(t *testing.T) {
	original := []byte(`{
  "model":"gpt-5.4",
  "previous_response_id":"resp_previous",
  "store":false,
  "input":[
    {"type":"message","role":"user","content":"inspect this"},
    {"type":"reasoning","id":"rs_1","encrypted_content":"opaque-reasoning","summary":[{"type":"summary_text","text":"looked"}]},
    {"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"},
    {"type":"compaction","id":"cmp_1","encrypted_content":"opaque-compaction","custom_future_field":{"nested":[1,2,3]}},
    {"type":"function_call_output","call_id":"call_1","output":"contents"}
  ]
}`)

	chat, err := (&Adapter{}).Unmarshal(original)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got["previous_response_id"] != "resp_previous" || got["store"] != false {
		t.Fatalf("top-level Responses fields changed: %s", encoded)
	}
	items, ok := got["input"].([]any)
	if !ok || len(items) != 5 {
		t.Fatalf("input = %#v, want five ordered items", got["input"])
	}
	wantTypes := []string{"message", "reasoning", "function_call", "compaction", "function_call_output"}
	for i, want := range wantTypes {
		item := items[i].(map[string]any)
		if item["type"] != want {
			t.Fatalf("item %d type = %v, want %s", i, item["type"], want)
		}
	}
	if gotReasoning := items[1].(map[string]any); gotReasoning["encrypted_content"] != "opaque-reasoning" {
		t.Fatalf("reasoning item changed: %#v", gotReasoning)
	}
	gotCompaction := items[3].(map[string]any)
	wantFuture := map[string]any{"nested": []any{float64(1), float64(2), float64(3)}}
	if gotCompaction["encrypted_content"] != "opaque-compaction" || !reflect.DeepEqual(gotCompaction["custom_future_field"], wantFuture) {
		t.Fatalf("compaction item changed: %#v", gotCompaction)
	}
}

func TestResponsesOpaqueItemsSurviveKnownMessageMutation(t *testing.T) {
	chat, err := (&Adapter{}).Unmarshal([]byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"before"},{"type":"compaction","encrypted_content":"opaque"},{"type":"message","role":"user","content":"after"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range chat.Messages[0].Blocks {
		if b.Text != nil {
			b.Text.Text = "changed"
		}
	}
	encoded, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	firstContent, firstOK := got.Input[0]["content"].([]any)
	lastContent, lastOK := got.Input[2]["content"].([]any)
	if !firstOK || !lastOK || len(firstContent) != 1 || len(lastContent) != 1 ||
		firstContent[0].(map[string]any)["text"] != "changed" ||
		lastContent[0].(map[string]any)["text"] != "after" ||
		got.Input[1]["type"] != "compaction" || got.Input[1]["encrypted_content"] != "opaque" {
		t.Fatalf("unexpected mutated round trip: %s", encoded)
	}
}

func TestResponsesCodexCustomToolItemsAreCanonicalAndRoundTrip(t *testing.T) {
	original := []byte(`{
  "model":"gpt-5.6-sol",
  "input":[
    {"type":"message","role":"user","content":[{"type":"input_text","text":"run it"}]},
    {"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","input":"sed -n '1p' marker.txt","status":"completed","internal_chat_message_metadata_passthrough":{"opaque":1}},
    {"type":"custom_tool_call_output","id":"ctco_1","call_id":"call_1","output":[{"type":"input_text","text":"TORANA_TOOL_PATH_OK"}],"internal_chat_message_metadata_passthrough":{"opaque":2}}
  ]
}`)

	chat, err := (&Adapter{}).Unmarshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("projected messages = %d, want message + free-form call + result", len(chat.Messages))
	}
	call := chat.Messages[1].Blocks[0].ToolUse
	if call == nil || call.InvocationKind != engine.ToolInvocationFreeform || call.InputText == nil ||
		*call.InputText != "sed -n '1p' marker.txt" || call.ID != "call_1" || call.Name != "exec" {
		t.Fatalf("custom call projection mismatch: %+v", call)
	}
	result := chat.Messages[2].Blocks[0].ToolResult
	if result == nil || result.InvocationKind != engine.ToolInvocationFreeform || result.ToolCallID != "call_1" ||
		len(result.Content) != 1 || result.Content[0].Text != "TORANA_TOOL_PATH_OK" {
		t.Fatalf("custom result projection mismatch: %+v", result)
	}
	encoded, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 3 {
		t.Fatalf("input item count = %d, want 3: %s", len(got.Input), encoded)
	}
	for _, i := range []int{1, 2} {
		var want, actual any
		if err := json.Unmarshal(mustInputItem(t, original, i), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got.Input[i], &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("canonical item %d changed:\n got %s\nwant %s", i, got.Input[i], mustInputItem(t, original, i))
		}
	}
	updated := "printf changed"
	chat.Messages[1].Blocks[0].ToolUse.InputText = &updated
	chat.Messages[2].Blocks[0].ToolResult.Content[0].Text = "CHANGED_OUTPUT"
	encoded, err = (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	var callWire map[string]any
	var resultWire map[string]any
	if err := json.Unmarshal(got.Input[1], &callWire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got.Input[2], &resultWire); err != nil {
		t.Fatal(err)
	}
	if callWire["input"] != updated || callWire["id"] != "ctc_1" || callWire["status"] != "completed" || callWire["internal_chat_message_metadata_passthrough"] == nil {
		t.Fatalf("call mutation lost canonical input or provider metadata: %s", got.Input[1])
	}
	output, ok := resultWire["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("result output shape mismatch: %s", got.Input[2])
	}
	outputItem, ok := output[0].(map[string]any)
	if !ok || outputItem["text"] != "CHANGED_OUTPUT" ||
		resultWire["id"] != "ctco_1" || resultWire["internal_chat_message_metadata_passthrough"] == nil {
		t.Fatalf("result mutation lost canonical output or provider metadata: %s", got.Input[2])
	}
}

func TestResponsesAdditionalToolsExposeFunctionAndFreeformDefinitions(t *testing.T) {
	original := []byte(`{
  "model":"gpt-5.6-sol",
  "input":[{"type":"additional_tools","id":"tools_1","role":"system","tools":[
    {"type":"namespace","name":"functions","description":"callable tools","tools":[
      {"type":"custom","name":"exec","description":"run shell","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"},"vendor_leaf":"keep"},
      {"type":"function","name":"wait","description":"wait","strict":true,"parameters":{"type":"object","properties":{"ms":{"type":"integer"}}}}
    ],"vendor_namespace":"keep"}
  ],"vendor_root":"keep"}]
}`)

	chat, err := (&Adapter{}).Unmarshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 2 {
		t.Fatalf("definitions = %d, want 2", len(chat.Tools))
	}
	custom, function := chat.Tools[0], chat.Tools[1]
	if custom.InvocationKind != engine.ToolInvocationFreeform || !reflect.DeepEqual(custom.NamespacePath, []string{"functions"}) ||
		custom.Name != "exec" || custom.InputFormat.String() != `{"type":"grammar","syntax":"lark","definition":"start: /.+/"}` {
		t.Fatalf("custom definition mismatch: %+v", custom)
	}
	if function.InvocationKind != engine.ToolInvocationFunction || !function.Strict || function.Name != "wait" ||
		!reflect.DeepEqual(function.NamespacePath, []string{"functions"}) {
		t.Fatalf("function definition mismatch: %+v", function)
	}

	chat.Tools[0].Description = "updated shell"
	chat.Tools[0].InputFormat = mustRequiredObject(t, `{"type":"grammar","syntax":"lark","definition":"start: /[a-z]+/"}`)
	chat.Tools[1].Parameters = mustRequiredObject(t, `{"type":"object","properties":{"seconds":{"type":"number"}}}`)
	encoded, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []struct {
			Type       string `json:"type"`
			VendorRoot string `json:"vendor_root"`
			Tools      []struct {
				Type            string            `json:"type"`
				Name            string            `json:"name"`
				VendorNamespace string            `json:"vendor_namespace"`
				Tools           []json.RawMessage `json:"tools"`
			} `json:"tools"`
		} `json:"input"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Input) != 1 || wire.Input[0].VendorRoot != "keep" || len(wire.Input[0].Tools) != 1 ||
		wire.Input[0].Tools[0].VendorNamespace != "keep" || len(wire.Input[0].Tools[0].Tools) != 2 {
		t.Fatalf("additional_tools topology/extras changed: %s", encoded)
	}
	var gotCustom struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		VendorLeaf  string          `json:"vendor_leaf"`
		Format      json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(wire.Input[0].Tools[0].Tools[0], &gotCustom); err != nil {
		t.Fatal(err)
	}
	if gotCustom.Type != "custom" || gotCustom.Name != "exec" || gotCustom.Description != "updated shell" ||
		gotCustom.VendorLeaf != "keep" || string(gotCustom.Format) != `{"type":"grammar","syntax":"lark","definition":"start: /[a-z]+/"}` {
		t.Fatalf("rebuilt custom definition mismatch: %s", wire.Input[0].Tools[0].Tools[0])
	}
	var gotFunction struct {
		Type       string          `json:"type"`
		Strict     bool            `json:"strict"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(wire.Input[0].Tools[0].Tools[1], &gotFunction); err != nil {
		t.Fatal(err)
	}
	if gotFunction.Type != "function" || !gotFunction.Strict || string(gotFunction.Parameters) != `{"type":"object","properties":{"seconds":{"type":"number"}}}` {
		t.Fatalf("rebuilt function definition mismatch: %s", wire.Input[0].Tools[0].Tools[1])
	}
}

func TestResponsesAdditionalToolsAfterConversationIsRejected(t *testing.T) {
	raw := []byte(`{"model":"gpt","input":[
  {"type":"message","role":"user","content":"before"},
  {"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","format":{}}]}]}
]}`)
	_, err := (&Adapter{}).Unmarshal(raw)
	if err == nil || !strings.Contains(err.Error(), "must precede conversation items") {
		t.Fatalf("late additional_tools error = %v", err)
	}
}

func TestResponsesNamespacedToolTopologyIsHostOwned(t *testing.T) {
	base := &pb.ChatRequest{Tools: []*pb.ToolDef{
		{Name: "top", ParametersJson: []byte(`{}`)},
		{Name: "exec", InvocationKind: pb.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM,
			InputFormatJson: []byte(`{}`), NamespacePath: []string{"functions", "shell"}},
	}}
	for _, tc := range []struct {
		name string
		edit func(*pb.ChatRequest)
		ok   bool
	}{
		{"definition edit", func(r *pb.ChatRequest) { r.Tools[1].Description = "safe shell" }, true},
		{"top-level add", func(r *pb.ChatRequest) {
			r.Tools = append(r.Tools, &pb.ToolDef{Name: "new", ParametersJson: []byte(`{}`)})
		}, true},
		{"namespace move", func(r *pb.ChatRequest) { r.Tools[1].NamespacePath = []string{"other"} }, false},
		{"namespaced delete", func(r *pb.ChatRequest) { r.Tools = r.Tools[:1] }, false},
		{"namespaced add", func(r *pb.ChatRequest) {
			r.Tools = append(r.Tools, &pb.ToolDef{Name: "extra", ParametersJson: []byte(`{}`), NamespacePath: []string{"functions"}})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := proto.Clone(base).(*pb.ChatRequest)
			tc.edit(candidate)
			if got := VerifyResponsesToolTopologyPB(base, candidate) == nil; got != tc.ok {
				t.Fatalf("accepted = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestResponsesUnchangedCustomItemsKeepRawMemberOrderAndLexemes(t *testing.T) {
	raw := []byte(`{"model":"gpt","input":[
  {"vendor":{"z":1e999,"a":1.0},"input":"echo \\u0068i","name":"shell","call_id":"c1","type":"custom_tool_call"},
  {"output":[{"text":"ok","type":"input_text"}],"vendor":"keep","call_id":"c1","type":"custom_tool_call_output"}
]}`)
	chat, err := (&Adapter{}).Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		wantItem := mustInputItem(t, raw, i)
		gotItem := mustInputItem(t, got, i)
		var wantCompact, gotCompact bytes.Buffer
		if err := json.Compact(&wantCompact, wantItem); err != nil {
			t.Fatal(err)
		}
		if err := json.Compact(&gotCompact, gotItem); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotCompact.Bytes(), wantCompact.Bytes()) {
			t.Fatalf("item %d changed without a semantic mutation:\n got %s\nwant %s", i, gotCompact.Bytes(), wantCompact.Bytes())
		}
	}
}

func mustRequiredObject(t *testing.T, raw string) engine.RequiredJSONObject {
	t.Helper()
	v, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustInputItem(t *testing.T, body []byte, index int) json.RawMessage {
	t.Helper()
	var envelope struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Input[index]
}
