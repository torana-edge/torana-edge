package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestBoundModelServiceUsesOperatorDestinationAndReturnsNeutralResult(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"provider-reported","choices":[{"message":{"role":"assistant","content":"safe"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	providers := testProviderConfig(upstream.URL, "bound", "openai")
	providers.Providers["bound"] = provider.Provider{URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}
	server, err := New(Config{Port: "0", Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	requestedMax := uint32(80)
	result, hostErr := server.completeModel(context.Background(), "pii", wasm.ModelServiceResource{Name: "classifier", Provider: "bound", Model: "operator-model", Path: "/v1/chat/completions", Timeout: time.Second, MaxTokens: 40, MaxInputBytes: 1000, MaxCallsPerMinute: 2, MaxTokensPerHour: 100}, &pbv1.ModelCompleteArgs{Service: "classifier", Messages: []*pbv1.ModelMessage{{Role: "system", Content: "classify"}, {Role: "user", Content: "payload"}}, MaxTokens: &requestedMax})
	if hostErr != nil {
		t.Fatalf("host error = %+v", hostErr)
	}
	if result.Content != "safe" || result.ReportedModel != "provider-reported" || result.FinishReason != "stop" || result.Usage == nil || result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 2 {
		t.Fatalf("result = %+v", result)
	}
	if captured["model"] != "operator-model" || captured["max_tokens"] != float64(40) {
		t.Fatalf("request = %#v", captured)
	}
	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", captured["messages"])
	}
}

func TestBoundModelServiceProviderRefusalIsValueFree(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"SECRET-provider-body"}`))
	}))
	defer upstream.Close()
	providers := testProviderConfig(upstream.URL, "bound", "openai")
	providers.Providers["bound"] = provider.Provider{URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}
	server, err := New(Config{Port: "0", Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	_, hostErr := server.completeModel(context.Background(), "pii", wasm.ModelServiceResource{Name: "classifier", Provider: "bound", Model: "operator-model", Path: "/v1/chat/completions", Timeout: time.Second, MaxTokens: 40, MaxInputBytes: 1000, MaxCallsPerMinute: 2, MaxTokensPerHour: 100}, &pbv1.ModelCompleteArgs{Service: "classifier", Messages: []*pbv1.ModelMessage{{Role: "user", Content: "SECRET-request"}}})
	if hostErr == nil || hostErr.Code != pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE || hostErr.Message != "model service provider refused the request" {
		t.Fatalf("host error = %+v", hostErr)
	}
}

func TestPricingResourceSelectsTheRoutedRequestWithoutGuestCoordinates(t *testing.T) {
	server := &Server{}
	priced := 2.5
	colliding := 7.5
	resource := wasm.PricingResource{Name: "request", Prices: map[string]*pbv1.ModelPricing{
		wasm.PricingCoordinate("anthropic", "claude"):      {InputUsdPerMtok: &priced},
		wasm.PricingCoordinate("anthropic\x00claude", "x"): {InputUsdPerMtok: &colliding},
	}}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{Provider: "anthropic", Model: "claude"})
	got, hostErr := server.modelPricing(ctx, "compactor", resource)
	if hostErr != nil || got == nil || got.InputUsdPerMtok == nil || *got.InputUsdPerMtok != priced {
		t.Fatalf("pricing = %+v, host error = %+v", got, hostErr)
	}
	*got.InputUsdPerMtok = 99
	if *resource.Prices[wasm.PricingCoordinate("anthropic", "claude")].InputUsdPerMtok != priced {
		t.Fatal("returned pricing aliases the approval-bound resource")
	}
	for _, wrong := range []*reqState{{Provider: "anthropic", Model: "other"}, {Provider: "other", Model: "claude"}, nil} {
		candidate := context.Background()
		if wrong != nil {
			candidate = context.WithValue(candidate, reqStateKey{}, wrong)
		}
		if value, refusal := server.modelPricing(candidate, "compactor", resource); value != nil || refusal == nil || refusal.Code != pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
			t.Fatalf("wrong route returned (%+v, %+v)", value, refusal)
		}
	}
}

func TestPricingResourceCoordinatesCannotCollide(t *testing.T) {
	server := &Server{}
	left, right := 1.0, 2.0
	resource := wasm.PricingResource{Name: "request", Prices: map[string]*pbv1.ModelPricing{
		wasm.PricingCoordinate("a\x00b", "c"): {InputUsdPerMtok: &left},
		wasm.PricingCoordinate("a", "b\x00c"): {InputUsdPerMtok: &right},
	}}
	for _, row := range []struct {
		provider string
		model    string
		want     float64
	}{{"a\x00b", "c", left}, {"a", "b\x00c", right}} {
		ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{Provider: row.provider, Model: row.model})
		got, refusal := server.modelPricing(ctx, "compactor", resource)
		if refusal != nil || got == nil || got.InputUsdPerMtok == nil || *got.InputUsdPerMtok != row.want {
			t.Fatalf("%q/%q = %+v, refusal %+v", row.provider, row.model, got, refusal)
		}
	}
}

func TestPricingResourceUsesThePendingRoute(t *testing.T) {
	server := &Server{}
	original, routed := 1.0, 3.0
	resource := wasm.PricingResource{Name: "request", Prices: map[string]*pbv1.ModelPricing{
		wasm.PricingCoordinate("original", "original-model"): {InputUsdPerMtok: &original},
		wasm.PricingCoordinate("routed", "routed-model"):     {InputUsdPerMtok: &routed},
	}}
	state := &reqState{
		Provider: "original", Model: "original-model", InitialProvider: "original",
		PendingRoute: &wasm.RouteVerdict{Plugin: "router", Provider: "routed", Model: "routed-model"},
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, state)
	got, refusal := server.modelPricing(ctx, "compactor", resource)
	if refusal != nil || got == nil || got.InputUsdPerMtok == nil || *got.InputUsdPerMtok != routed {
		t.Fatalf("pending-route pricing = %+v, refusal %+v", got, refusal)
	}
}

func TestBoundModelServiceRejectsOversizedInputBeforeSpend(t *testing.T) {
	server := &Server{}
	result, refusal := server.completeModel(context.Background(), "pii", wasm.ModelServiceResource{MaxInputBytes: 3}, &pbv1.ModelCompleteArgs{Messages: []*pbv1.ModelMessage{{Role: "user", Content: "secret"}}})
	if result != nil || refusal == nil || refusal.Code != pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || refusal.Message != "model request exceeds the approved input limit" {
		t.Fatalf("result/refusal = %+v / %+v", result, refusal)
	}
}
