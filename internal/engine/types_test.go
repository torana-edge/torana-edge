package engine

import (
	"testing"
)

func TestStreamEvent_OneField(t *testing.T) {
	// TextDelta set, all others nil/zero.
	s1 := "hello"
	ev := StreamEvent{TextDelta: &s1}
	if ev.TextDelta == nil || *ev.TextDelta != "hello" {
		t.Errorf("TextDelta not set correctly")
	}
	if ev.ToolCallStart != nil {
		t.Errorf("ToolCallStart should be nil when TextDelta is set")
	}
	if ev.ToolCallDelta != nil {
		t.Errorf("ToolCallDelta should be nil when TextDelta is set")
	}
	if ev.ToolCallEnd != nil {
		t.Errorf("ToolCallEnd should be nil when TextDelta is set")
	}
	if ev.FinishReason != "" {
		t.Errorf("FinishReason should be empty when TextDelta is set")
	}
	if ev.Error != nil {
		t.Errorf("Error should be nil when TextDelta is set")
	}

	// ToolCallStart set, all others nil/zero.
	ev2 := StreamEvent{ToolCallStart: &ToolCallStart{Index: 0, ID: "tc1", Name: "myfunc"}}
	if ev2.TextDelta != nil {
		t.Errorf("TextDelta should be nil when ToolCallStart is set")
	}
	if ev2.ToolCallStart == nil || ev2.ToolCallStart.Name != "myfunc" {
		t.Errorf("ToolCallStart not set correctly")
	}
	if ev2.ToolCallDelta != nil {
		t.Errorf("ToolCallDelta should be nil when ToolCallStart is set")
	}
	if ev2.ToolCallEnd != nil {
		t.Errorf("ToolCallEnd should be nil when ToolCallStart is set")
	}
	if ev2.FinishReason != "" {
		t.Errorf("FinishReason should be empty when ToolCallStart is set")
	}
	if ev2.Error != nil {
		t.Errorf("Error should be nil when ToolCallStart is set")
	}

	// ToolCallDelta set, all others nil/zero.
	ev3 := StreamEvent{ToolCallDelta: &ToolCallDelta{Index: 1, ArgumentsDelta: `{"key":`}}
	if ev3.TextDelta != nil {
		t.Errorf("TextDelta should be nil when ToolCallDelta is set")
	}
	if ev3.ToolCallStart != nil {
		t.Errorf("ToolCallStart should be nil when ToolCallDelta is set")
	}
	if ev3.ToolCallDelta == nil || ev3.ToolCallDelta.ArgumentsDelta != `{"key":` {
		t.Errorf("ToolCallDelta not set correctly")
	}
	if ev3.ToolCallEnd != nil {
		t.Errorf("ToolCallEnd should be nil when ToolCallDelta is set")
	}
	if ev3.FinishReason != "" {
		t.Errorf("FinishReason should be empty when ToolCallDelta is set")
	}
	if ev3.Error != nil {
		t.Errorf("Error should be nil when ToolCallDelta is set")
	}

	// ToolCallEnd set, all others nil/zero.
	ev4 := StreamEvent{ToolCallEnd: &ToolCallEnd{Index: 0}}
	if ev4.TextDelta != nil {
		t.Errorf("TextDelta should be nil when ToolCallEnd is set")
	}
	if ev4.ToolCallStart != nil {
		t.Errorf("ToolCallStart should be nil when ToolCallEnd is set")
	}
	if ev4.ToolCallDelta != nil {
		t.Errorf("ToolCallDelta should be nil when ToolCallEnd is set")
	}
	if ev4.ToolCallEnd == nil || ev4.ToolCallEnd.Index != 0 {
		t.Errorf("ToolCallEnd not set correctly")
	}
	if ev4.FinishReason != "" {
		t.Errorf("FinishReason should be empty when ToolCallEnd is set")
	}
	if ev4.Error != nil {
		t.Errorf("Error should be nil when ToolCallEnd is set")
	}

	// FinishReason set, all others nil/zero.
	ev5 := StreamEvent{FinishReason: "stop"}
	if ev5.TextDelta != nil {
		t.Errorf("TextDelta should be nil when FinishReason is set")
	}
	if ev5.ToolCallStart != nil {
		t.Errorf("ToolCallStart should be nil when FinishReason is set")
	}
	if ev5.ToolCallDelta != nil {
		t.Errorf("ToolCallDelta should be nil when FinishReason is set")
	}
	if ev5.ToolCallEnd != nil {
		t.Errorf("ToolCallEnd should be nil when FinishReason is set")
	}
	if ev5.FinishReason != "stop" {
		t.Errorf("FinishReason not set correctly")
	}
	if ev5.Error != nil {
		t.Errorf("Error should be nil when FinishReason is set")
	}

	// Error set, all others nil/zero.
	ev6 := StreamEvent{Error: &StreamError{Code: 500, Message: "internal error"}}
	if ev6.TextDelta != nil {
		t.Errorf("TextDelta should be nil when Error is set")
	}
	if ev6.ToolCallStart != nil {
		t.Errorf("ToolCallStart should be nil when Error is set")
	}
	if ev6.ToolCallDelta != nil {
		t.Errorf("ToolCallDelta should be nil when Error is set")
	}
	if ev6.ToolCallEnd != nil {
		t.Errorf("ToolCallEnd should be nil when Error is set")
	}
	if ev6.FinishReason != "" {
		t.Errorf("FinishReason should be empty when Error is set")
	}
	if ev6.Error == nil || ev6.Error.Code != 500 || ev6.Error.Message != "internal error" {
		t.Errorf("Error not set correctly")
	}
}

func TestRoleConstants(t *testing.T) {
	if RoleSystem != "system" {
		t.Errorf("RoleSystem = %q, want %q", RoleSystem, "system")
	}
	if RoleUser != "user" {
		t.Errorf("RoleUser = %q, want %q", RoleUser, "user")
	}
	if RoleAssistant != "assistant" {
		t.Errorf("RoleAssistant = %q, want %q", RoleAssistant, "assistant")
	}
	if RoleTool != "tool" {
		t.Errorf("RoleTool = %q, want %q", RoleTool, "tool")
	}
}
