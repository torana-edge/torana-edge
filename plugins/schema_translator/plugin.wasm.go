package main

import (
	"context"

	"encoding/json"
	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		modified := false
		for _, t := range req.Tools {
			if len(t.ParametersJson) == 0 {
				continue
			}
			var params map[string]any
			if err := json.Unmarshal(t.ParametersJson, &params); err != nil {
				continue
			}

			propsAny, _ := params["properties"].(map[string]any)
			if propsAny == nil {
				continue
			}

			if _, ok := propsAny["i"]; !ok {
				propsAny["i"] = map[string]any{"type": "string", "description": "what you intend to accomplish"}
				if reqAny, ok := params["required"].([]any); ok {
					params["required"] = append(reqAny, "i")
				}

				newParamsJson, err := json.Marshal(params)
				if err == nil {
					t.ParametersJson = newParamsJson
					modified = true
				}
			}
		}

		if !modified {
			return nil, nil
		}
		return req, nil
	})
}
