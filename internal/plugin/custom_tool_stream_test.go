package plugin

import (
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func freeformToolStream(index int32, input, signature string) []*pbv1.StreamEvent {
	return []*pbv1.StreamEvent{
		{Event: &pbv1.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv1.ContentBlockStart{
			Index: index,
			Block: &pbv1.ContentBlockStart_ToolCall{ToolCall: &pbv1.ToolCallRef{
				Id: "call", Name: "shell", Signature: signature,
				InvocationKind: pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM,
			}},
		}}},
		{Event: &pbv1.StreamEvent_ToolCallDelta{ToolCallDelta: &pbv1.ToolCallDelta{Index: index, InputTextDelta: &input}}},
		{Event: &pbv1.StreamEvent_ContentBlockStop{ContentBlockStop: &pbv1.ContentBlockStop{Index: index}}},
	}
}

func TestFreeformStreamSignatureAndGrantPolicy(t *testing.T) {
	accepted := freeformToolStream(2, "echo before", "signed")
	unchanged := freeformToolStream(2, "echo before", "signed")
	changed := freeformToolStream(2, "echo after", "")
	stale := freeformToolStream(2, "echo after", "signed")
	grant := func(name string) bool { return name == "ir.messages.write.assistant" }

	if err := verifyStream(accepted, unchanged, nil); err != nil {
		t.Fatalf("unchanged stream rejected: %v", err)
	}
	if err := verifyStream(accepted, changed, grant); err != nil {
		t.Fatalf("cleared free-form mutation rejected: %v", err)
	}
	if err := verifyStreamPolicy(accepted, changed, grant); err != nil {
		t.Fatalf("assistant-granted free-form mutation rejected: %v", err)
	}
	if err := verifyStreamPolicy(accepted, changed, nil); err == nil || !strings.Contains(err.Error(), "ir.messages.write.assistant") {
		t.Fatalf("grant-less free-form mutation error = %v", err)
	}
	if err := verifyStream(accepted, stale, grant); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale signature accepted: %v", err)
	}
}

func TestStreamDisciplineRejectsCrossFamilyDeltas(t *testing.T) {
	empty := ""
	for _, events := range [][]*pbv1.StreamEvent{
		{
			toolStartEvent(0, "c", "function"),
			{Event: &pbv1.StreamEvent_ToolCallDelta{ToolCallDelta: &pbv1.ToolCallDelta{Index: 0, InputTextDelta: &empty}}},
		},
		{
			freeformToolStream(0, "text", "")[0],
			{Event: &pbv1.StreamEvent_ToolCallDelta{ToolCallDelta: &pbv1.ToolCallDelta{Index: 0, ArgumentsDelta: `{}`}}},
		},
	} {
		if err := validateAcceptedStream(events); err == nil || !strings.Contains(err.Error(), "payload") && !strings.Contains(err.Error(), "free-form") {
			t.Fatalf("cross-family delta error = %v", err)
		}
		walker := &streamDisciplineWalker{}
		if err := walker.walk(events[0]); err != nil {
			t.Fatal(err)
		}
		if err := walker.walk(events[1]); err == nil {
			t.Fatal("returned-side walker accepted cross-family delta")
		}
	}
}
