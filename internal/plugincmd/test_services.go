package plugincmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// scenarioServices is the closed, deterministic host-service surface available
// to `plugin test`. HTTP and model entries are FIFO: each guest call consumes
// exactly one fixture for its manifest slot.
type scenarioServices struct {
	Cache scenarioCacheServices             `json:"cache,omitempty"`
	State map[string]string                 `json:"state,omitempty"`
	HTTP  map[string][]scenarioHTTPFixture  `json:"http,omitempty"`
	Model map[string][]scenarioModelFixture `json:"model,omitempty"`
}

type scenarioCacheServices struct {
	Private map[string]string `json:"private,omitempty"`
	Shared  map[string]string `json:"shared,omitempty"`
}

type scenarioHTTPFixture struct {
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}

type scenarioModelFixture struct {
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}

type preparedHTTPFixture struct {
	request  *pbv1.OutboundHTTPRequestArgs
	response *pbv1.OutboundHTTPResponse
	hostErr  *pbv1.HostError
}

type preparedModelFixture struct {
	request  *pbv1.ModelCompleteArgs
	response *pbv1.ModelCompleteResult
	hostErr  *pbv1.HostError
}

type scenarioFixtureError struct{ host *pbv1.HostError }

func (e *scenarioFixtureError) Error() string { return e.host.GetMessage() }
func (e *scenarioFixtureError) HostError() *pbv1.HostError {
	return proto.Clone(e.host).(*pbv1.HostError)
}

func decodeServiceProto(raw json.RawMessage, dst proto.Message, label string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("%s is required", label)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	if validator, ok := dst.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return fmt.Errorf("validate %s: %w", label, err)
		}
	}
	return nil
}

// buildScenarioRuntime constructs the same runtime/cache/state stack used by
// production, but installs only deterministic callbacks named by the scenario.
func buildScenarioRuntime(ctx context.Context, bundle *plugin.PluginBundle, services *scenarioServices, stagedRoot string) (*wasm.Runtime, plugin.PluginConfig, func() error, error) {
	if bundle == nil {
		return nil, plugin.PluginConfig{}, nil, errors.New("plugin bundle is required")
	}
	if services == nil {
		services = &scenarioServices{}
	}

	httpFixtures := make(map[string][]preparedHTTPFixture, len(services.HTTP))
	modelFixtures := make(map[string][]preparedModelFixture, len(services.Model))
	declaredHTTP := make(map[string]plugin.HTTPEndpointDeclaration, len(bundle.Manifest.HTTPEndpoints))
	for _, declaration := range bundle.Manifest.HTTPEndpoints {
		declaredHTTP[declaration.Name] = declaration
	}
	declaredModels := make(map[string]plugin.ModelServiceDeclaration, len(bundle.Manifest.ModelServices))
	for _, declaration := range bundle.Manifest.ModelServices {
		declaredModels[declaration.Name] = declaration
	}
	for slot, fixtures := range services.HTTP {
		if _, ok := declaredHTTP[slot]; !ok {
			return nil, plugin.PluginConfig{}, nil, fmt.Errorf("HTTP fixture slot %q is not declared by manifest", slot)
		}
		for i, fixture := range fixtures {
			var prepared preparedHTTPFixture
			prepared.request = &pbv1.OutboundHTTPRequestArgs{}
			if err := decodeServiceProto(fixture.Request, prepared.request, fmt.Sprintf("HTTP fixture %q[%d].request", slot, i)); err != nil {
				return nil, plugin.PluginConfig{}, nil, err
			}
			if (len(fixture.Response) == 0) == (len(fixture.Error) == 0) {
				return nil, plugin.PluginConfig{}, nil, fmt.Errorf("HTTP fixture %q[%d] must contain exactly one of response or error", slot, i)
			}
			if len(fixture.Response) != 0 {
				prepared.response = &pbv1.OutboundHTTPResponse{}
				if err := decodeServiceProto(fixture.Response, prepared.response, fmt.Sprintf("HTTP fixture %q[%d].response", slot, i)); err != nil {
					return nil, plugin.PluginConfig{}, nil, err
				}
			}
			if len(fixture.Error) != 0 {
				prepared.hostErr = &pbv1.HostError{}
				if err := decodeServiceProto(fixture.Error, prepared.hostErr, fmt.Sprintf("HTTP fixture %q[%d].error", slot, i)); err != nil {
					return nil, plugin.PluginConfig{}, nil, err
				}
			}
			httpFixtures[slot] = append(httpFixtures[slot], prepared)
		}
	}
	for slot, fixtures := range services.Model {
		if _, ok := declaredModels[slot]; !ok {
			return nil, plugin.PluginConfig{}, nil, fmt.Errorf("model fixture slot %q is not declared by manifest", slot)
		}
		for i, fixture := range fixtures {
			prepared := preparedModelFixture{request: &pbv1.ModelCompleteArgs{}}
			if err := decodeServiceProto(fixture.Request, prepared.request, fmt.Sprintf("model fixture %q[%d].request", slot, i)); err != nil {
				return nil, plugin.PluginConfig{}, nil, err
			}
			if (len(fixture.Response) == 0) == (len(fixture.Error) == 0) {
				return nil, plugin.PluginConfig{}, nil, fmt.Errorf("model fixture %q[%d] must contain exactly one of response or error", slot, i)
			}
			if len(fixture.Response) != 0 {
				prepared.response = &pbv1.ModelCompleteResult{}
				if err := decodeServiceProto(fixture.Response, prepared.response, fmt.Sprintf("model fixture %q[%d].response", slot, i)); err != nil {
					return nil, plugin.PluginConfig{}, nil, err
				}
			}
			if len(fixture.Error) != 0 {
				prepared.hostErr = &pbv1.HostError{}
				if err := decodeServiceProto(fixture.Error, prepared.hostErr, fmt.Sprintf("model fixture %q[%d].error", slot, i)); err != nil {
					return nil, plugin.PluginConfig{}, nil, err
				}
			}
			modelFixtures[slot] = append(modelFixtures[slot], prepared)
		}
	}

	approval := plugin.Approval{Digest: bundle.Digest}
	for _, permission := range bundle.Manifest.Permissions {
		approval.Permissions = append(approval.Permissions, permission.Name)
	}
	approval.HTTPEndpoints = make(map[string]plugin.HTTPApproval, len(httpFixtures))
	for slot := range httpFixtures {
		d := declaredHTTP[slot]
		calls := d.MaxCallsPerMinute
		if calls == 0 {
			calls = len(httpFixtures[slot])
			if calls == 0 {
				calls = 1
			}
		}
		approval.HTTPEndpoints[slot] = plugin.HTTPApproval{Origin: "https://fixture.invalid", Methods: append([]string(nil), d.Methods...), TimeoutMS: d.TimeoutMS, MaxRequestBytes: d.MaxRequestBytes, MaxResponseBytes: d.MaxResponseBytes, MaxCallsPerMinute: calls}
	}
	approval.ModelServices = make(map[string]plugin.ModelServiceApproval, len(modelFixtures))
	for slot := range modelFixtures {
		d := declaredModels[slot]
		approval.ModelServices[slot] = plugin.ModelServiceApproval{Provider: "fixture", Model: "fixture", Path: "/v1/complete", TimeoutMS: d.TimeoutMS, MaxTokens: d.MaxTokens, MaxInputBytes: d.MaxInputBytes, MaxCallsPerMinute: d.MaxCallsPerMinute, MaxTokensPerHour: d.MaxTokensPerHour}
	}
	config := plugin.PluginConfig{Dir: stagedRoot, Order: []string{bundle.Manifest.Name}, Config: map[string]json.RawMessage{}, Approvals: map[string]plugin.Approval{bundle.Manifest.Name: approval}, Strict: true}

	store := cache.NewLocalCache(time.Hour)
	stateDir, err := os.MkdirTemp("", "torana-plugin-test-state-")
	if err != nil {
		store.Close()
		return nil, plugin.PluginConfig{}, nil, fmt.Errorf("create scenario state directory: %w", err)
	}
	state, err := pluginstate.New(pluginstate.Options{Path: filepath.Join(stateDir, "state.json")})
	if err != nil {
		store.Close()
		_ = os.RemoveAll(stateDir)
		return nil, plugin.PluginConfig{}, nil, err
	}
	for key, value := range services.State {
		if err := state.Set(bundle.Manifest.Name, key, value); err != nil {
			store.Close()
			_ = os.RemoveAll(stateDir)
			return nil, plugin.PluginConfig{}, nil, fmt.Errorf("initialize state %q: %w", key, err)
		}
	}

	resources := wasm.PluginResources{Credentials: map[string]string{}, Files: map[string]wasm.FileResource{}, HTTP: map[string]wasm.HTTPResource{}, ModelServices: map[string]wasm.ModelServiceResource{}, PricingResources: map[string]wasm.PricingResource{}, PromptCachePolicies: map[string]wasm.PromptCacheResource{}}
	for slot, a := range approval.HTTPEndpoints {
		timeout, maxRequest, maxResponse := a.TimeoutMS, a.MaxRequestBytes, a.MaxResponseBytes
		if timeout == 0 {
			timeout = 5000
		}
		if maxRequest == 0 {
			maxRequest = 1 << 20
		}
		if maxResponse == 0 {
			maxResponse = 4 << 20
		}
		methods := map[string]bool{}
		for _, method := range a.Methods {
			methods[method] = true
		}
		resources.HTTP[slot] = wasm.HTTPResource{Name: slot, Origin: a.Origin, Methods: methods, Timeout: time.Duration(timeout) * time.Millisecond, MaxRequestBytes: maxRequest, MaxResponseBytes: maxResponse, MaxCallsPerMinute: a.MaxCallsPerMinute}
	}
	for slot, a := range approval.ModelServices {
		resources.ModelServices[slot] = wasm.ModelServiceResource{Name: slot, Provider: a.Provider, Model: a.Model, Path: a.Path, Timeout: time.Duration(a.TimeoutMS) * time.Millisecond, MaxTokens: a.MaxTokens, MaxInputBytes: a.MaxInputBytes, MaxCallsPerMinute: a.MaxCallsPerMinute, MaxTokensPerHour: a.MaxTokensPerHour}
	}
	identity := bundle.Manifest.Name
	if len(resources.HTTP) != 0 || len(resources.ModelServices) != 0 {
		raw, marshalErr := json.Marshal(resources)
		if marshalErr != nil {
			store.Close()
			_ = os.RemoveAll(stateDir)
			return nil, plugin.PluginConfig{}, nil, marshalErr
		}
		digest := sha256.Sum256(raw)
		identity += "\x00resources\x00" + string(digest[:])
	}
	for key, value := range services.Cache.Private {
		if err := store.Set(ctx, wasm.PrivateCacheKey(identity, key), value, time.Hour); err != nil {
			store.Close()
			_ = os.RemoveAll(stateDir)
			return nil, plugin.PluginConfig{}, nil, err
		}
	}
	for key, value := range services.Cache.Shared {
		if err := store.Set(ctx, wasm.SharedCacheKey(key), value, time.Hour); err != nil {
			store.Close()
			_ = os.RemoveAll(stateDir)
			return nil, plugin.PluginConfig{}, nil, err
		}
	}

	rt := wasm.NewRuntimeWithCache(ctx, store)
	rt.StateGetFunc, rt.StateSetFunc, rt.StateKeysFunc = state.Get, state.Set, state.Keys
	rt.StateDeleteFunc, rt.StateGetVersionedFunc = state.Delete, state.GetVersioned
	rt.StateCompareAndSetFunc, rt.StateCompareAndDeleteFunc, rt.StateScanFunc = state.CompareAndSet, state.CompareAndDelete, state.Scan

	var mu sync.Mutex
	usedHTTP, usedModel := map[string]int{}, map[string]int{}
	var mismatches []error
	rt.HTTPRequestFunc = func(_ context.Context, _ string, resource wasm.HTTPResource, request *pbv1.OutboundHTTPRequestArgs) (*pbv1.OutboundHTTPResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		index := usedHTTP[resource.Name]
		if index >= len(httpFixtures[resource.Name]) {
			err := fmt.Errorf("unexpected HTTP call for slot %q", resource.Name)
			mismatches = append(mismatches, err)
			return nil, err
		}
		usedHTTP[resource.Name] = index + 1
		fixture := httpFixtures[resource.Name][index]
		if !proto.Equal(request, fixture.request) {
			err := fmt.Errorf("HTTP fixture %q[%d] request mismatch: got %s, want %s", resource.Name, index, compactJSON(request), compactJSON(fixture.request))
			mismatches = append(mismatches, err)
			return nil, err
		}
		if fixture.hostErr != nil {
			return nil, &scenarioFixtureError{host: fixture.hostErr}
		}
		return proto.Clone(fixture.response).(*pbv1.OutboundHTTPResponse), nil
	}
	rt.ModelCompleteFunc = func(_ context.Context, _ string, resource wasm.ModelServiceResource, request *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError) {
		mu.Lock()
		defer mu.Unlock()
		index := usedModel[resource.Name]
		if index >= len(modelFixtures[resource.Name]) {
			err := fmt.Errorf("unexpected model call for slot %q", resource.Name)
			mismatches = append(mismatches, err)
			return nil, &pbv1.HostError{Code: pbv1.ErrorCode_ERROR_CODE_INTERNAL, Message: err.Error()}
		}
		usedModel[resource.Name] = index + 1
		fixture := modelFixtures[resource.Name][index]
		if !proto.Equal(request, fixture.request) {
			err := fmt.Errorf("model fixture %q[%d] request mismatch: got %s, want %s", resource.Name, index, compactJSON(request), compactJSON(fixture.request))
			mismatches = append(mismatches, err)
			return nil, &pbv1.HostError{Code: pbv1.ErrorCode_ERROR_CODE_INTERNAL, Message: err.Error()}
		}
		if fixture.hostErr != nil {
			return nil, proto.Clone(fixture.hostErr).(*pbv1.HostError)
		}
		return proto.Clone(fixture.response).(*pbv1.ModelCompleteResult), nil
	}

	var finishOnce sync.Once
	var finishErr error
	finish := func() error {
		finishOnce.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			var errs []error
			errs = append(errs, mismatches...)
			for slot, fixtures := range httpFixtures {
				if remaining := len(fixtures) - usedHTTP[slot]; remaining != 0 {
					errs = append(errs, fmt.Errorf("HTTP slot %q has %d unused fixture(s)", slot, remaining))
				}
			}
			for slot, fixtures := range modelFixtures {
				if remaining := len(fixtures) - usedModel[slot]; remaining != 0 {
					errs = append(errs, fmt.Errorf("model slot %q has %d unused fixture(s)", slot, remaining))
				}
			}
			finishErr = errors.Join(errs...)
			_ = rt.Close()
			store.Close()
			_ = os.RemoveAll(stateDir)
		})
		return finishErr
	}
	return rt, config, finish, nil
}
