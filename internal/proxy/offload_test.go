package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/provider"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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
			"cheap": {URL: url, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
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
func offloadCall(t *testing.T, s *Server, ctx context.Context, payload string) (economics.OffloadResult, *pbv1.HostError) {
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

// TestOffloadCallerAuthIsRejectedAtStartup proves that a host-originated call
// cannot borrow a request credential. The selected provider must use
// credential or none mode, and an invalid configuration never starts.
func TestOffloadCallerAuthIsRejectedAtStartup(t *testing.T) {
	upstream := offloadServer(t, "", "cheap-1")
	defer upstream.Close()
	cfg := offloadConfig(upstream.URL)
	cheap := cfg.Providers["cheap"]
	cheap.Auth = provider.ProviderAuth{Mode: "caller"}
	cfg.Providers["cheap"] = cheap
	if _, err := New(Config{Providers: cfg}); err == nil || !strings.Contains(err.Error(), "host-originated offload requires credential or none") {
		t.Fatalf("New error = %v, want caller-auth rejection", err)
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
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})
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
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})
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

func TestOffloadUsesProviderCredential(t *testing.T) {
	upstream := offloadServer(t, "Bearer dedicated-key", "cheap-1")
	defer upstream.Close()

	t.Setenv("TORANA_TEST_OFFLOAD_KEY", "dedicated-key")
	cfg := offloadConfig(upstream.URL)
	cfg.Credentials = provider.CredentialsConfig{
		Sources: map[string]provider.CredentialSource{"env": {Type: "env"}},
		Entries: map[string]provider.CredentialEntry{"offload": {Source: "env", Key: "TORANA_TEST_OFFLOAD_KEY"}},
	}
	cheap := cfg.Providers["cheap"]
	cheap.Auth = provider.ProviderAuth{Mode: "credential", Credential: "offload"}
	cfg.Providers["cheap"] = cheap

	s, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})
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
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})
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
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})
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
			"cheap": {URL: "http://unused", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
			"local": {URL: local.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}
	s, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})
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

// An override must not inherit the default provider's named credential.
func TestOffloadProviderOverrideCannotReceiveDefaultCredential(t *testing.T) {
	var gotAuth string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer local.Close()

	cfg := provider.Config{
		Credentials: provider.CredentialsConfig{
			Sources: map[string]provider.CredentialSource{"env": {Type: "env"}},
			Entries: map[string]provider.CredentialEntry{"default": {Source: "env", Key: "TORANA_DEFAULT_KEY"}},
		},
		Providers: map[string]provider.Provider{
			"cheap": {URL: "http://unused", Format: "openai", Auth: provider.ProviderAuth{Mode: "credential", Credential: "default"}},
			"local": {URL: local.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
		},
		Offload: provider.OffloadConfig{
			Enabled:  true,
			Provider: "cheap",
			Model:    "cheap-1",
		},
	}
	t.Setenv("TORANA_DEFAULT_KEY", "default-secret")
	s, err := New(Config{Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
		Credentials: provider.CredentialsConfig{
			Sources: map[string]provider.CredentialSource{"env": {Type: "env"}},
			Entries: map[string]provider.CredentialEntry{"local": {Source: "env", Key: "TORANA_LOCAL_KEY"}},
		},
		Providers: map[string]provider.Provider{
			"cheap": {URL: "http://unused", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
			"local": {URL: local.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "credential", Credential: "local"}},
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
//   - INVALID_ARGUMENT: caller bugs — malformed payload, unknown members, or
//     an override missing its model.
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
			"cheap": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
			"local": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}
	withDead := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
			"dead":  {URL: "http://127.0.0.1:1", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}
	non200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusBadGateway)
	}))
	defer non200.Close()
	withNon200 := provider.Config{
		Providers: map[string]provider.Provider{
			"cheap": {URL: non200.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}},
		},
		Offload: provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"},
	}

	t.Run("malformed payload", func(t *testing.T) {
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `not json`)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("malformed payload: got %v, want INVALID_ARGUMENT", herr)
		}
	})
	t.Run("offload disabled", func(t *testing.T) {
		s, _ := New(Config{Providers: disabled})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u"}`)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
			t.Fatalf("disabled offload: got %v, want NOT_CONFIGURED", herr)
		}
	})
	t.Run("unknown override provider", func(t *testing.T) {
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u","provider":"ghost","model":"m"}`)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
			t.Fatalf("unknown override: got %v, want NOT_CONFIGURED", herr)
		}
	})
	t.Run("override without model", func(t *testing.T) {
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u","provider":"local"}`)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("override without model: got %v, want INVALID_ARGUMENT", herr)
		}
	})
	t.Run("unknown payload member", func(t *testing.T) {
		s, _ := New(Config{Providers: enabled})
		_, herr := offloadCall(t, s, context.Background(), `{
			"provider":"local","model":"local-1","user_prompt":"u",
			"unexpected":"value"
		}`)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("unknown member: got %v, want INVALID_ARGUMENT", herr)
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		s, _ := New(Config{Providers: withDead})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u","provider":"dead","model":"m"}`)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
			t.Fatalf("dead endpoint: got %v, want UNAVAILABLE", herr)
		}
	})
	t.Run("upstream non-200", func(t *testing.T) {
		s, _ := New(Config{Providers: withNon200})
		_, herr := offloadCall(t, s, context.Background(), `{"user_prompt":"u"}`)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
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

func TestOffloadRejectsUnknownMembers(t *testing.T) {
	upstream := offloadServer(t, "", "cheap-1")
	defer upstream.Close()

	s, err := New(Config{Providers: offloadConfig(upstream.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})

	for _, payload := range []string{
		`{"system_prompt":"s","user_prompt":"u","unexpected":""}`,
		`{"system_prompt":"s","user_prompt":"u","unexpected":null}`,
		`{"system_prompt":"s","user_prompt":"u","unexpected":{"nested":true}}`,
	} {
		_, herr := offloadCall(t, s, ctx, payload)
		if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
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
				if herr == nil || herr.Code != pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
					t.Fatalf("oversized body was not refused as INVALID_ARGUMENT: %v", herr)
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

// TestOffloadRedirectsStayOnOrigin — the offload client uses the same
// redirect policy as egress: a cross-origin redirect must not be followed
// (no credential crosses, the attacker receives nothing), and a same-origin
// loop terminates at the ten-hop bound with a non-retryable refusal.
func TestOffloadRedirectsStayOnOrigin(t *testing.T) {
	t.Run("cross-origin redirect is not followed", func(t *testing.T) {
		var attackerMu sync.Mutex
		attackerHits := 0
		var attackerAuth string
		attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attackerMu.Lock()
			attackerHits++
			attackerAuth = r.Header.Get("Authorization")
			attackerMu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		defer attacker.Close()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
		}))
		defer upstream.Close()

		s, err := New(Config{Providers: offloadConfig(upstream.URL)})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})

		_, herr := offloadCall(t, s, ctx, `{"system_prompt":"s","user_prompt":"u"}`)
		if herr == nil {
			t.Fatal("a cross-origin redirect completed")
		}
		attackerMu.Lock()
		hits, auth := attackerHits, attackerAuth
		attackerMu.Unlock()
		if hits != 0 {
			t.Fatalf("attacker server saw %d requests", hits)
		}
		if auth != "" {
			t.Fatalf("credential reached the attacker: auth=%q", auth)
		}
	})

	t.Run("same-origin redirect loop is bounded", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
		}))
		defer upstream.Close()

		s, err := New(Config{Providers: offloadConfig(upstream.URL)})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 1})

		_, herr := offloadCall(t, s, ctx, `{"system_prompt":"s","user_prompt":"u"}`)
		if herr == nil {
			t.Fatal("a redirect loop completed")
		}
		if herr.Code != pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
			t.Fatalf("a redirect loop was classified %v, want NOT_CONFIGURED (deterministic, non-retryable)", herr.Code)
		}
		mu.Lock()
		n := hits
		mu.Unlock()
		if n > 11 {
			t.Fatalf("redirect loop issued %d requests, want <= 11 (10-hop bound)", n)
		}
		if n < 2 {
			t.Fatalf("redirect loop issued %d requests, want > 1 (the loop was actually followed)", n)
		}
	})
}
