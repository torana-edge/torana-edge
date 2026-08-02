package main

import (
	"context"
	"encoding/json"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
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
	// v2 dropped HttpResponse.Handled: serving is an action, so returning a
	// ServeHTTP result IS handling it and PassHTTP() is declining. v1 needed
	// the flag because an all-defaults response marshals to zero bytes and was
	// indistinguishable from not answering.
	sdk.OnHTTPRequest(func(ctx context.Context, req *pb.HttpRequest) (sdk.HTTPResult, error) {
		if req.Path == "/agent/status" {
			body, err := json.Marshal(map[string]any{
				"plugin":      "test-http-server",
				"status":      "ready",
				"method":      req.Method,
				"query":       req.Query,
				"scheme":      req.Scheme,
				"remote_addr": req.RemoteAddr,
			})
			if err != nil {
				return sdk.PassHTTP(), err
			}
			return sdk.ServeHTTP(&pb.HttpResponse{
				Status:      200,
				HeadersJson: []byte(`{"Content-Type":["application/json"]}`),
				Body:        body,
			}), nil
		}
		if req.Path == "/echo" {
			// Fixture-only JSON echo for the plugin route. The browser page
			// stays exactly as it always was: raw caller input is never
			// reflected into HTML. This path exists so tests can assert the
			// forwarded fields byte-exactly.
			body, err := json.Marshal(map[string]any{
				"method":      req.Method,
				"path":        req.Path,
				"query":       req.Query,
				"scheme":      req.Scheme,
				"remote_addr": req.RemoteAddr,
			})
			if err != nil {
				return sdk.PassHTTP(), err
			}
			return sdk.ServeHTTP(&pb.HttpResponse{
				Status:      200,
				HeadersJson: []byte(`{"Content-Type":["application/json"]}`),
				Body:        body,
			}), nil
		}
		return sdk.ServeHTTP(&pb.HttpResponse{
			Status:      200,
			HeadersJson: []byte(`{"Content-Type":["text/html; charset=utf-8"]}`),
			Body:        []byte("<h1>test-http-server</h1><p>" + req.Method + " " + req.Path + "</p>"),
		}), nil
	})
}
