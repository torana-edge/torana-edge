package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture: exercises the SDK tool-result seam helpers through a real
// guest.
//   - model "change": ReplaceToolResultText on the LAST tool-result block
//     with a changed text (the helper clears the result signature and any
//     final trailing-signature block);
//   - model "noop": ReplaceToolResultText with the SAME text — the
//     byte-identical helper call is a pass/no-op and every provenance token
//     survives.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		last := len(req.Messages) - 1
		for ; last >= 0; last-- {
			for bi := len(req.Messages[last].Blocks) - 1; bi >= 0; bi-- {
				if tr := req.Messages[last].Blocks[bi].GetToolResult(); tr != nil {
					for _, c := range tr.Content {
						if c.GetText() != nil {
							switch req.Model {
							case "change":
								changed, err := sdk.ReplaceToolResultText(req.Messages[last], bi, "changed-by-helper")
								if err != nil {
									return sdk.RequestResult{}, err
								}
								if !changed {
									return sdk.PassRequest(), nil
								}
								return sdk.ReplaceRequest(req), nil
							case "noop":
								_, err := sdk.ReplaceToolResultText(req.Messages[last], bi, c.GetText().Text)
								if err != nil {
									return sdk.RequestResult{}, err
								}
								return sdk.PassRequest(), nil
							}
						}
					}
				}
			}
		}
		return sdk.PassRequest(), nil
	})
}
