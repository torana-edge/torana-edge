package main

import (
	"context"
	"errors"
	"fmt"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		state, found, err := sdk.StateGet("seed")
		if err != nil {
			return sdk.RequestResult{}, err
		}
		if !found {
			return sdk.RequestResult{}, errors.New("state seed absent")
		}
		private, found, err := sdk.CacheGet("seed")
		if err != nil {
			return sdk.RequestResult{}, err
		}
		if !found {
			return sdk.RequestResult{}, errors.New("private cache seed absent")
		}
		shared, found, err := sdk.SharedCacheGet("seed")
		if err != nil {
			return sdk.RequestResult{}, err
		}
		if !found {
			return sdk.RequestResult{}, errors.New("shared cache seed absent")
		}
		text, err := sdk.ModelCompleteText(&pb.ModelCompleteArgs{Service: "summarizer", Messages: req.Messages})
		if err != nil {
			return sdk.RequestResult{}, err
		}
		response, err := sdk.HTTPRequest(&pb.OutboundHTTPRequestArgs{Endpoint: "lookup", Method: "POST", Path: "/lookup", Body: []byte(text)})
		if err != nil {
			var refusal *sdk.HostCallRefusalError
			if errors.As(err, &refusal) {
				req.Model = "refused:" + refusal.Code.String()
				return sdk.ReplaceRequest(req), nil
			}
			return sdk.RequestResult{}, err
		}
		req.Model = fmt.Sprintf("%s:%s:%s:%s:%s", state, private, shared, text, response.Body)
		return sdk.ReplaceRequest(req), nil
	})
}
