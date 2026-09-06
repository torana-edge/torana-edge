package pbconv

import (
	"reflect"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestFreeformToolContractRoundTripsAcrossEnginePB(t *testing.T) {
	input := ""
	format, err := engine.ParseRequiredJSONObject([]byte(`{"type":"grammar","syntax":"lark"}`))
	if err != nil {
		t.Fatal(err)
	}
	chat := &engine.ChatRequest{
		Model: "gpt",
		Tools: []engine.ToolDef{{Name: "exec", InvocationKind: engine.ToolInvocationFreeform, InputFormat: format, NamespacePath: []string{"functions"}}},
		Messages: []engine.Message{
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "c1", Name: "exec", InvocationKind: engine.ToolInvocationFreeform, InputText: &input}}}},
			{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "c1", InvocationKind: engine.ToolInvocationFreeform, Content: []engine.ToolResultContentBlock{{Text: "out"}}}}}},
		},
	}
	if err := ValidateFullRequest(chat); err != nil {
		t.Fatal(err)
	}
	wire, err := ToPBChatRequestChecked(chat)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Messages[0].Blocks[0].GetToolUse().InputText == nil ||
		wire.Messages[0].Blocks[0].GetToolUse().InvocationKind != pb.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM ||
		wire.Tools[0].InputFormatJson == nil || !reflect.DeepEqual(wire.Tools[0].NamespacePath, []string{"functions"}) {
		t.Fatalf("PB projection lost free-form presence: %+v", wire)
	}
	back, err := FromPBChatRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if back.Messages[0].Blocks[0].ToolUse.InputText == nil || *back.Messages[0].Blocks[0].ToolUse.InputText != "" ||
		back.Messages[1].Blocks[0].ToolResult.InvocationKind != engine.ToolInvocationFreeform ||
		!reflect.DeepEqual(back.Tools[0].NamespacePath, []string{"functions"}) {
		t.Fatalf("engine round trip lost free-form state: %+v", back)
	}
}

func TestFreeformToolStreamRoundTripsAcrossEnginePB(t *testing.T) {
	input := ""
	events := []*engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 3, ID: "c", Name: "shell", Signature: "sig", InvocationKind: engine.ToolInvocationFreeform}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 3, InputTextDelta: &input}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 3}},
	}
	tracker := &BlockKindTracker{}
	for i, event := range events {
		wire := ToPBStreamEvent(event)
		back, err := tracker.FromPBStreamEvent(wire)
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		switch i {
		case 0:
			if back.ToolCallStart == nil || back.ToolCallStart.InvocationKind != engine.ToolInvocationFreeform || back.ToolCallStart.Signature != "sig" {
				t.Fatalf("start %+v", back)
			}
		case 1:
			if back.ToolCallDelta == nil || back.ToolCallDelta.InputTextDelta == nil || *back.ToolCallDelta.InputTextDelta != "" || back.ToolCallDelta.ArgumentsDelta != "" {
				t.Fatalf("delta %+v", back)
			}
		case 2:
			if back.ToolCallEnd == nil || back.ToolCallEnd.Index != 3 {
				t.Fatalf("end %+v", back)
			}
		}
	}
}

func TestFromPBStreamRejectsInvocationFamilyMismatch(t *testing.T) {
	tracker := &BlockKindTracker{}
	start := ToPBStreamEvent(&engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{
		Index: 1, ID: "c", Name: "shell", InvocationKind: engine.ToolInvocationFreeform,
	}})
	if _, err := tracker.FromPBStreamEvent(start); err != nil {
		t.Fatal(err)
	}
	wrong := &pb.StreamEvent{Event: &pb.StreamEvent_ToolCallDelta{ToolCallDelta: &pb.ToolCallDelta{Index: 1, ArgumentsDelta: `{}`}}}
	if _, err := tracker.FromPBStreamEvent(wrong); err == nil {
		t.Fatal("function arguments accepted for free-form call")
	}
}
