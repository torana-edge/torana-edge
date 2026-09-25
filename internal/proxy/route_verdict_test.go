package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestRouteOutcomeReachesResponseHookMetadata(t *testing.T) {
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"original": {URL: "https://original.example", Format: "openai"},
		"target":   {URL: "https://target.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
	}}
	for _, tc := range []struct {
		name, provider, refusal, finalProvider, finalModel string
	}{
		{"accepted", "target", "", "target", "target-model"},
		{"refused", "missing", "unknown_provider", "original", "original-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &reqState{Provider: "original", Model: "original-model"}
			rc := &RouteContext{ProviderName: "original", StrippedPath: "/v1/chat/completions"}
			ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
			ctx = context.WithValue(ctx, routeContextKey{}, rc)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1/chat/completions", nil)
			chat := &engine.ChatRequest{Model: "original-model"}
			(&Server{}).applyRoute(req, chat, "openai", "original",
				&wasm.RouteVerdict{Provider: tc.provider, Model: "target-model", Plugin: "router"}, cfg)
			rs.Model = chat.Model
			var meta struct {
				Route struct {
					Provider string  `json:"provider"`
					Model    string  `json:"model"`
					Plugin   string  `json:"verdict_plugin"`
					Refused  *string `json:"refused"`
				} `json:"_route_applied"`
			}
			if err := json.Unmarshal(rs.chatResponse(chat.Model, "", nil, "").ToranaMetaJSON, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Route.Provider != tc.finalProvider || meta.Route.Model != tc.finalModel || meta.Route.Plugin != "router" {
				t.Fatalf("route outcome = %+v", meta.Route)
			}
			if tc.refusal == "" && meta.Route.Refused != nil || tc.refusal != "" && (meta.Route.Refused == nil || *meta.Route.Refused != tc.refusal) {
				t.Fatalf("refusal = %v, want %q", meta.Route.Refused, tc.refusal)
			}
		})
	}
}

func TestRouteOutcomeSeparatesPluginRouteFromFailover(t *testing.T) {
	rs := &reqState{Provider: "original", Model: "original-model"}
	rc := &RouteContext{ProviderName: "original", StrippedPath: "/v1/chat/completions"}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	ctx = context.WithValue(ctx, routeContextKey{}, rc)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1/chat/completions", nil)
	chat := &engine.ChatRequest{Model: "original-model"}
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"target": {URL: "https://target.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
	}}
	if !(&Server{}).applyRoute(req, chat, "openai", "original", &wasm.RouteVerdict{Provider: "target", Model: "target-model", Plugin: "router"}, cfg) {
		t.Fatal("route refused")
	}
	// A fallback changes the upstream attempt, not the plugin's verdict.
	rs.Provider = "fallback"
	rs.Model = chat.Model
	var meta struct {
		Route struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			ServedBy string `json:"served_by"`
			Failover bool   `json:"failover"`
		} `json:"_route_applied"`
	}
	if err := json.Unmarshal(rs.chatResponse(chat.Model, "", nil, "").ToranaMetaJSON, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Route.Provider != "target" || meta.Route.Model != "target-model" || meta.Route.ServedBy != "fallback" || !meta.Route.Failover {
		t.Fatalf("route outcome = %+v", meta.Route)
	}
}

func TestRouteRefusalCodes(t *testing.T) {
	base := provider.Config{Providers: map[string]provider.Provider{
		"different_format": {URL: "https://other.example", Format: "anthropic"},
		"invalid_url":      {URL: "://bad", Format: "openai"},
		"credential":       {URL: "https://cred.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "credential", Credential: "missing"}},
		"valid":            {URL: "https://valid.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
	}}
	for _, tc := range []struct {
		name, provider, want string
		noRouteContext       bool
	}{
		{"unknown", "unknown", "unknown_provider", false},
		{"format", "different_format", "format_mismatch", false},
		{"url", "invalid_url", "invalid_target_url", false},
		{"credential", "credential", "credential_policy", false},
		{"route_context", "valid", "missing_route_context", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &reqState{Provider: "original", Model: "original-model"}
			ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
			if !tc.noRouteContext {
				ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "original", StrippedPath: "/v1/chat/completions"})
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1/chat/completions", nil)
			chat := &engine.ChatRequest{Model: "original-model"}
			if (&Server{}).applyRoute(req, chat, "openai", "original", &wasm.RouteVerdict{Provider: tc.provider, Model: "new-model", Plugin: "router"}, base) {
				t.Fatal("route unexpectedly applied")
			}
			if rs.RouteRefused != tc.want || rs.RouteProvider != "original" || rs.RouteModel != "original-model" {
				t.Fatalf("route state = %+v", rs)
			}
		})
	}
}

func TestNoopRouteIsNotReportedAsRefused(t *testing.T) {
	rs := &reqState{Provider: "original", Model: "original-model"}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1/chat/completions", nil)
	chat := &engine.ChatRequest{Model: "original-model"}
	if (&Server{}).applyRoute(req, chat, "openai", "original", &wasm.RouteVerdict{Provider: "original", Plugin: "router"}, provider.Config{}) {
		t.Fatal("no-op route reported as applied")
	}
	if rs.RouteAttempted || rs.RouteRefused != "" || len(rs.chatResponse(chat.Model, "", nil, "").ToranaMetaJSON) != 0 {
		t.Fatalf("no-op route state = %+v", rs)
	}
}

// A routing verdict is ONE decision: "send this to provider X as model Y".
// When the provider half is rejected, the model half must not be applied on
// its own — the request would go to the ORIGINAL provider carrying a model
// chosen for a different one, which that provider does not serve. The caller
// sees a model error from an upstream they did not choose, for a route that
// was refused.
//
// applyRoute fails OPEN by design, and failing open has to mean the original
// route rather than half of a route nobody asked for.
func TestRejectedRouteLeavesTheModelAlone(t *testing.T) {
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"original":     {URL: "https://original.example", Format: "openai"},
		"otherformat":  {URL: "https://other.example", Format: "anthropic"},
		"unparseable":  {URL: "://not-a-url", Format: "openai"},
		"nocredential": {URL: "https://nc.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "credential", Credential: "absent"}},
	}}

	for _, tc := range []struct {
		name   string
		target string
		why    string
	}{
		{"unknown provider", "nosuch", "the provider is not configured"},
		{"format mismatch", "otherformat", "cross-format routing is unsupported"},
		{"unparseable url", "unparseable", "the provider URL does not parse"},
		{"credential unavailable", "nocredential", "the configured credential cannot be resolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &RouteContext{ProviderName: "original", StrippedPath: "/v1/chat/completions"}
			req, _ := http.NewRequestWithContext(
				context.WithValue(context.Background(), routeContextKey{}, rc),
				http.MethodPost, "https://original.example/v1/chat/completions", nil)

			chat := &engine.ChatRequest{Model: "original-model"}
			applied := (&Server{}).applyRoute(req, chat, "openai", "original",
				&wasm.RouteVerdict{Provider: tc.target, Model: "model-only-on-the-target", Plugin: "p"}, cfg)
			if applied {
				t.Errorf("rejected route reported itself as applied")
			}

			if chat.Model != "original-model" {
				t.Errorf("model = %q, want it untouched at %q — the route was rejected because %s, "+
					"so the original provider is now being asked for a model chosen for %q",
					chat.Model, "original-model", tc.why, tc.target)
			}
			if rc.ProviderName != "original" {
				t.Errorf("provider = %q, want the original", rc.ProviderName)
			}
			if req.URL.Host != "original.example" {
				t.Errorf("upstream host = %q, want it untouched", req.URL.Host)
			}
		})
	}
}

// The other half of the contract: a verdict that passes every check applies
// both parts, and a model-only verdict needs no provider to validate.
func TestAcceptedRouteAppliesProviderAndModel(t *testing.T) {
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"target": {URL: "https://target.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "caller"}},
	}}
	rc := &RouteContext{ProviderName: "original", StrippedPath: "/v1/chat/completions"}
	req, _ := http.NewRequestWithContext(
		context.WithValue(context.Background(), routeContextKey{}, rc),
		http.MethodPost, "https://original.example/v1/chat/completions", nil)

	chat := &engine.ChatRequest{Model: "original-model"}
	applied := (&Server{}).applyRoute(req, chat, "openai", "original",
		&wasm.RouteVerdict{Provider: "target", Model: "target-model", Plugin: "p"}, cfg)
	if !applied {
		t.Error("accepted route reported itself as rejected")
	}

	if chat.Model != "target-model" {
		t.Errorf("model = %q, want the verdict's", chat.Model)
	}
	if rc.ProviderName != "target" || req.URL.Host != "target.example" {
		t.Errorf("route not applied: provider=%q host=%q", rc.ProviderName, req.URL.Host)
	}
}

func TestModelOnlyVerdictStillApplies(t *testing.T) {
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"original": {URL: "https://original.example", Format: "openai"},
	}}
	for _, target := range []string{"", "original"} {
		rc := &RouteContext{ProviderName: "original", StrippedPath: "/v1/chat/completions"}
		req, _ := http.NewRequestWithContext(
			context.WithValue(context.Background(), routeContextKey{}, rc),
			http.MethodPost, "https://original.example/v1/chat/completions", nil)
		chat := &engine.ChatRequest{Model: "original-model"}
		applied := (&Server{}).applyRoute(req, chat, "openai", "original",
			&wasm.RouteVerdict{Provider: target, Model: "cheaper-model", Plugin: "p"}, cfg)
		if !applied {
			t.Errorf("provider %q: model-only route reported itself as rejected", target)
		}
		if chat.Model != "cheaper-model" {
			t.Errorf("provider %q: model = %q, want the override to apply with no provider change",
				target, chat.Model)
		}
	}
}

func TestEffortOnlyVerdictKeepsProviderAndModel(t *testing.T) {
	rs := &reqState{Provider: "original", Model: "original-model"}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1/chat/completions", nil)
	chat := &engine.ChatRequest{Model: "original-model"}
	if !(&Server{}).applyRoute(req, chat, "openai", "original", &wasm.RouteVerdict{
		Plugin: "router", Effort: pb.Effort_EFFORT_HIGH,
	}, provider.Config{}) {
		t.Fatal("effort-only verdict was discarded")
	}
	if chat.Model != "original-model" || rs.RouteProvider != "original" || rs.RouteModel != "original-model" {
		t.Fatalf("effort-only verdict changed route: model %q, state %+v", chat.Model, rs)
	}
}

func TestResponsesProviderSideHistoryRefusesCrossProviderRoute(t *testing.T) {
	rs := &reqState{Provider: "original", Model: "original-model"}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "original", StrippedPath: "/v1/responses"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1/responses", nil)
	extensions, err := engine.ParseOptionalJSONObject([]byte(`{"previous_response_id":"resp_123"}`))
	if err != nil {
		t.Fatal(err)
	}
	chat := &engine.ChatRequest{Model: "original-model", OpenAIVariant: engine.OpenAIResponses, ProviderExtensions: extensions}
	cfg := provider.Config{Providers: map[string]provider.Provider{"target": {URL: "https://target.example", Format: "openai"}}}
	if (&Server{}).applyRoute(req, chat, "openai", "original", &wasm.RouteVerdict{Provider: "target", Model: "target-model", Plugin: "router"}, cfg) {
		t.Fatal("provider-side history moved to another provider")
	}
	if rs.RouteRefused != "server_state" || chat.Model != "original-model" {
		t.Fatalf("route refusal = %q, model %q", rs.RouteRefused, chat.Model)
	}
}

func TestGeminiRouteRewritesModelPath(t *testing.T) {
	rs := &reqState{Provider: "original", Model: "old"}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "original", StrippedPath: "/v1beta/models/old:generateContent"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1beta/models/old:generateContent", nil)
	chat := &engine.ChatRequest{Model: "old"}
	cfg := provider.Config{Providers: map[string]provider.Provider{"target": {URL: "https://target.example", Format: "gemini", Auth: provider.ProviderAuth{Mode: "none"}}}}
	if !(&Server{}).applyRoute(req, chat, "gemini", "original", &wasm.RouteVerdict{Provider: "target", Model: "new", Plugin: "router"}, cfg) {
		t.Fatal("Gemini route refused")
	}
	if req.URL.Path != "/v1beta/models/new:generateContent" || chat.Model != "new" {
		t.Fatalf("Gemini route path %q, model %q", req.URL.Path, chat.Model)
	}
}

func TestRouteHostCallValidatesKnownTargets(t *testing.T) {
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"original": {URL: "https://original.example", Format: "openai"},
		"same":     {URL: "https://same.example", Format: "openai"},
		"other":    {URL: "https://other.example", Format: "anthropic"},
	}}
	s := &Server{config: Config{Providers: cfg}}
	chat := &engine.ChatRequest{Model: "m"}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{Provider: "original", InitialFormat: "openai"})
	ctx = context.WithValue(ctx, syntheticResponseScopeKey{}, syntheticResponseScope{format: format.Lookup("openai"), request: chat})
	for _, tc := range []struct {
		provider string
		code     pb.ErrorCode
	}{
		{"missing", pb.ErrorCode_ERROR_CODE_NOT_FOUND},
		{"other", pb.ErrorCode_ERROR_CODE_UNSUPPORTED},
		{"same", pb.ErrorCode_ERROR_CODE_UNSPECIFIED},
	} {
		got := s.validateRouteHost(ctx, &pb.RouteRequestArgs{Provider: tc.provider, Model: "m"})
		if tc.code == pb.ErrorCode_ERROR_CODE_UNSPECIFIED && got != nil || tc.code != pb.ErrorCode_ERROR_CODE_UNSPECIFIED && (got == nil || got.Code != tc.code) {
			t.Fatalf("provider %q: validation = %+v, want %v", tc.provider, got, tc.code)
		}
	}
	extensions, _ := engine.ParseOptionalJSONObject([]byte(`{"previous_response_id":"resp_123"}`))
	chat.OpenAIVariant, chat.ProviderExtensions = engine.OpenAIResponses, extensions
	if got := s.validateRouteHost(ctx, &pb.RouteRequestArgs{Provider: "same"}); got == nil || got.Code != pb.ErrorCode_ERROR_CODE_UNSUPPORTED {
		t.Fatalf("provider-side history validation = %+v", got)
	}
}

func TestBridgeRouteRequiresMatchingClientContract(t *testing.T) {
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"target": {
			URL: "https://target.example", Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"},
			Bridge: &provider.BridgeConfig{Client: bridge.Anthropic, Upstream: bridge.Anthropic},
		},
	}}
	rc := &RouteContext{ProviderName: "original", StrippedPath: "/v1/responses"}
	exchange := &bridgeExchange{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIChat}
	rs := &reqState{Provider: "original", Model: "original-model"}
	ctx := context.WithValue(context.Background(), routeContextKey{}, rc)
	ctx = context.WithValue(ctx, bridgeContextKey{}, exchange)
	ctx = context.WithValue(ctx, reqStateKey{}, rs)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://original.example/v1/responses", nil)
	chat := &engine.ChatRequest{Model: "original-model", OpenAIVariant: engine.OpenAIResponses}

	applied := (&Server{}).applyRoute(req, chat, "openai", "original",
		&wasm.RouteVerdict{Provider: "target", Model: "target-model", Plugin: "p"}, cfg)
	if applied {
		t.Fatal("route using another exposed client contract was applied")
	}
	if rc.ProviderName != "original" || req.URL.Host != "original.example" || chat.Model != "original-model" {
		t.Fatalf("rejected route changed provider=%q host=%q model=%q", rc.ProviderName, req.URL.Host, chat.Model)
	}
	if rs.RouteRefused != "bridge_unrepresentable" {
		t.Fatalf("bridge refusal = %q", rs.RouteRefused)
	}
}
