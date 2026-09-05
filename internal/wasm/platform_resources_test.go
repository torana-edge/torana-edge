package wasm

import (
	"context"
	"testing"
	"time"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func marshalHostArgs(t *testing.T, message proto.Message) []byte {
	t.Helper()
	raw, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func hostCallValue(t *testing.T, result *pbv1.HostCallResult) []byte {
	t.Helper()
	value, ok := result.Result.(*pbv1.HostCallResult_Value)
	if !ok {
		t.Fatalf("host call returned %T, want value", result.Result)
	}
	return value.Value
}

func TestPlatformResourcesAreBoundToLoadedPlugin(t *testing.T) {
	r, p := newGrantedPlugin(t,
		"env.credential_get", "env.file_append", "env.file_read", "env.file_write",
		"env.file_list", "env.file_delete", "env.http_request", "env.model_complete", "env.model_pricing",
	)
	p.SetResources(PluginResources{
		Credentials: map[string]string{"service": "operator-credential-id"},
		Files: map[string]FileResource{
			"usage.jsonl": {Operations: map[string]bool{"append": true, "read": true, "write": true, "list": true, "delete": true}, MaxBytes: 1234, RetainedFiles: 2},
		},
		HTTP: map[string]HTTPResource{
			"billing": {Name: "billing", Origin: "https://billing.example", Methods: map[string]bool{"POST": true}, Timeout: 3 * time.Second, MaxRequestBytes: 11, MaxResponseBytes: 22, MaxCallsPerMinute: 7},
		},
		ModelServices:    map[string]ModelServiceResource{"summarizer": {Name: "summarizer", Provider: "model-provider", Model: "small-model", Path: "/v1/chat", Timeout: time.Second, MaxTokens: 32, MaxInputBytes: 1000, MaxCallsPerMinute: 2, MaxTokensPerHour: 100}},
		PricingResources: map[string]PricingResource{"request-price": {Name: "request-price", ForModelService: "summarizer", Prices: map[string]*pbv1.ModelPricing{PricingCoordinate("model-provider", "small-model"): {InputUsdPerMtok: ptrForTest(1.25)}}}},
	})

	r.CredentialGetFunc = func(_ context.Context, plugin, credentialID string) ([]byte, error) {
		if plugin != p.name || credentialID != "operator-credential-id" {
			t.Fatalf("credential callback = (%q, %q)", plugin, credentialID)
		}
		return []byte("secret-value"), nil
	}
	if got := string(hostCallValue(t, hostCallDirect(t, r, p, "env.credential_get", marshalHostArgs(t, &pbv1.CredentialGetArgs{Slot: "service"})))); got != "secret-value" {
		t.Fatalf("credential = %q", got)
	}

	var fileCalls []string
	r.FileAppendFunc = func(plugin, path string, data []byte, resource FileResource) error {
		fileCalls = append(fileCalls, "append:"+plugin+":"+path+":"+string(data))
		if resource.MaxBytes != 1234 || resource.RetainedFiles != 2 {
			t.Fatalf("append resource = %+v", resource)
		}
		return nil
	}
	r.FileReadFunc = func(plugin, path string, resource FileResource) ([]byte, error) {
		fileCalls = append(fileCalls, "read:"+plugin+":"+path)
		return []byte("line\n"), nil
	}
	r.FileWriteFunc = func(plugin, path string, data []byte, resource FileResource) error {
		fileCalls = append(fileCalls, "write:"+plugin+":"+path+":"+string(data))
		return nil
	}
	r.FileListFunc = func(plugin, prefix string, resources map[string]FileResource) ([]string, error) {
		fileCalls = append(fileCalls, "list:"+plugin+":"+prefix)
		if len(resources) != 1 || resources["usage.jsonl"].MaxBytes != 1234 {
			t.Fatalf("list resources = %+v", resources)
		}
		return []string{"usage.jsonl"}, nil
	}
	r.FileDeleteFunc = func(plugin, path string, resource FileResource) error {
		fileCalls = append(fileCalls, "delete:"+plugin+":"+path)
		return nil
	}

	hostCallValue(t, hostCallDirect(t, r, p, "env.file_append", marshalHostArgs(t, &pbv1.FileAppendArgs{Path: "usage.jsonl", Data: []byte("a")})))
	if got := string(hostCallValue(t, hostCallDirect(t, r, p, "env.file_read", marshalHostArgs(t, &pbv1.FileReadArgs{Path: "usage.jsonl"})))); got != "line\n" {
		t.Fatalf("read = %q", got)
	}
	hostCallValue(t, hostCallDirect(t, r, p, "env.file_write", marshalHostArgs(t, &pbv1.FileWriteArgs{Path: "usage.jsonl", Data: []byte("b")})))
	listRaw := hostCallValue(t, hostCallDirect(t, r, p, "env.file_list", marshalHostArgs(t, &pbv1.FileListArgs{})))
	var listed pbv1.FileListResult
	if err := proto.Unmarshal(listRaw, &listed); err != nil || len(listed.Paths) != 1 || listed.Paths[0] != "usage.jsonl" {
		t.Fatalf("list = %+v, err %v", listed.Paths, err)
	}
	hostCallValue(t, hostCallDirect(t, r, p, "env.file_delete", marshalHostArgs(t, &pbv1.FileDeleteArgs{Path: "usage.jsonl"})))
	if len(fileCalls) != 5 {
		t.Fatalf("file calls = %v", fileCalls)
	}

	r.HTTPRequestFunc = func(_ context.Context, plugin string, resource HTTPResource, request *pbv1.OutboundHTTPRequestArgs) (*pbv1.OutboundHTTPResponse, error) {
		if plugin != p.name || resource.Origin != "https://billing.example" || resource.MaxCallsPerMinute != 7 || request.Endpoint != "billing" || request.Path != "/usage" {
			t.Fatalf("HTTP callback = plugin %q resource %+v request %+v", plugin, resource, request)
		}
		return &pbv1.OutboundHTTPResponse{Status: 204}, nil
	}
	httpRaw := hostCallValue(t, hostCallDirect(t, r, p, "env.http_request", marshalHostArgs(t, &pbv1.OutboundHTTPRequestArgs{Endpoint: "billing", Method: "POST", Path: "/usage"})))
	var response pbv1.OutboundHTTPResponse
	if err := proto.Unmarshal(httpRaw, &response); err != nil || response.Status != 204 {
		t.Fatalf("HTTP response status = %d, err %v", response.Status, err)
	}

	r.ModelCompleteFunc = func(_ context.Context, plugin string, resource ModelServiceResource, request *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError) {
		if plugin != p.name || resource.Provider != "model-provider" || resource.Model != "small-model" || request.Service != "summarizer" {
			t.Fatalf("model callback = plugin %q resource %+v request %+v", plugin, resource, request)
		}
		return &pbv1.ModelCompleteResult{Content: "summary"}, nil
	}
	modelRaw := hostCallValue(t, hostCallDirect(t, r, p, "env.model_complete", marshalHostArgs(t, &pbv1.ModelCompleteArgs{Service: "summarizer", Messages: []*pbv1.ModelMessage{{Role: "user", Content: "hello"}}})))
	var modelResult pbv1.ModelCompleteResult
	if err := proto.Unmarshal(modelRaw, &modelResult); err != nil || modelResult.Content != "summary" {
		t.Fatalf("model result content = %q, err %v", modelResult.Content, err)
	}
	r.ModelPricingFunc = func(_ context.Context, plugin string, resource PricingResource) (*pbv1.ModelPricing, *pbv1.HostError) {
		if plugin != p.name || resource.Name != "request-price" {
			t.Fatalf("pricing callback = %q %+v", plugin, resource)
		}
		return proto.Clone(resource.Prices[PricingCoordinate("model-provider", "small-model")]).(*pbv1.ModelPricing), nil
	}
	pricingRaw := hostCallValue(t, hostCallDirect(t, r, p, "env.model_pricing", marshalHostArgs(t, &pbv1.ModelPricingGetArgs{Resource: "request-price"})))
	var pricing pbv1.ModelPricing
	if err := proto.Unmarshal(pricingRaw, &pricing); err != nil || pricing.InputUsdPerMtok == nil || *pricing.InputUsdPerMtok != 1.25 {
		t.Fatalf("pricing input rate = %v, err %v", pricing.InputUsdPerMtok, err)
	}
}

func ptrForTest[T any](value T) *T { return &value }

func TestPricingCoordinateIsPositionFramed(t *testing.T) {
	left := PricingCoordinate("a\x00b", "c")
	right := PricingCoordinate("a", "b\x00c")
	if left == right {
		t.Fatal("provider/model boundary collision")
	}
	if left != PricingCoordinate("a\x00b", "c") {
		t.Fatal("pricing coordinate is not deterministic")
	}
}

func TestPlatformResourceNamesCannotEscapeApproval(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.credential_get", "env.file_append", "env.http_request", "env.model_complete", "env.model_pricing")
	p.SetResources(PluginResources{
		Credentials:      map[string]string{"approved": "credential-id"},
		Files:            map[string]FileResource{"approved.log": {Operations: map[string]bool{"append": true}, MaxBytes: 100}},
		HTTP:             map[string]HTTPResource{"approved": {Methods: map[string]bool{"GET": true}}},
		ModelServices:    map[string]ModelServiceResource{"approved": {Name: "approved"}},
		PricingResources: map[string]PricingResource{"approved": {Name: "approved", Prices: map[string]*pbv1.ModelPricing{PricingCoordinate("p", "m"): {}}}},
	})
	for _, call := range []struct {
		command string
		args    proto.Message
	}{
		{command: "env.credential_get", args: &pbv1.CredentialGetArgs{Slot: "other"}},
		{command: "env.file_append", args: &pbv1.FileAppendArgs{Path: "other.log", Data: []byte("x")}},
		{command: "env.http_request", args: &pbv1.OutboundHTTPRequestArgs{Endpoint: "other", Method: "GET", Path: "/"}},
		{command: "env.http_request", args: &pbv1.OutboundHTTPRequestArgs{Endpoint: "approved", Method: "POST", Path: "/"}},
		{command: "env.model_complete", args: &pbv1.ModelCompleteArgs{Service: "other", Messages: []*pbv1.ModelMessage{{Role: "user", Content: "x"}}}},
		{command: "env.model_pricing", args: &pbv1.ModelPricingGetArgs{Resource: "other"}},
	} {
		result := hostCallDirect(t, r, p, call.command, marshalHostArgs(t, call.args))
		errArm, ok := result.Result.(*pbv1.HostCallResult_Error)
		if !ok || errArm.Error.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatalf("%s returned %+v, want PERMISSION_DENIED", call.command, result.Result)
		}
	}
}
