package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestOffloadRequestBudget: the offload request must reserve enough token
// budget to cover a reasoning model's reasoning_content plus the summary,
// otherwise reasoning-heavy inputs come back with empty content
// (finish_reason "length"). Regression guard for the dogfood-observed
// "offload: empty response" failures against deepseek-v4-flash.
func TestOffloadRequestBudget(t *testing.T) {
	var gotMaxTokens float64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		gotMaxTokens, _ = req["max_tokens"].(float64)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"summary"}}]}`))
	}))
	defer upstream.Close()

	s, err := New(Config{Providers: offloadConfig(upstream.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1, CallerAuth: "k"})
	if _, err := s.offloadCompletion(ctx, `{"system_prompt":"sum","user_prompt":"text"}`); err != nil {
		t.Fatalf("offloadCompletion: %v", err)
	}
	if gotMaxTokens < 4096 {
		t.Fatalf("offload max_tokens = %v, want >= 4096 (reasoning budget headroom)", gotMaxTokens)
	}
}

// TestOffloadEmptyContentSurfacesFinishReason: an empty completion must
// report the finish_reason so budget-exhaustion ("length") is distinguishable
// from other empties in logs/stats.
func TestOffloadEmptyContentSurfacesFinishReason(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"content":""}}]}`))
	}))
	defer upstream.Close()

	s, err := New(Config{Providers: offloadConfig(upstream.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1, CallerAuth: "k"})
	_, err = s.offloadCompletion(ctx, `{"system_prompt":"sum","user_prompt":"text"}`)
	if err == nil {
		t.Fatal("expected error for empty completion")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Fatalf("error %q should surface finish_reason \"length\"", err)
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
