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
	if ev.BlockStart != nil {
		t.Errorf("BlockStart should be nil when TextDelta is set")
	}
	if ev.BlockStop != nil {
		t.Errorf("BlockStop should be nil when TextDelta is set")
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
	if ev2.BlockStart != nil {
		t.Errorf("BlockStart should be nil when ToolCallStart is set")
	}
	if ev2.BlockStop != nil {
		t.Errorf("BlockStop should be nil when ToolCallStart is set")
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
	if ev3.BlockStart != nil {
		t.Errorf("BlockStart should be nil when ToolCallDelta is set")
	}
	if ev3.BlockStop != nil {
		t.Errorf("BlockStop should be nil when ToolCallDelta is set")
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
	if ev4.BlockStart != nil {
		t.Errorf("BlockStart should be nil when ToolCallEnd is set")
	}
	if ev4.BlockStop != nil {
		t.Errorf("BlockStop should be nil when ToolCallEnd is set")
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

	// BlockStart set, all others nil/zero.
	ev7 := StreamEvent{BlockStart: &BlockStart{Index: 3, Kind: BlockKindThinking}}
	if ev7.BlockStart == nil || ev7.BlockStart.Index != 3 || ev7.BlockStart.Kind != BlockKindThinking {
		t.Errorf("BlockStart not set correctly")
	}
	if ev7.TextDelta != nil {
		t.Errorf("TextDelta should be nil when BlockStart is set")
	}
	if ev7.BlockStop != nil {
		t.Errorf("BlockStop should be nil when BlockStart is set")
	}
	if ev7.ToolCallStart != nil {
		t.Errorf("ToolCallStart should be nil when BlockStart is set")
	}
	if ev7.ToolCallDelta != nil {
		t.Errorf("ToolCallDelta should be nil when BlockStart is set")
	}
	if ev7.ToolCallEnd != nil {
		t.Errorf("ToolCallEnd should be nil when BlockStart is set")
	}
	if ev7.FinishReason != "" {
		t.Errorf("FinishReason should be empty when BlockStart is set")
	}
	if ev7.Error != nil {
		t.Errorf("Error should be nil when BlockStart is set")
	}

	// BlockStop set, all others nil/zero.
	ev8 := StreamEvent{BlockStop: &BlockStop{Index: 3}}
	if ev8.BlockStop == nil || ev8.BlockStop.Index != 3 {
		t.Errorf("BlockStop not set correctly")
	}
	if ev8.TextDelta != nil {
		t.Errorf("TextDelta should be nil when BlockStop is set")
	}
	if ev8.BlockStart != nil {
		t.Errorf("BlockStart should be nil when BlockStop is set")
	}
	if ev8.ToolCallStart != nil {
		t.Errorf("ToolCallStart should be nil when BlockStop is set")
	}
	if ev8.ToolCallDelta != nil {
		t.Errorf("ToolCallDelta should be nil when BlockStop is set")
	}
	if ev8.ToolCallEnd != nil {
		t.Errorf("ToolCallEnd should be nil when BlockStop is set")
	}
	if ev8.FinishReason != "" {
		t.Errorf("FinishReason should be empty when BlockStop is set")
	}
	if ev8.Error != nil {
		t.Errorf("Error should be nil when BlockStop is set")
	}

	// FinishReason set, all others nil/zero.
	ev5 := StreamEvent{FinishReason: "stop"}
	if ev5.TextDelta != nil {
		t.Errorf("TextDelta should be nil when FinishReason is set")
	}
	if ev5.BlockStart != nil {
		t.Errorf("BlockStart should be nil when FinishReason is set")
	}
	if ev5.BlockStop != nil {
		t.Errorf("BlockStop should be nil when FinishReason is set")
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
	if ev6.BlockStart != nil {
		t.Errorf("BlockStart should be nil when Error is set")
	}
	if ev6.BlockStop != nil {
		t.Errorf("BlockStop should be nil when Error is set")
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
