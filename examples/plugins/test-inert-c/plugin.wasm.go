package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

// Benchmark fixture: does nothing at all, deliberately.
//
// Isolating the cost of one WASM boundary crossing needs a plugin that adds no
// guest work of its own. The other fixtures all do something — the blockers
// scan every message for a trigger, the observer makes a cache host call, the
// metrics fixture emits three metrics — so a 1-vs-3-plugin comparison built
// from them measures crossings AND guest work mixed together, and cannot
// attribute the difference to either.
//
// There are three identical copies (a, b, c) only because the pipeline keys
// plugins by name, so the same bundle cannot be loaded twice.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		return nil, nil
	})
}
