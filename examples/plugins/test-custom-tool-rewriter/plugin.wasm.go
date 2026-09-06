package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

func init() {
	sdk.NewStreamHandler().OnToolCall(func(_ context.Context, call sdk.ToolCall) (sdk.ToolCallAction, error) {
		if call.InvocationKind != pb.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM {
			return sdk.PassToolCall(), nil
		}
		return sdk.ReplaceToolInput("rewritten free-form input"), nil
	}).Register()
}
