package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/provider"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
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

// offloadCall drives the REAL callback and decodes whichever arm it landed
// on: the value arm is the marshaled OffloadResult; a refusal is the framed
// classified HostError. Callers assert on the arm that matches their case.
func offloadCall(t *testing.T, s *Server, ctx context.Context, payload string) (economics.OffloadResult, *pbv2.HostError) {
	t.Helper()
	res := s.offloadCompletionResult(ctx, payload)
	if err := res.Validate(); err != nil {
		t.Fatalf("callback returned an invalid result: %v", err)
	}
	if res.Refusal() != nil {
		return economics.OffloadResult{}, res.Refusal()
	}
	var out economics.OffloadResult
	if err := json.Unmarshal(res.Value(), &out); err != nil {
		t.Fatalf("decode offload result: %v (body %q)", err, string(res.Value()))
	}
	return out, nil
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
	got, herr := offloadCall(t, s, ctx, `{"system_prompt":"sum","user_prompt":"text"}`)
	if herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
	}
	if got.Completion != "summary" {
		t.Fatalf("got %q want summary", got.Completion)
	}
}

func TestOffloadResultReturnsProviderModelAndUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"summary"}}],"usage":{"prompt_tokens":1200,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":900,"cache_write_tokens":100}}}`))
	}))
	defer upstream.Close()

	s, err := New(Config{Providers: offloadConfig(upstream.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1, CallerAuth: "k"})
	got, herr := offloadCall(t, s, ctx, `{"system_prompt":"sum","user_prompt":"text"}`)
	if herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
	}
	if got.Completion != "summary" || got.Provider != "cheap" || got.Model != "cheap-1" {
		t.Fatalf("identity/completion not returned: %+v", got)
	}
	if !got.Usage.Reported || got.Usage.InputTokens != 1200 || got.Usage.OutputTokens != 80 || got.Usage.CacheReadTokens != 900 || got.Usage.CacheWriteTokens != 100 || !got.Usage.InputIncludesCacheRead {
		t.Fatalf("usage not returned: %+v", got.Usage)
	}
}

func TestOffloadResultReturnsDeepSeekCacheUsageAndRecordsStats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"summary"}}],"usage":{"prompt_tokens":1200,"completion_tokens":80,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":300}}`))
	}))
	defer upstream.Close()

	s, err := New(Config{Providers: offloadConfig(upstream.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1, CallerAuth: "k"})
	got, herr := offloadCall(t, s, ctx, `{"system_prompt":"sum","user_prompt":"text"}`)
	if herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
	}
	if got.Usage.CacheReadTokens != 900 {
		t.Fatalf("DeepSeek cache usage not returned: %+v", got.Usage)
	}
	stats := s.stats.Snapshot()
	if stats.OffloadInputTokens != 1200 || stats.OffloadOutputTokens != 80 || stats.OffloadCacheReadTokens != 900 {
		t.Fatalf("offload usage not recorded: %+v", stats)
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
	if _, herr := offloadCall(t, s, ctx, `{"system_prompt":"sum","user_prompt":"text"}`); herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
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
	if _, herr := offloadCall(t, s, ctx, `{"system_prompt":"sum","user_prompt":"text"}`); herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
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
	_, herr := offloadCall(t, s, ctx, `{"system_prompt":"sum","user_prompt":"text"}`)
	if herr == nil {
		t.Fatal("expected a refusal for empty completion")
	}
	if !strings.Contains(herr.Message, "length") {
		t.Fatalf("refusal %q should surface finish_reason \"length\"", herr.Message)
	}
}

// TestOffloadProviderOverride: a plugin payload naming a different provider
// directs the call there (a local model, say), with its own model, and does
// NOT forward the caller's credential to that provider.
func TestOffloadProviderOverride(t *testing.T) {
	var gotModel, gotAuth string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"local-summary"}}]}`))
	}))
	defer local.Close()

	cfg := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: "http://unused", Format: "openai"},
			"local": {URL: local.URL, Format: "openai"},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}
	s, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1, CallerAuth: "caller-key"})
	got, herr := offloadCall(t, s, ctx, `{"system_prompt":"s","user_prompt":"u","provider":"local","model":"local-1"}`)
	if herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
	}
	if got.Completion != "local-summary" {
		t.Fatalf("got %q, want local-summary (call must hit the overridden provider)", got.Completion)
	}
	if gotModel != "local-1" {
		t.Fatalf("model = %q, want local-1", gotModel)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty — caller credential must NOT be forwarded to an overridden provider", gotAuth)
	}
}

// An override must not inherit the default provider's encrypted key. This is
// the credential-boundary regression: before the fix, resolving an empty
// override env still passed off.APIKeyEnc to resolveSecret.
func TestOffloadProviderOverrideCannotReceiveDefaultEncryptedKey(t *testing.T) {
	var gotAuth string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer local.Close()

	cfg := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: "http://unused", Format: "openai"},
			"local": {URL: local.URL, Format: "openai"},
		},
		Offload: provider.OffloadConfig{
			Enabled:  true,
			Provider: "cheap",
			Model:    "cheap-1",
		},
	}
	s, err := New(Config{Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	encrypted, err := s.secrets.Encrypt("default-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	s.config.Providers.Offload.APIKeyEnc = encrypted
	if _, herr := offloadCall(t, s, context.Background(), `{"provider":"local","model":"local-1","user_prompt":"u"}`); herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestOffloadProviderOverrideUsesOnlyOverrideProviderCredential(t *testing.T) {
	t.Setenv("TORANA_LOCAL_KEY", "local-secret")
	var gotAuth string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer local.Close()

	cfg := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: "http://unused", Format: "openai"},
			"local": {URL: local.URL, Format: "openai", APIKeyEnv: "TORANA_LOCAL_KEY"},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}
	s, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, herr := offloadCall(t, s, context.Background(), `{"provider":"local","model":"local-1","user_prompt":"u"}`); herr != nil {
		t.Fatalf("offloadCompletion refused: %v", herr)
	}
	if gotAuth != "Bearer local-secret" {
		t.Fatalf("Authorization = %q, want provider-scoped credential", gotAuth)
	}
}

// TestOffloadClassification is the F1 regression matrix over the REAL
// callback: every failure branch must land on the code a plugin can act on,
// instead of the old blanket UNAVAILABLE.
//
//   - INVALID_ARGUMENT: caller bugs — malformed payload, override missing its
//     model, guest-selected api_key_env.
//   - NOT_CONFIGURED: operator gaps — offload disabled, unknown override
//     provider.
//   - UNAVAILABLE: valid call that could not complete — transport failure,
//     upstream non-200.
func TestOffloadClassification(t *testing.T) {
	upstream := offloadServer(t, "", "cheap-1")
	defer upstream.Close()

	disabled := provider.Config{
		Providers: map[string]provider.Provider{"cheap": {URL: upstream.URL, Format: "openai"}},
		// Offload not enabled at all.
	}
	enabled := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: upstream.URL, Format: "openai"},
			"local": {URL: upstream.URL, Format: "openai"},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}
	withDead := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: upstream.URL, Format: "openai"},
			"dead":  {URL: "http://127.0.0.1:1", Format: "openai"},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}
	non200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusBadGateway)
	}))
	defer non200.Close()
	withNon200 := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: non200.URL, Format: "openai"},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}

	t.Run("malformed payload", func(t *testing.T) {
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `not json`)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("malformed payload: got %v, want INVALID_ARGUMENT", herr)
		}
	})
	t.Run("offload disabled", func(t *testing.T) {
		s, _ := New(Config{Providers: disabled})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u"}`)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
			t.Fatalf("disabled offload: got %v, want NOT_CONFIGURED", herr)
		}
	})
	t.Run("unknown override provider", func(t *testing.T) {
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u","provider":"ghost","model":"m"}`)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
			t.Fatalf("unknown override: got %v, want NOT_CONFIGURED", herr)
		}
	})
	t.Run("override without model", func(t *testing.T) {
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u","provider":"local"}`)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("override without model: got %v, want INVALID_ARGUMENT", herr)
		}
	})
	t.Run("guest-selected api_key_env", func(t *testing.T) {
		// The guest field is obsolete: it must be rejected even when it names
		// the provider's own configured variable — a guest may never select
		// process environment variables.
		t.Setenv("UNRELATED_PROCESS_SECRET", "must-not-leak")
		t.Setenv("TORANA_LOCAL_KEY", "local-secret")
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `{
			"provider":"local","model":"local-1","user_prompt":"u",
			"api_key_env":"TORANA_LOCAL_KEY"
		}`)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || !strings.Contains(herr.Message, "obsolete") {
			t.Fatalf("guest api_key_env: got %v, want INVALID_ARGUMENT naming the field obsolete", herr)
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		s, _ := New(Config{Providers: withDead})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u","provider":"dead","model":"m"}`)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE {
			t.Fatalf("dead endpoint: got %v, want UNAVAILABLE", herr)
		}
	})
	t.Run("upstream non-200", func(t *testing.T) {
		s, _ := New(Config{Providers: withNon200})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u"}`)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE {
			t.Fatalf("non-200: got %v, want UNAVAILABLE", herr)
		}
	})
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

// TestOffloadRejectsApiKeyEnvPresence — the field is REMOVED, not deprecated:
// any occurrence — empty string or null — is rejected as INVALID_ARGUMENT,
// not silently honored or ignored.
func TestOffloadRejectsApiKeyEnvPresence(t *testing.T) {
	upstream := offloadServer(t, "", "cheap-1")
	defer upstream.Close()

	s, err := New(Config{Providers: offloadConfig(upstream.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})

	for _, payload := range []string{
		`{"system_prompt":"s","user_prompt":"u","api_key_env":""}`,
		`{"system_prompt":"s","user_prompt":"u","api_key_env":null}`,
		`{"system_prompt":"s","user_prompt":"u","api_key_env":"MY_VAR"}`,
	} {
		_, herr := offloadCall(t, s, ctx, payload)
		if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("payload %s was not rejected as INVALID_ARGUMENT: %v", payload, herr)
		}
	}
}

// TestOffloadResponseLimits — an oversized offload body is a refusal, not a
// truncated completion; an exactly-at-limit body is a full success.
func TestOffloadResponseLimits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size       int
		wantRefuse bool
	}{
		{"exact limit", maxOffloadResponseBytes, false},
		{"over limit", maxOffloadResponseBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A body that is exactly the limit AND parses: pad the content
			// field so the whole JSON lands on the byte boundary.
			prefix := `{"choices":[{"message":{"content":"`
			suffix := `"}}]}`
			content := strings.Repeat("a", tc.size-len(prefix)-len(suffix))
			body := prefix + content + suffix

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(body))
			}))
			defer upstream.Close()

			s, err := New(Config{Providers: offloadConfig(upstream.URL)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})

			got, herr := offloadCall(t, s, ctx, `{"system_prompt":"s","user_prompt":"u"}`)
			if tc.wantRefuse {
				if herr == nil || herr.Code != pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE {
					t.Fatalf("oversized body was not refused as UNAVAILABLE: %v", herr)
				}
			} else {
				if herr != nil {
					t.Fatalf("exact-limit body refused: %v", herr)
				}
				if len(got.Completion) != tc.size-len(prefix)-len(suffix) {
					t.Errorf("completion = %d chars, want %d", len(got.Completion), tc.size-len(prefix)-len(suffix))
				}
			}
		})
	}
}
