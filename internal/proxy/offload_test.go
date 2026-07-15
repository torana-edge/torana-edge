package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func offloadServer(t *testing.T, wantAuth, wantModel string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("Authorization: got %q want %q", got, wantAuth)
		}
		var req struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != wantModel {
			t.Errorf("model: got %q want %q", req.Model, wantModel)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"summary"}}]}`))
	}))
}

func offloadConfig(url string) provider.Config {
	return provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: url, Format: "openai"},
		},
		Offload: provider.OffloadConfig{
			Enabled:  true,
			Provider: "cheap",
			Model:    "cheap-1",
		},
	}
}

// TestOffloadUsesCallerCredential: without a dedicated key, the caller's
// request credential is forwarded to the offload provider.
func TestOffloadUsesCallerCredential(t *testing.T) {
	upstream := offloadServer(t, "Bearer caller-key", "cheap-1")
	defer upstream.Close()

	s, err := New(Config{Providers: offloadConfig(upstream.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1, CallerAuth: "caller-key"})
	got, err := s.offloadCompletion(ctx, `{"system_prompt":"sum","user_prompt":"text"}`)
	if err != nil {
		t.Fatalf("offloadCompletion: %v", err)
	}
	if got != "summary" {
		t.Fatalf("got %q want summary", got)
	}
}

// TestOffloadDedicatedKeyWins: offload.api_key_env overrides the caller key.
func TestOffloadDedicatedKeyWins(t *testing.T) {
	upstream := offloadServer(t, "Bearer dedicated-key", "cheap-1")
	defer upstream.Close()

	t.Setenv("TORANA_TEST_OFFLOAD_KEY", "dedicated-key")
	cfg := offloadConfig(upstream.URL)
	cfg.Offload.APIKeyEnv = "TORANA_TEST_OFFLOAD_KEY"

	s, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1, CallerAuth: "caller-key"})
	if _, err := s.offloadCompletion(ctx, `{"system_prompt":"sum","user_prompt":"text"}`); err != nil {
		t.Fatalf("offloadCompletion: %v", err)
	}
}

// TestOffloadDisabledErrors: offload without config errors instead of
// guessing a provider.
func TestOffloadDisabledErrors(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.offloadCompletion(context.Background(), `{"user_prompt":"x"}`); err == nil {
		t.Fatal("expected error for unconfigured offload")
	}
}

// TestOffloadValidation: enabling offload with a bad reference fails at
// startup (regression guard for torana-edge#20).
func TestOffloadValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  provider.OffloadConfig
	}{
		{"missing provider", provider.OffloadConfig{Enabled: true, Provider: "ghost", Model: "m"}},
		{"wrong format", provider.OffloadConfig{Enabled: true, Provider: "anth", Model: "m"}},
		{"missing model", provider.OffloadConfig{Enabled: true, Provider: "ok"}},
	}
	providers := map[string]provider.Provider{
		"ok":   {URL: "http://x", Format: "openai"},
		"anth": {URL: "http://y", Format: "anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{Providers: provider.Config{Providers: providers, Offload: tc.cfg}})
			if err == nil {
				t.Fatal("expected New to fail")
			}
		})
	}
}
