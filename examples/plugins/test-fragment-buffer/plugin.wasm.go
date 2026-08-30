package main

import (
	"context"
	"encoding/json"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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
//
// It uses MetaAppend rather than meta_get + meta_set. The pair was two round
// trips and a lost update: two fragments interleaving between the read and the
// write silently drop one, and the corrupted tool call surfaces much later as
// invalid JSON reaching the agent. MetaAppend is one atomic call, keyed by
// block index — which also removes the hand-rolled key and the need to reset
// it, since current ABI block indexes are unique within a streamed message and never
// reused.

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		delta, ok := ev.Event.(*pb.StreamEvent_ToolCallDelta)
		if !ok || delta.ToolCallDelta == nil {
			return sdk.PassEvent(), nil
		}
		td := delta.ToolCallDelta

		// Persist BEFORE suppressing. An error between the two would lose the
		// fragment with no way to recover it.
		if _, herr, err := sdk.MetaAppend(td.Index, []byte(td.ArgumentsDelta)); err != nil {
			return sdk.PassEvent(), err
		} else if herr != nil {
			// The buffer is unreliable, so suppressing would truncate the tool
			// call. Passing the fragment through leaves it intact.
			return sdk.PassEvent(), nil
		}

		// An empty fragment reads back the complete buffer without appending.
		accumulated, herr, err := sdk.MetaAppend(td.Index, nil)
		if err != nil {
			return sdk.PassEvent(), err
		}
		if herr != nil {
			return sdk.PassEvent(), nil
		}

		// Incomplete: swallow the fragment and keep waiting. The host must
		// still be holding this state when the next chunk arrives.
		if !json.Valid(accumulated) {
			return sdk.SuppressEvent(), nil
		}

		// Complete. Emit the accumulated arguments in one delta, so a test can
		// tell this path ran rather than a fragment slipping through.
		return sdk.EmitEvents(&pb.StreamEvent{
			Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{
					Index:          td.Index,
					ArgumentsDelta: string(accumulated),
				},
			},
		}), nil
	})
}
