package main

import (
	"context"
	"encoding/json"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func main() {}

// Test fixture: produces a REQUEST replacement whose Code Assist envelope
// smuggles the canonical outer `model` member through the extras path —
// provider-invalid output the host must attribute to THIS plugin (pass
// rolls back, block refuses). Only fires on Code Assist requests (the
// envelope has a `request` member).
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		out := proto.Clone(req).(*pb.ChatRequest)
		var m map[string]any
		if err := json.Unmarshal(out.ProviderExtensionsJson, &m); err != nil {
			return sdk.PassRequest(), nil
		}
		if _, ok := m["request"]; !ok {
			return sdk.PassRequest(), nil
		}
		m["model"] = "smuggled-model"
		raw, err := json.Marshal(m)
		if err != nil {
			return sdk.PassRequest(), nil
		}
		out.ProviderExtensionsJson = raw
		return sdk.ReplaceRequest(out), nil
	})
}
