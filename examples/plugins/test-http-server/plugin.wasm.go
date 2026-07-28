package main

import (
	"context"
	"encoding/json"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

// Test fixture for the run_on_http_request ABI and the agent-operation dispatch
// path.
//
// It serves two shapes the host treats differently: an HTML page for a browser
// under /_torana/plugin/<name>/, and a JSON operation declared in agent.json
// whose response the host validates against the declared output schema. Both
// need env.serve_http, which is what makes this fixture the one that exercises
// the grant gate.
//
// Echoing the method and path back is deliberate: a test can then assert the
// host forwarded the request faithfully rather than merely that something
// answered.
func init() {
	sdk.OnHTTPRequest(func(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
		if req.Path == "/agent/status" {
			body, err := json.Marshal(map[string]any{
				"plugin": "test-http-server",
				"status": "ready",
				"method": req.Method,
			})
			if err != nil {
				return nil, err
			}
			return &pb.HttpResponse{
				Status:      200,
				HeadersJson: []byte(`{"Content-Type":["application/json"]}`),
				Body:        body,
				Handled:     true,
			}, nil
		}
		return &pb.HttpResponse{
			Status:      200,
			HeadersJson: []byte(`{"Content-Type":["text/html; charset=utf-8"]}`),
			Body:        []byte("<h1>test-http-server</h1><p>" + req.Method + " " + req.Path + "</p>"),
			Handled:     true,
		}, nil
	})
}
