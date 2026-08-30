package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Test fixture: exercises the SDK tool-result seam helper through a real
// guest.
//   - model "change": ReplaceToolResultText on the LAST tool-result block
//     with a changed text (the helper clears ONLY the containing result
//     signature; the trailing carrier, unrelated tokens and sibling results
//     are preserved byte-for-byte);
//   - model "change-json": a VALID JSON-object replacement (the gemini wire
//     re-emits the raw response bytes, so the text must stay a JSON object);
//   - model "noop": ReplaceToolResultText with the SAME text — the
//     byte-identical helper call is a pass/no-op and every provenance token
//     survives.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		// The FIRST tool-result block with a text arm is the designated
		// candidate (deterministic across runs).
		for mi, m := range req.Messages {
			for bi, b := range m.Blocks {
				if tr := b.GetToolResult(); tr != nil {
					for _, c := range tr.Content {
						if c.GetText() != nil {
							switch req.Model {
							case "change":
								changed, err := sdk.ReplaceToolResultText(req.Messages[mi], bi, "changed-by-helper")
								if err != nil {
									return sdk.RequestResult{}, err
								}
								if !changed {
									return sdk.PassRequest(), nil
								}
								return sdk.ReplaceRequest(req), nil
							case "change-json":
								changed, err := sdk.ReplaceToolResultText(req.Messages[mi], bi, `{"output":"changed"}`)
								if err != nil {
									return sdk.RequestResult{}, err
								}
								if !changed {
									return sdk.PassRequest(), nil
								}
								return sdk.ReplaceRequest(req), nil
							case "noop":
								_, err := sdk.ReplaceToolResultText(req.Messages[mi], bi, c.GetText().Text)
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
