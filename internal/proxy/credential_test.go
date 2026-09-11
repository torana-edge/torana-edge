package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

func TestCallerCredentialRateIdentityCoversSupportedSchemes(t *testing.T) {
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key"} {
		h := make(http.Header)
		h.Add(name, "same-secret")
		got := (callerCredentials{headers: h}).rateIdentity()
		want := http.CanonicalHeaderKey(name) + "\x00same-secret"
		if got != want {
			t.Errorf("%s identity = %q, want %q", name, got, want)
		}
	}
	if a, b := (callerCredentials{headers: http.Header{"Authorization": {"same"}}}).rateIdentity(),
		(callerCredentials{headers: http.Header{"X-Api-Key": {"same"}}}).rateIdentity(); a == b {
		t.Fatal("equal secrets in different credential schemes collapsed into one rate bucket")
	}
}

func TestApplyProviderCredentialUsesProtocolNativeHeader(t *testing.T) {
	for _, test := range []struct {
		format string
		header string
		value  string
	}{
		{format: "openai", header: "Authorization", value: "Bearer managed-secret"},
		{format: "anthropic", header: "X-Api-Key", value: "managed-secret"},
		{format: "gemini", header: "X-Goog-Api-Key", value: "managed-secret"},
		{format: "gemini-codeassist", header: "Authorization", value: "Bearer managed-secret"},
	} {
		t.Run(test.format, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://provider.example/infer", nil)
			req.Header.Set("Authorization", "Bearer caller")
			req.Header.Set("X-Api-Key", "caller")
			req.Header.Set("X-Goog-Api-Key", "caller")
			err := applyProviderCredential(context.Background(), req, provider.Provider{
				Format: test.format,
				Auth:   provider.ProviderAuth{Mode: "credential", Credential: "managed"},
			}, callerCredentialsFrom(req), func(context.Context, string) ([]byte, error) {
				return []byte("managed-secret"), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
				want := ""
				if name == test.header {
					want = test.value
				}
				if got := req.Header.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestApplyProviderCredentialCallerUsesIngressSnapshot(t *testing.T) {
	// Every credential family caller mode is expected to forward. The strip
	// list grew to close a leak in credential mode while the snapshot stayed
	// at three fields, so an Azure api-key, an AWS session token and the
	// Google client identity were removed on the way through and never put
	// back — caller-mode authentication simply stopped working for them.
	ingress, _ := http.NewRequest(http.MethodPost, "https://provider.example/infer", nil)
	want := map[string]string{
		"Authorization":        "Bearer original-caller-secret",
		"X-Api-Key":            "original-api-key",
		"X-Goog-Api-Key":       "original-google-key",
		"Api-Key":              "original-azure-key",
		"X-Amz-Security-Token": "original-aws-session-token",
		"X-Goog-Api-Client":    "gl-go/1.26 gapic/1.0",
	}
	for name, value := range want {
		ingress.Header.Set(name, value)
	}
	// Ambient credentials that are not the caller authenticating to a model
	// provider. These are dropped in every mode, caller included.
	ingress.Header.Set("Cookie", "session=secret")
	ingress.Header.Set("Proxy-Authorization", "Basic proxy-secret")
	caller := callerCredentialsFrom(ingress)

	req, _ := http.NewRequest(http.MethodPost, "https://fallback.example/infer", nil)
	// A managed credential already installed for the primary. It must not
	// become the caller credential merely because failover clones req.
	req.Header.Set("Authorization", "Bearer primary-managed-secret")

	if err := applyProviderCredential(context.Background(), req, provider.Provider{
		Auth: provider.ProviderAuth{Mode: "caller"},
	}, caller, nil); err != nil {
		t.Fatal(err)
	}
	for name, value := range want {
		if got := req.Header.Get(name); got != value {
			t.Errorf("%s = %q, want the immutable ingress value %q", name, got, value)
		}
	}
	for _, name := range []string{"Cookie", "Proxy-Authorization"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("%s = %q; ambient credentials must not reach an upstream in any mode", name, got)
		}
	}
}

// The complement: a provider-managed credential replaces the caller's, and
// nothing the caller sent survives the boundary.
func TestApplyProviderCredentialCredentialModeDropsEveryCallerHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://provider.example/infer", nil)
	for _, name := range append(append([]string{}, callerForwardedHeaders...), neverForwardedHeaders...) {
		req.Header.Set(name, "caller-secret-"+name)
	}
	err := applyProviderCredential(context.Background(), req, provider.Provider{
		Auth: provider.ProviderAuth{Mode: "credential", Credential: "managed"},
	}, callerCredentialsFrom(req), func(context.Context, string) ([]byte, error) {
		return []byte("managed-secret"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer managed-secret" {
		t.Fatalf("Authorization = %q, want the managed credential", got)
	}
	for _, name := range callerForwardedHeaders {
		if name == "Authorization" {
			continue
		}
		if got := req.Header.Get(name); got != "" {
			t.Errorf("%s = %q survived into credential mode; the caller's secrets "+
				"must not leave the machine", name, got)
		}
	}
	for _, name := range neverForwardedHeaders {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("%s = %q survived into credential mode", name, got)
		}
	}
}

func TestApplyUpstreamCredentialNeverFallsBackToLiveHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://provider.example/infer", nil)
	req.Header.Set("Authorization", "Bearer mutable-live-secret")
	req.Header.Set("X-Api-Key", "mutable-api-key")
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"target": {Format: "openai", Auth: provider.ProviderAuth{Mode: "caller"}},
	}}
	if err := (&Server{}).applyUpstreamCredential(req, cfg, &RouteContext{ProviderName: "target"}); err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		if got := req.Header.Get(header); got != "" {
			t.Fatalf("%s leaked from mutable request headers: %q", header, got)
		}
	}
}

func TestApplyRouteNeverFallsBackToLiveHeaders(t *testing.T) {
	rc := &RouteContext{ProviderName: "original", StrippedPath: "/v1/chat/completions"}
	req, _ := http.NewRequestWithContext(
		context.WithValue(context.Background(), routeContextKey{}, rc),
		http.MethodPost,
		"https://original.example/v1/chat/completions",
		nil,
	)
	req.Header.Set("Authorization", "Bearer mutable-live-secret")
	req.Header.Set("X-Api-Key", "mutable-api-key")
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"target": {URL: "https://target.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "caller"}},
	}}
	(&Server{}).applyRoute(req, &engine.ChatRequest{}, "openai", "original", &wasm.RouteVerdict{Provider: "target", Plugin: "test"}, cfg)
	if rc.ProviderName != "target" {
		t.Fatalf("route was not applied: %+v", rc)
	}
	for _, header := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		if got := req.Header.Get(header); got != "" {
			t.Fatalf("%s leaked from mutable request headers after routing: %q", header, got)
		}
	}
}
