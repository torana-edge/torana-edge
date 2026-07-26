package main

import (
	"context"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		changed := false
		for _, m := range req.Messages {
			if m.Role == "user" && m.Content != "" && !strings.HasSuffix(m.Content, marker) {
				m.Content += marker
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
			return nil, nil
		}
		return req, nil
	})
}
