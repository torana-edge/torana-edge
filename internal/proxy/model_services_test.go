package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	result, hostErr := server.completeModel(context.Background(), "pii", wasm.ModelServiceResource{Name: "classifier", Provider: "bound", Model: "operator-model", Path: "/v1/chat/completions", Timeout: time.Second, MaxTokens: 40, MaxInputBytes: 1000, MaxCallsPerMinute: 2, MaxTokensPerHour: 100}, &pbv1.ModelCompleteArgs{Service: "classifier", Messages: []*pbv1.Message{{Role: "system", Blocks: modelTextBlocks("classify")}, {Role: "user", Blocks: modelTextBlocks("payload")}}, MaxTokens: &requestedMax})
	if hostErr != nil {
		t.Fatalf("host error = %+v", hostErr)
	}
	if result.GetMessage().GetBlocks()[0].GetText().GetText() != "safe" || result.ReportedModel != "provider-reported" || result.FinishReason != "stop" || result.Usage == nil || result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 2 {
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
	_, hostErr := server.completeModel(context.Background(), "pii", wasm.ModelServiceResource{Name: "classifier", Provider: "bound", Model: "operator-model", Path: "/v1/chat/completions", Timeout: time.Second, MaxTokens: 40, MaxInputBytes: 1000, MaxCallsPerMinute: 2, MaxTokensPerHour: 100}, &pbv1.ModelCompleteArgs{Service: "classifier", Messages: []*pbv1.Message{{Role: "user", Blocks: modelTextBlocks("SECRET-request")}}})
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

func TestPromptCachePolicySelectsRouteOrUnambiguousBackgroundBinding(t *testing.T) {
	server := &Server{}
	firstRate, secondRate := 0.1, 0.2
	first := &pbv1.PromptCachePolicy{CacheReadUsdPerMtok: &firstRate}
	second := &pbv1.PromptCachePolicy{CacheReadUsdPerMtok: &secondRate}
	resource := wasm.PromptCacheResource{Name: "request-cache", Policies: map[string]*pbv1.PromptCachePolicy{
		wasm.PricingCoordinate("anthropic", "claude"): first,
		wasm.PricingCoordinate("openai", "gpt"):       second,
	}}

	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{Provider: "anthropic", Model: "claude"})
	got, refusal := server.promptCachePolicy(ctx, "cache_tier_selector", resource)
	if refusal != nil || got == nil || got.CacheReadUsdPerMtok == nil || *got.CacheReadUsdPerMtok != firstRate {
		t.Fatalf("routed policy = %+v, refusal %+v", got, refusal)
	}
	*got.CacheReadUsdPerMtok = 9
	if *first.CacheReadUsdPerMtok != firstRate {
		t.Fatal("returned policy aliases the approval-bound resource")
	}

	if value, hostErr := server.promptCachePolicy(context.Background(), "cache_warmer", resource); value != nil || hostErr == nil || hostErr.Code != pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
		t.Fatalf("ambiguous background policy = (%+v, %+v)", value, hostErr)
	}
	single := wasm.PromptCacheResource{Name: "warm-cache", Policies: map[string]*pbv1.PromptCachePolicy{wasm.PricingCoordinate("anthropic", "claude"): first}}
	if value, hostErr := server.promptCachePolicy(context.Background(), "cache_warmer", single); hostErr != nil || value == nil || value.CacheReadUsdPerMtok == nil || *value.CacheReadUsdPerMtok != firstRate {
		t.Fatalf("unambiguous background policy = (%+v, %+v)", value, hostErr)
	}
}

func TestPromptCachePolicyUsesPendingRoute(t *testing.T) {
	server := &Server{}
	originalRate, routedRate := 0.1, 0.3
	resource := wasm.PromptCacheResource{Name: "request-cache", Policies: map[string]*pbv1.PromptCachePolicy{
		wasm.PricingCoordinate("original", "original-model"): {CacheReadUsdPerMtok: &originalRate},
		wasm.PricingCoordinate("routed", "routed-model"):     {CacheReadUsdPerMtok: &routedRate},
	}}
	state := &reqState{
		Provider: "original", Model: "original-model", InitialProvider: "original",
		PendingRoute: &wasm.RouteVerdict{Plugin: "router", Provider: "routed", Model: "routed-model"},
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, state)
	got, refusal := server.promptCachePolicy(ctx, "cache_tier_selector", resource)
	if refusal != nil || got == nil || got.CacheReadUsdPerMtok == nil || *got.CacheReadUsdPerMtok != routedRate {
		t.Fatalf("pending-route cache policy = %+v, refusal %+v", got, refusal)
	}
}

func TestBoundModelServiceRejectsOversizedInputBeforeSpend(t *testing.T) {
	server := &Server{}
	result, refusal := server.completeModel(context.Background(), "pii", wasm.ModelServiceResource{MaxInputBytes: 3}, &pbv1.ModelCompleteArgs{Messages: []*pbv1.Message{{Role: "user", Blocks: modelTextBlocks("secret")}}})
	if result != nil || refusal == nil || refusal.Code != pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || refusal.Message != "model request exceeds the approved input limit" {
		t.Fatalf("result/refusal = %+v / %+v", result, refusal)
	}
}

// Guests must not double-charge cache reads or infer that cache writes overlap
// input totals. The feed must keep the original usage, even for empty content.
func TestModelServiceUsageNormalizesCacheReads(t *testing.T) {
	for _, tc := range []struct {
		name, format, body string
		wantInput          int32
		wantCacheWrite     int32
		providerInput      int64
	}{
		{"openai", "openai", `{"choices":[{"message":{"content":""}}],"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":80}}}`, 20, 0, 100},
		{"deepseek", "openai", `{"choices":[{"message":{"content":""}}],"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_cache_hit_tokens":80}}`, 20, 0, 100},
		{"responses_with_cache_writes", "openai", `{"output":[{"type":"message","content":[{"type":"output_text","text":""}]}],"usage":{"input_tokens":100,"output_tokens":2,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":150}}}`, 20, 150, 100},
		{"gemini", "gemini", `{"candidates":[{"content":{"parts":[{"text":""}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":2,"cachedContentTokenCount":80}}`, 20, 0, 100},
		{"gemini-codeassist", "gemini-codeassist", `{"response":{"candidates":[{"content":{"parts":[{"text":""}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":2,"cachedContentTokenCount":80}}}`, 20, 0, 100},
		{"anthropic", "anthropic", `{"content":[{"type":"text","text":""}],"usage":{"input_tokens":20,"output_tokens":2,"cache_read_input_tokens":80,"cache_creation_input_tokens":15}}`, 20, 15, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			providers := testProviderConfig(upstream.URL, "bound", tc.format)
			providers.Providers["bound"] = provider.Provider{URL: upstream.URL, Format: tc.format, Auth: provider.ProviderAuth{Mode: "none"}}
			s, err := New(Config{Port: "0", Providers: providers})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Shutdown(context.Background())
			result, herr := s.completeModel(context.Background(), "compactor", wasm.ModelServiceResource{Name: "summarizer", Provider: "bound", Model: "m", Path: inferenceTestPath(tc.format), Timeout: time.Second, MaxTokens: 40, MaxInputBytes: 1000, MaxCallsPerMinute: 10, MaxTokensPerHour: 1000}, &pbv1.ModelCompleteArgs{Service: "summarizer", Messages: []*pbv1.Message{{Role: "user", Blocks: modelTextBlocks("summarize")}}})
			if herr != nil || result == nil || result.Usage == nil {
				t.Fatalf("result=%v refusal=%v", result, herr)
			}
			if result.Usage.InputTokens != tc.wantInput || result.Usage.CacheReadTokens != 80 || result.Usage.CacheWriteTokens != tc.wantCacheWrite || result.Usage.OutputTokens != 2 {
				t.Fatalf("usage=%v", result.Usage)
			}
			// Empty/unusable content must still be visible as a paid model-service call.
			events := s.feed.Snapshot()
			if len(events) != 1 || events[0].Verdict != "plugin-egress" || events[0].TokensIn != tc.providerInput || events[0].TokensOut != 2 || events[0].CacheReadTokens != 80 || events[0].CacheWriteTokens != int64(tc.wantCacheWrite) {
				t.Fatalf("paid empty response missing from feed: %+v", events)
			}
		})
	}
}

// A successful completion with unreliable metering is usable by callers that
// do not need cost data. Cost-sensitive callers can decline on absent Usage
// instead of turning a provider reporting defect into a plugin hook error.
func TestModelServiceInvalidUsagePreservesCompletion(t *testing.T) {
	for _, tc := range []struct {
		name                string
		input, output, read int64
	}{
		{"inconsistent_cache_read", 10, 2, 80},
		{"negative_input", -1, 2, 0},
		{"input_exceeds_int32", 2147483648, 2, 0},
		{"output_exceeds_int32", 100, 2147483648, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				input, output, read := tc.input, tc.output, tc.read
				if calls.Add(1) > 1 {
					input, output, read = 1, 1, 0
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"model":   "reported-model",
					"choices": []any{map[string]any{"message": map[string]any{"content": "useful summary"}, "finish_reason": "stop"}},
					"usage":   map[string]any{"prompt_tokens": input, "completion_tokens": output, "prompt_tokens_details": map[string]any{"cached_tokens": read}},
				}); err != nil {
					t.Error(err)
				}
			}))
			defer upstream.Close()
			providers := testProviderConfig(upstream.URL, "bound", "openai")
			providers.Providers["bound"] = provider.Provider{URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}
			s, err := New(Config{Port: "0", Providers: providers})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Shutdown(context.Background())
			resource := wasm.ModelServiceResource{Name: "summarizer", Provider: "bound", Model: "m", Path: "/v1/chat/completions", Timeout: time.Second, MaxTokens: 40, MaxInputBytes: 1000, MaxCallsPerMinute: 10, MaxTokensPerHour: 1}
			args := &pbv1.ModelCompleteArgs{Service: "summarizer", Messages: []*pbv1.Message{{Role: "user", Blocks: modelTextBlocks("summarize")}}}
			result, herr := s.completeModel(context.Background(), "compactor", resource, args)
			if herr != nil || result == nil {
				t.Fatalf("successful completion lost: result=%v refusal=%v", result, herr)
			}
			if result.GetMessage().GetBlocks()[0].GetText().GetText() != "useful summary" || result.ReportedModel != "reported-model" || result.FinishReason != "stop" || result.Usage != nil {
				t.Fatalf("want preserved completion with unknown usage, got %v", result)
			}
			events := s.feed.Snapshot()
			if len(events) != 1 || events[0].Verdict != "plugin-egress" || events[0].TokensIn != tc.input || events[0].TokensOut != tc.output || events[0].CacheReadTokens != tc.read {
				t.Fatalf("original provider metering missing from feed: %+v", events)
			}
			// Invalid metering must not exhaust the hourly budget. A later valid
			// report must still charge it and stop further provider requests.
			result, herr = s.completeModel(context.Background(), "compactor", resource, args)
			if herr != nil || result == nil || result.Usage == nil || result.Usage.InputTokens != 1 || result.Usage.OutputTokens != 1 {
				t.Fatalf("invalid report poisoned later completion: result=%v refusal=%v", result, herr)
			}
			result, herr = s.completeModel(context.Background(), "compactor", resource, args)
			if result != nil || herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE || !strings.Contains(herr.Message, "tokens/hour") || calls.Load() != 2 {
				t.Fatalf("valid report did not bind the token budget: result=%v refusal=%v calls=%d", result, herr, calls.Load())
			}
		})
	}
}

func modelTextBlocks(text string) []*pbv1.RequestBlock {
	return []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: text}}}}
}
