package main

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for streaming state that must survive a pipeline hot reload.
//
// A plugin that rewrites tool-call arguments cannot act on a single chunk: the
// arguments arrive as fragments and are only meaningful once complete. So it
// suppresses each fragment, accumulates them in REQUEST-SCOPED host state, and
// emits the whole thing at the end.
//
// That makes it the right shape for testing a host property: a pipeline swap
// mid-stream must not drain the runtime holding an in-flight request's state.
// If it does, the buffer is gone and the arguments never arrive — which is
// visible from outside without knowing anything about what the plugin would
// have done to them.
//
// The accumulation is verbatim. A fixture that also transformed the arguments
// would make a failure ambiguous between "state was lost" and "the transform
// changed".

func bufferKey(index int32) string {
	return fmt.Sprintf("fragbuf:%d", index)
}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		delta, ok := ev.Event.(*pb.StreamEvent_ToolCallDelta)
		if !ok || delta.ToolCallDelta == nil {
			return sdk.PassEvent(), nil
		}

		td := delta.ToolCallDelta
		key := bufferKey(td.Index)

		prev, _ := sdk.HostCall("env.meta_get", key)
		accumulated := prev + td.ArgumentsDelta

		// Incomplete: swallow the fragment and keep waiting. The host must
		// still be holding this state when the next chunk arrives.
		if !json.Valid([]byte(accumulated)) {
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":%q,"value":%q}`, key, accumulated))
			return sdk.Emit(), nil
		}

		// Complete. Emit the accumulated arguments in one delta, tagged so a
		// test can tell this path ran rather than a fragment slipping through.
		sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":%q,"value":%q}`, key, ""))
		return sdk.Emit(&pb.StreamEvent{
			Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{
					Index:          td.Index,
					ArgumentsDelta: accumulated,
				},
			},
		}), nil
	})
}
