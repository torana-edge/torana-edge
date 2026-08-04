package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for the request-mutation path: it rewrites both messages and
// tool definitions, which is the combination the host's cache-control
// round-trip and dispatch tests need.
//
// Every mutation is a pure function of the input. That is not incidental — a
// mutation that varied between two identical requests would change the cached
// prefix on every turn and invalidate the provider's prompt cache, so the
// determinism test exists to catch exactly that. A fixture is the right place
// to pin it, because its output is predictable enough to assert byte for byte.
const marker = " [seen by test-mutator]"

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		changed := false
		for _, m := range req.Messages {
			if m.Role == "user" && blockutil.TextOf(m) != "" && !strings.HasSuffix(blockutil.TextOf(m), marker) {
				blockutil.SetText(m, blockutil.TextOf(m)+marker)
				changed = true
			}
		}
		// Touch a tool definition too, so a test can prove tool-level cache
		// breakpoints survive a plugin that rewrites tools.
		for _, t := range req.Tools {
			if t.Description == "" {
				t.Description = "described by test-mutator"
				changed = true
			}
		}
		if !changed {
			return sdk.PassRequest(), nil
		}
		return sdk.ReplaceRequest(req), nil
	})
}

// Response-side mutation, for testing the host's response locators.
//
// runJSONResponseHooks has to find a tool call inside four different provider
// body shapes, hand its arguments to a plugin, and write the result back
// without disturbing anything else. That is host mechanics — the plugin is only
// the thing that changes a value — so a fixture with a fixed, known
// substitution tests it far more precisely than a real plugin whose output
// depends on its own logic.
func init() {
	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		// A response carries exactly one message in v2, so there is no outer
		// loop and no question of which message the tool call belonged to.
		if resp.Message == nil {
			return sdk.PassResponse(), nil
		}
		var changed bool
		for _, tc := range resp.Message.ToolCalls {
			if tc == nil {
				continue
			}
			// A fixed replacement: the test asserts these exact bytes came
			// back, which proves the host both read and wrote the right place
			// in the body.
			tc.ArgumentsJson = []byte(`{"mutated_by":"test-mutator"}`)
			changed = true
		}
		if !changed {
			return sdk.PassResponse(), nil
		}
		return sdk.ReplaceResponse(resp), nil
	})
}
