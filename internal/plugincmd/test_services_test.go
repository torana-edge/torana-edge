package plugincmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func serviceJSON(t *testing.T, message proto.Message) json.RawMessage {
	t.Helper()
	raw, err := protojson.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func serviceBundle() *plugin.PluginBundle {
	return &plugin.PluginBundle{Digest: "sha256:test", Manifest: plugin.PluginManifest{
		Name:          "fixture-plugin",
		Permissions:   []plugin.Permission{{Name: "env.http_request"}, {Name: "env.model_complete"}},
		HTTPEndpoints: []plugin.HTTPEndpointDeclaration{{Name: "api", Methods: []string{"POST"}, MaxCallsPerMinute: 3}},
		ModelServices: []plugin.ModelServiceDeclaration{{Name: "classifier", TimeoutMS: 1000, MaxTokens: 64, MaxInputBytes: 4096, MaxCallsPerMinute: 3, MaxTokensPerHour: 1000}},
	}}
}

func TestScenarioServicesMatchingFIFO(t *testing.T) {
	req := &pbv1.OutboundHTTPRequestArgs{Endpoint: "api", Method: "POST", Path: "/check", Body: []byte("raw bytes")}
	resp := &pbv1.OutboundHTTPResponse{Status: 204}
	services := &scenarioServices{HTTP: map[string][]scenarioHTTPFixture{"api": {{Request: serviceJSON(t, req), Response: serviceJSON(t, resp)}}}}
	rt, config, finish, err := buildScenarioRuntime(context.Background(), serviceBundle(), services, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if config.AllowUnapproved || config.Approvals["fixture-plugin"].Digest != "sha256:test" {
		t.Fatalf("runtime was not digest approved: %+v", config)
	}
	got, callErr := rt.HTTPRequestFunc(context.Background(), "fixture-plugin", wasm.HTTPResource{Name: "api"}, req)
	if callErr != nil || got.GetStatus() != 204 {
		t.Fatalf("HTTP fixture: response=%v error=%v", got, callErr)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
}

func TestScenarioServicesReportsMismatch(t *testing.T) {
	want := &pbv1.ModelCompleteArgs{Service: "classifier", Messages: []*pbv1.Message{{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "expected"}}}}}}}
	response := &pbv1.ModelCompleteResult{Message: &pbv1.ResponseMessage{Blocks: []*pbv1.ResponseBlock{{Kind: &pbv1.ResponseBlock_Text{Text: &pbv1.ResponseTextBlock{Text: "ok"}}}}}}
	services := &scenarioServices{Model: map[string][]scenarioModelFixture{"classifier": {{Request: serviceJSON(t, want), Response: serviceJSON(t, response)}}}}
	rt, _, finish, err := buildScenarioRuntime(context.Background(), serviceBundle(), services, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := &pbv1.ModelCompleteArgs{Service: "classifier", Messages: []*pbv1.Message{{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "different"}}}}}}}
	if _, hostErr := rt.ModelCompleteFunc(context.Background(), "fixture-plugin", wasm.ModelServiceResource{Name: "classifier"}, got); hostErr == nil {
		t.Fatal("mismatching callback succeeded")
	}
	if err := finish(); err == nil || !strings.Contains(err.Error(), "request mismatch") {
		t.Fatalf("finish error = %v", err)
	}
}

func TestScenarioServicesReportsUnusedFixture(t *testing.T) {
	req := &pbv1.OutboundHTTPRequestArgs{Endpoint: "api", Method: "POST", Path: "/unused"}
	resp := &pbv1.OutboundHTTPResponse{Status: 200}
	services := &scenarioServices{HTTP: map[string][]scenarioHTTPFixture{"api": {{Request: serviceJSON(t, req), Response: serviceJSON(t, resp)}}}}
	_, _, finish, err := buildScenarioRuntime(context.Background(), serviceBundle(), services, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(); err == nil || !strings.Contains(err.Error(), "1 unused fixture") {
		t.Fatalf("finish error = %v", err)
	}
}
