package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

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
		{format: "bedrock", header: "Authorization", value: "Bearer managed-secret"},
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
	req, _ := http.NewRequest(http.MethodPost, "https://fallback.example/infer", nil)
	// This is a managed credential already installed for the primary. It must
	// not become the caller credential merely because failover clones req.
	req.Header.Set("Authorization", "Bearer primary-managed-secret")
	caller := callerCredentials{
		Authorization: "Bearer original-caller-secret",
		APIKey:        "original-api-key",
		GoogleAPIKey:  "original-google-key",
	}
	if err := applyProviderCredential(context.Background(), req, provider.Provider{
		Auth: provider.ProviderAuth{Mode: "caller"},
	}, caller, nil); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != caller.Authorization {
		t.Fatalf("Authorization = %q, want immutable ingress value %q", got, caller.Authorization)
	}
	if got := req.Header.Get("X-Api-Key"); got != caller.APIKey {
		t.Fatalf("X-Api-Key = %q, want %q", got, caller.APIKey)
	}
	if got := req.Header.Get("X-Goog-Api-Key"); got != caller.GoogleAPIKey {
		t.Fatalf("X-Goog-Api-Key = %q, want %q", got, caller.GoogleAPIKey)
	}
}
