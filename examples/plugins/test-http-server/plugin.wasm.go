package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

type valueInput struct {
	Value string `json:"value"`
}

type valueUndo struct {
	Before        string `json:"before"`
	BeforePresent bool   `json:"before_present"`
	After         string `json:"after"`
	Undone        bool   `json:"undone,omitempty"`
}

func valueResponse(status int32, value string) (sdk.HTTPResult, error) {
	body, err := json.Marshal(valueInput{Value: value})
	if err != nil {
		return sdk.PassHTTP(), err
	}
	return sdk.ServeHTTP(&pb.HttpResponse{Status: status, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
}

func reversibleValue(req *pb.HttpRequest, undo bool) (sdk.HTTPResult, error) {
	binding, present, err := sdk.HTTPConversation(req)
	if err != nil || !present || !binding.Bound {
		return valueResponse(409, "")
	}
	var input valueInput
	if json.Unmarshal(req.Body, &input) != nil {
		return valueResponse(400, "")
	}
	sum := sha256.Sum256([]byte(binding.CallID))
	// Use the complete digest while keeping the state key independent of raw
	// provider call IDs.
	undoKey := "reversible/undo/" + hex.EncodeToString(sum[:])
	var record valueUndo
	foundRecord, err := sdk.StateGetJSON(undoKey, &record)
	if err != nil {
		return sdk.PassHTTP(), err
	}
	current, foundCurrent, err := sdk.StateGet("reversible/current")
	if err != nil {
		return sdk.PassHTTP(), err
	}
	if !undo {
		if foundRecord {
			if !record.Undone && record.After == input.Value && foundCurrent && current == record.After {
				return valueResponse(200, current)
			}
			return valueResponse(409, current)
		}
		record = valueUndo{Before: current, BeforePresent: foundCurrent, After: input.Value}
		if err := sdk.StateSetJSON(undoKey, record); err != nil {
			return sdk.PassHTTP(), err
		}
		if err := sdk.StateSet("reversible/current", input.Value); err != nil {
			_ = sdk.StateDelete(undoKey)
			return sdk.PassHTTP(), err
		}
		return valueResponse(200, input.Value)
	}
	if !foundRecord || record.After != input.Value {
		return valueResponse(409, current)
	}
	if record.Undone {
		if foundCurrent == record.BeforePresent && (!foundCurrent || current == record.Before) {
			return valueResponse(200, record.Before)
		}
		return valueResponse(409, current)
	}
	if !foundCurrent || current != record.After {
		return valueResponse(409, current)
	}
	if record.BeforePresent {
		err = sdk.StateSet("reversible/current", record.Before)
	} else {
		err = sdk.StateDelete("reversible/current")
	}
	if err != nil {
		return sdk.PassHTTP(), err
	}
	record.Undone = true
	if err := sdk.StateSetJSON(undoKey, record); err != nil {
		return sdk.PassHTTP(), err
	}
	return valueResponse(200, record.Before)
}

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
	// current ABI dropped HttpResponse.Handled: serving is an action, so returning a
	// ServeHTTP result IS handling it and PassHTTP() is declining. v1 needed
	// the flag because an all-defaults response marshals to zero bytes and was
	// indistinguishable from not answering.
	sdk.OnHTTPRequest(func(ctx context.Context, req *pb.HttpRequest) (sdk.HTTPResult, error) {
		// The forwarded headers, decoded: tests assert the three-class policy
		// output byte-exactly (canonical names, multi-values, absences).
		var headers map[string][]string
		if len(req.HeadersJson) > 0 {
			_ = json.Unmarshal(req.HeadersJson, &headers)
		}
		if headers == nil {
			headers = map[string][]string{}
		}
		if req.Path == "/agent/status" {
			body, err := json.Marshal(map[string]any{
				"plugin":      "test-http-server",
				"status":      "ready",
				"method":      req.Method,
				"query":       req.Query,
				"scheme":      req.Scheme,
				"remote_addr": req.RemoteAddr,
				"headers":     headers,
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
		if req.Path == "/agent/value" && req.Method == http.MethodPost {
			return reversibleValue(req, false)
		}
		if req.Path == "/agent/value/undo" && req.Method == http.MethodPost {
			return reversibleValue(req, true)
		}
		if req.Path == "/echo" {
			// Fixture-only JSON echo for the plugin route. The browser page
			// stays exactly as it always was: raw caller input is never
			// reflected into HTML. This path exists so tests can assert the
			// forwarded fields byte-exactly.
			// The cache read is the deterministic snapshot barrier: a
			// wrapping store can mutate the caller's raw header map inside
			// this host call, and the echo must still reflect the entry
			// snapshot.
			_, _, _ = sdk.CacheGet("header-barrier")
			body, err := json.Marshal(map[string]any{
				"method":      req.Method,
				"path":        req.Path,
				"query":       req.Query,
				"scheme":      req.Scheme,
				"remote_addr": req.RemoteAddr,
				"headers":     headers,
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
