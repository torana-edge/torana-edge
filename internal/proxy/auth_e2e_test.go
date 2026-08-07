package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Golden identity values for fixed vectors, computed ONCE at authoring time
// from sdk.ContentAddressedCacheKey over the same inputs the auth plugin
// composes. The integration boundary pins these literals so the test cannot
// drift together with an implementation that recomputes the algorithm.
const (
	goldenIdentityT1Tm1U1 = "auth-identity-v2:sha256:d66d5624f9d2345aac5be659937b7df1c46e50f2372a8cf400c35d2faf3168e6"
	goldenIdentityT1Tm1U2 = "auth-identity-v2:sha256:fba7180750a73b5c3115d16d8d940a961a774e760a41dc2e4150123d30d92787"
	goldenIdentityT1Tm2U1 = "auth-identity-v2:sha256:ead017e121a0d1771d13ec5eaaac20efc722e0bb52eeea484ca018b68e5fec7e"
	goldenIdentityT2Tm1U1 = "auth-identity-v2:sha256:429e6f4918e292906604009704136b39a7eb8342124d6fb5542eaff0dd54f165"
	goldenDigestTokenA    = "auth-verified-key-v2:sha256:525415c531efe883a60226b69a10c85ec59bb664763c5fac7683f290a28eca5a"
	goldenDigestTokenB    = "auth-verified-key-v2:sha256:7636ea3ef36095e1002324d2dafcc75774b2162219ad47135d37a0c303e352c5"

	// authBundleGrants is the v2 auth manifest's declared permission set,
	// used verbatim for approval-override pipelines (all-or-nothing).
	authBundleGrants = "env.host_call.verify_virtual_key,env.request_headers,env.set_identity"
)

// authEnvOptions drives the single auth harness.
type authEnvOptions struct {
	// wire is the verifier callback body; NIL leaves VerifyVirtualKeyFunc
	// unwired, exercising the host's absent-callback NOT_CONFIGURED branch.
	// It runs on the HTTP server goroutine, so it MUST NOT call t.Fatal;
	// record atomically and assert counts from the test goroutine.
	wire func() wasm.ExtensionResult
	// failureMode overrides the plugin's effective failure mode via the
	// approval ("pass" or "block"); empty uses the manifest default.
	failureMode string
}

// authEnv stands up the REAL server with the REAL v2 auth bundle from the
// bundles dir. Lifecycle is deterministic: the wired/unwired runtime is built
// through the server's own newRuntime, the pipeline is constructed and
// atomically swapped in (the displaced startup pipeline drained and closed
// SYNCHRONOUSLY — no request can hold it), and the transport wrapper is
// installed BEFORE Serve starts. A finite CONCURRENCY limit (RPM disabled)
// materializes limiter buckets without token-refill arithmetic.
//
// Observations: per-outgoing-request RouteContext.Identity; the attributed
// identity VERDICT (pipeline Verdicts(requestID).Identity() — plugin + value
// copy) read while request state is live at the delegating wrapper; verifier
// call count and exact payload bytes; limiter map keys under hashIdentity;
// upstream hit count.
func authEnv(t *testing.T, opts authEnvOptions) (
	post func(headers map[string]string, body string) (int, []byte),
	identities func() []string,
	verdicts func() []wasm.IdentityVerdict,
	verifier func() (int, [][]byte),
	limiterKeys func() []string,
	hits *int32,
) {
	t.Helper()
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "auth")

	var hitCount int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	providers := provider.Config{
		Providers: map[string]provider.Provider{
			"oai": {URL: upstream.URL, Format: "openai"},
		},
		Limits: provider.Limits{Concurrency: 8},
		// Deliberately NO Plugins config: New would otherwise load an unwired
		// auth pipeline (a second 9 MiB bundle load) and start a watcher,
		// both only to be torn down. The single wired/unwired pipeline below
		// is built through the server's own runtime factory and installed
		// before Serve; the startup pipeline New builds is EMPTY (nothing
		// loaded) and its watcher is canceled immediately.
	}
	srv, err := New(Config{Port: "8080", Providers: providers})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.watchCancel != nil {
		srv.watchCancel() // no bundle watcher in this test
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	// Test-scoped runtime: wire the verifier BEFORE the pipeline loads the
	// bundle, or leave it unwired to exercise the absent-callback branch.
	var (
		callMu   sync.Mutex
		callN    int
		payloads [][]byte
	)
	rt := srv.newRuntime()
	if opts.wire != nil {
		rt.VerifyVirtualKeyFunc = func(_ context.Context, payload string) wasm.ExtensionResult {
			// Copy the payload and release the recorder BEFORE invoking the
			// caller's wire body; wire runs on the HTTP server goroutine.
			callMu.Lock()
			callN++
			payloads = append(payloads, []byte(payload))
			callMu.Unlock()
			return opts.wire()
		}
	}
	cfg := plugin.PluginConfig{Dir: bundles, Order: []string{"auth"}, AllowUnapproved: true}
	if opts.failureMode != "" {
		digest, err := plugin.BundleDigestForDir(bundles + "/auth")
		if err != nil {
			t.Fatalf("BundleDigestForDir: %v", err)
		}
		grants := strings.Split(authBundleGrants, ",")
		cfg = plugin.PluginConfig{
			Dir:   bundles,
			Order: []string{"auth"},
			Approvals: map[string]plugin.Approval{
				"auth": {Digest: digest, Permissions: grants, FailureMode: opts.failureMode},
			},
		}
	}
	pp, err := plugin.NewPipeline(rt, cfg)
	if err != nil {
		rt.Close()
		t.Fatalf("NewPipeline: %v", err)
	}
	if old := srv.pluginPipeline.Swap(pp); old != nil {
		// The displaced startup pipeline is the EMPTY generation (no plugins
		// were configured at New); nothing can hold it, so close it
		// synchronously. No request can ever have used it.
		if n := old.(*plugin.PluginPipeline).Len(); n != 0 {
			t.Fatalf("displaced startup pipeline unexpectedly loaded %d plugins", n)
		}
		old.(*plugin.PluginPipeline).DrainAndClose()
	}

	// Transport wrapper: capture the route identity AND the attributed
	// verdict while request state is live, then delegate to the real
	// failover transport (the real limiter still acquires/releases).
	var (
		idMu      sync.Mutex
		seen      []string
		verdictsL []wasm.IdentityVerdict
		origRT    = srv.proxy.Transport
	)
	srv.proxy.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if rs := reqStateFrom(req.Context()); rs != nil {
			if rc, ok := req.Context().Value(routeContextKey{}).(*RouteContext); ok {
				idMu.Lock()
				seen = append(seen, rc.Identity)
				if rs.Pipeline != nil {
					if v := rs.Pipeline.Verdicts(rs.ID).Identity(); v != nil {
						verdictsL = append(verdictsL, wasm.IdentityVerdict{Plugin: v.Plugin, Identity: v.Identity})
					}
				}
				idMu.Unlock()
			}
		}
		return origRT.RoundTrip(req)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 30 * time.Second}
	post = func(headers map[string]string, body string) (int, []byte) {
		req, _ := http.NewRequest("POST", base+"/provider/oai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	identities = func() []string {
		idMu.Lock()
		defer idMu.Unlock()
		return append([]string(nil), seen...)
	}
	verdicts = func() []wasm.IdentityVerdict {
		idMu.Lock()
		defer idMu.Unlock()
		return append([]wasm.IdentityVerdict(nil), verdictsL...)
	}
	verifier = func() (int, [][]byte) {
		callMu.Lock()
		defer callMu.Unlock()
		return callN, append([][]byte(nil), payloads...)
	}
	limiterKeys = func() []string {
		srv.rateLimiter.mu.Lock()
		defer srv.rateLimiter.mu.Unlock()
		keys := make([]string, 0, len(srv.rateLimiter.limits))
		for k := range srv.rateLimiter.limits {
			keys = append(keys, k)
		}
		return keys
	}
	return post, identities, verdicts, verifier, limiterKeys, &hitCount
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

const authChatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

func okProfile(tenant, team, user string) wasm.ExtensionResult {
	profile := ""
	if tenant != "" || team != "" || user != "" {
		profile = `,"tenant_id":"` + tenant + `","team_id":"` + team + `","user_id":"` + user + `"`
	}
	return wasm.ExtensionValue([]byte(`{"status":"ok"` + profile + `}`))
}

func assertIdentity(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("transport identities = %v, want exactly [%s]", got, want)
	}
}

func assertBucketKeys(t *testing.T, keys []string, want ...string) {
	t.Helper()
	got := append([]string(nil), keys...)
	sort.Strings(got)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if len(got) != len(w) {
		t.Fatalf("limiter keys = %v, want %v", got, w)
	}
	for i := range w {
		if got[i] != w[i] {
			t.Fatalf("limiter keys = %v, want %v", got, w)
		}
	}
}

// assertVerdict pins the attributed identity verdict exactly: plugin auth
// with the given golden identity, recorded once per request.
func assertVerdict(t *testing.T, got []wasm.IdentityVerdict, wantIdentity string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("identity verdicts = %v, want exactly one", got)
	}
	if got[0].Plugin != "auth" || got[0].Identity != wantIdentity {
		t.Fatalf("identity verdict = %+v, want plugin=auth identity=%s", got[0], wantIdentity)
	}
}

// assertEveryVerdict pins that EXACTLY wantCount verdicts were recorded and
// EVERY one is the attributed auth verdict with the given identity. The
// cardinality is part of the assertion: a silently empty capture fails.
func assertEveryVerdict(t *testing.T, got []wasm.IdentityVerdict, wantIdentity string, wantCount int) {
	t.Helper()
	if len(got) != wantCount {
		t.Fatalf("verdict count = %d, want exactly %d: %v", len(got), wantCount, got)
	}
	for _, v := range got {
		if v.Plugin != "auth" || v.Identity != wantIdentity {
			t.Fatalf("verdict = %+v, want plugin=auth identity=%s (all: %v)", v, wantIdentity, got)
		}
	}
}

// assertNoVerdict pins that NO attributed identity verdict was recorded.
func assertNoVerdict(t *testing.T, got []wasm.IdentityVerdict) {
	t.Helper()
	if len(got) != 0 {
		t.Fatalf("unexpected identity verdicts: %v", got)
	}
}

// assertExactVerifyPayloads pins the callback bytes LITERALLY for every
// verified call.
func assertExactVerifyPayloads(t *testing.T, n int, payloads [][]byte, expected ...string) {
	t.Helper()
	if len(payloads) != n {
		t.Fatalf("recorded payloads = %d, want %d", len(payloads), n)
	}
	if len(expected) != n {
		t.Fatalf("expected literals = %d, want %d", len(expected), n)
	}
	for i := range expected {
		if string(payloads[i]) != expected[i] {
			t.Fatalf("payload %d = %q, want the exact bytes %q", i, payloads[i], expected[i])
		}
	}
}

// TestAuthIntegrationProfileIdentityShareAndGolden — two DIFFERENT virtual
// keys verified with the SAME non-empty profile produce the IDENTICAL golden
// identity (the token is not part of the profile identity), one shared
// limiter bucket, and an attributed auth verdict; every verify payload is
// byte-exact.
func TestAuthIntegrationProfileIdentityShareAndGolden(t *testing.T) {
	post, identities, verdicts, verifier, limiterKeys, _ := authEnv(t, authEnvOptions{
		wire: func() wasm.ExtensionResult { return okProfile("t1", "tm1", "u1") },
	})

	for _, key := range []string{"sk-torana-golden-a", "sk-torana-golden-b"} {
		status, _ := post(map[string]string{"Authorization": "Bearer " + key}, authChatBody)
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
	}
	got := identities()
	if len(got) != 2 || got[0] != goldenIdentityT1Tm1U1 || got[1] != goldenIdentityT1Tm1U1 {
		t.Fatalf("both requests must carry the golden identity: %v", got)
	}
	vs := verdicts()
	if len(vs) != 2 || vs[0].Plugin != "auth" || vs[1].Plugin != "auth" ||
		vs[0].Identity != goldenIdentityT1Tm1U1 || vs[1].Identity != goldenIdentityT1Tm1U1 {
		t.Fatalf("both requests must carry the golden attributed verdict: %v", vs)
	}
	assertBucketKeys(t, limiterKeys(), hashIdentity(goldenIdentityT1Tm1U1))

	n, payloads := verifier()
	assertExactVerifyPayloads(t, n, payloads,
		`{"key":"sk-torana-golden-a"}`, `{"key":"sk-torana-golden-b"}`)
}

// TestAuthIntegrationProfilePositionChanges — changing each profile position
// independently changes the identity, the verdict, and the bucket.
func TestAuthIntegrationProfilePositionChanges(t *testing.T) {
	cases := []struct {
		name    string
		profile func() wasm.ExtensionResult
		want    string
	}{
		{"user change", func() wasm.ExtensionResult { return okProfile("t1", "tm1", "u2") }, goldenIdentityT1Tm1U2},
		{"team change", func() wasm.ExtensionResult { return okProfile("t1", "tm2", "u1") }, goldenIdentityT1Tm2U1},
		{"tenant change", func() wasm.ExtensionResult { return okProfile("t2", "tm1", "u1") }, goldenIdentityT2Tm1U1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			post, identities, verdicts, verifier, limiterKeys, _ := authEnv(t, authEnvOptions{wire: tc.profile})
			status, _ := post(map[string]string{"Authorization": "Bearer sk-torana-golden-a"}, authChatBody)
			if status != 200 {
				t.Fatalf("status = %d", status)
			}
			assertIdentity(t, identities(), tc.want)
			assertVerdict(t, verdicts(), tc.want)
			n, payloads := verifier()
			assertExactVerifyPayloads(t, n, payloads, `{"key":"sk-torana-golden-a"}`)
			assertBucketKeys(t, limiterKeys(), hashIdentity(tc.want))
		})
	}
}

// TestAuthIntegrationTokenFallbackDomains — an ALL-EMPTY successful profile
// falls back to the verified-token digest: the same key shares a bucket,
// different keys separate, verdicts are the token digests.
func TestAuthIntegrationTokenFallbackDomains(t *testing.T) {
	post, identities, verdicts, verifier, limiterKeys, _ := authEnv(t, authEnvOptions{
		wire: func() wasm.ExtensionResult { return okProfile("", "", "") },
	})
	for _, key := range []string{"sk-torana-golden-a", "sk-torana-golden-a", "sk-torana-golden-b"} {
		status, _ := post(map[string]string{"Authorization": "Bearer " + key}, authChatBody)
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
	}
	got := identities()
	if len(got) != 3 || got[0] != goldenDigestTokenA || got[1] != goldenDigestTokenA || got[2] != goldenDigestTokenB {
		t.Fatalf("token-fallback identities = %v", got)
	}
	vs := verdicts()
	if len(vs) != 3 {
		t.Fatalf("token-fallback verdicts = %v, want 3", vs)
	}
	for _, v := range vs {
		if v.Plugin != "auth" {
			t.Fatalf("token-fallback verdict attribution = %+v", v)
		}
	}
	if vs[0].Identity != goldenDigestTokenA || vs[1].Identity != goldenDigestTokenA || vs[2].Identity != goldenDigestTokenB {
		t.Fatalf("token-fallback verdicts = %v", vs)
	}
	n, payloads := verifier()
	assertExactVerifyPayloads(t, n, payloads,
		`{"key":"sk-torana-golden-a"}`, `{"key":"sk-torana-golden-a"}`, `{"key":"sk-torana-golden-b"}`)
	assertBucketKeys(t, limiterKeys(), hashIdentity(goldenDigestTokenA), hashIdentity(goldenDigestTokenB))
}

// TestAuthIntegrationDeterminism — identical inputs produce byte-identical
// identities and verdicts across repeated requests.
func TestAuthIntegrationDeterminism(t *testing.T) {
	post, identities, verdicts, verifier, _, _ := authEnv(t, authEnvOptions{
		wire: func() wasm.ExtensionResult { return okProfile("t1", "tm1", "u1") },
	})
	for i := 0; i < 3; i++ {
		status, _ := post(map[string]string{"Authorization": "Bearer sk-torana-golden-a"}, authChatBody)
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
	}
	ids := identities()
	if len(ids) != 3 {
		t.Fatalf("route identity count = %d, want exactly 3: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id != goldenIdentityT1Tm1U1 {
			t.Fatalf("non-deterministic identity: %v", ids)
		}
	}
	assertEveryVerdict(t, verdicts(), goldenIdentityT1Tm1U1, 3)
	n, payloads := verifier()
	assertExactVerifyPayloads(t, n, payloads,
		`{"key":"sk-torana-golden-a"}`, `{"key":"sk-torana-golden-a"}`, `{"key":"sk-torana-golden-a"}`)
}

// TestAuthIntegrationEscapedEnvelopeByteExact — a valid virtual key containing
// printable characters that require JSON escaping (quote and backslash are
// inside the plugin's ASCII grammar) proves the auth plugin's encoder plus
// host transport together: the callback receives the exact escaped envelope.
func TestAuthIntegrationEscapedEnvelopeByteExact(t *testing.T) {
	post, identities, verdicts, verifier, _, _ := authEnv(t, authEnvOptions{
		wire: func() wasm.ExtensionResult { return okProfile("t1", "tm1", "u1") },
	})
	key := `sk-torana-a"b\c`
	status, _ := post(map[string]string{"Authorization": "Bearer " + key}, authChatBody)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	assertIdentity(t, identities(), goldenIdentityT1Tm1U1)
	assertVerdict(t, verdicts(), goldenIdentityT1Tm1U1)
	n, payloads := verifier()
	assertExactVerifyPayloads(t, n, payloads, `{"key":"sk-torana-a\"b\\c"}`)
}

// TestAuthIntegrationNoOverrideFallbacks — unwired and advisory outcomes
// produce NO identity verdict; the host fallback is the EXACT Authorization
// header. Provider keys, JWT-shaped bearers, and non-token credentials never
// call the verifier. Callback bodies never call t.Fatal; zero-call rows assert
// count==0 from the test goroutine. Authoritative domain rejection is covered
// separately below: it must never enter this fallback path.
func TestAuthIntegrationNoOverrideFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		opts        authEnvOptions
		headers     map[string]string
		wantID      string
		wantVerify  int
		wantPayload string
	}{
		"wired not configured refusal": {
			opts: authEnvOptions{wire: func() wasm.ExtensionResult {
				return wasm.ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "%s", "not wired")
			}},
			headers:     map[string]string{"Authorization": "Bearer sk-torana-nc"},
			wantID:      "Bearer sk-torana-nc",
			wantVerify:  1,
			wantPayload: `{"key":"sk-torana-nc"}`,
		},
		"UNWIRED absent callback": {
			// NO VerifyVirtualKeyFunc: the real plugin runs against a runtime
			// with the absent-callback branch, receives the dispatcher's
			// NOT_CONFIGURED, and passes with the exact fallback.
			opts:       authEnvOptions{wire: nil},
			headers:    map[string]string{"Authorization": "Bearer sk-torana-unwired"},
			wantID:     "Bearer sk-torana-unwired",
			wantVerify: 0,
		},
		"advisory unavailable": {
			opts: authEnvOptions{wire: func() wasm.ExtensionResult {
				return wasm.ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "%s", "down")
			}},
			headers:     map[string]string{"Authorization": "Bearer sk-torana-down"},
			wantID:      "Bearer sk-torana-down",
			wantVerify:  1,
			wantPayload: `{"key":"sk-torana-down"}`,
		},
		"provider key never verifies": {
			opts: authEnvOptions{wire: func() wasm.ExtensionResult {
				// Valid deterministic result; the test asserts count==0.
				return okProfile("t1", "tm1", "u1")
			}},
			headers:    map[string]string{"Authorization": "Bearer sk-proj-provider-secret"},
			wantID:     "Bearer sk-proj-provider-secret",
			wantVerify: 0,
		},
		"jwt never verifies": {
			opts: authEnvOptions{wire: func() wasm.ExtensionResult {
				return okProfile("t1", "tm1", "u1")
			}},
			headers:    map[string]string{"Authorization": "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.sig"},
			wantID:     "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.sig",
			wantVerify: 0,
		},
		"non-token x-api-key never verifies": {
			opts: authEnvOptions{wire: func() wasm.ExtensionResult {
				return okProfile("t1", "tm1", "u1")
			}},
			headers:    map[string]string{"X-Api-Key": "plain-secret"},
			wantID:     "", // host default identity (rc.Identity stays empty)
			wantVerify: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			post, identities, verdicts, verifier, limiterKeys, _ := authEnv(t, tc.opts)
			status, _ := post(tc.headers, authChatBody)
			if status != 200 {
				t.Fatalf("status = %d", status)
			}
			assertIdentity(t, identities(), tc.wantID)
			assertNoVerdict(t, verdicts())
			n, payloads := verifier()
			if n != tc.wantVerify {
				t.Fatalf("verifier calls = %d, want %d", n, tc.wantVerify)
			}
			if tc.wantPayload != "" {
				assertExactVerifyPayloads(t, n, payloads, tc.wantPayload)
			} else if n > 0 {
				t.Fatalf("expected zero verifier calls but got %d", n)
			}
			wantBucket := hashIdentity(tc.wantID)
			if tc.wantID == "" {
				// The host's default identity when no verdict applies and no
				// Authorization header is present is the literal "default".
				wantBucket = hashIdentity("default")
			}
			assertBucketKeys(t, limiterKeys(), wantBucket)
		})
	}
}

// TestAuthIntegrationRejectedVirtualKeysBlock proves the authoritative value
// arm cannot fall back to either the caller's Authorization credential or the
// host's default X-Api-Key identity. The verifier diagnostic is deliberately
// secret-bearing; only the plugin's fixed value-free denial may reach the
// client.
func TestAuthIntegrationRejectedVirtualKeysBlock(t *testing.T) {
	const wantBody = `{"error":{"code":"virtual_key_rejected","message":"The Torana virtual key was rejected.","type":"virtual_key_rejected"}}`
	for name, tc := range map[string]struct {
		headers map[string]string
		payload string
	}{
		"authorization": {
			headers: map[string]string{"Authorization": "Bearer sk-torana-revoked"},
			payload: `{"key":"sk-torana-revoked"}`,
		},
		"x-api-key": {
			headers: map[string]string{"X-Api-Key": "sk-torana-apikey"},
			payload: `{"key":"sk-torana-apikey"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			post, identities, verdicts, verifier, limiterKeys, hits := authEnv(t, authEnvOptions{
				wire: func() wasm.ExtensionResult {
					return wasm.ExtensionValue([]byte(`{"status":"rejected","message":"SECRET-verifier-diagnostic"}`))
				},
			})
			status, body := post(tc.headers, authChatBody)
			if status != http.StatusUnauthorized || string(body) != wantBody {
				t.Fatalf("rejection = status %d body %s", status, body)
			}
			if bytes.Contains(body, []byte("SECRET-verifier-diagnostic")) {
				t.Fatalf("verifier diagnostic leaked: %s", body)
			}
			assertNoVerdict(t, verdicts())
			n, payloads := verifier()
			assertExactVerifyPayloads(t, n, payloads, tc.payload)
			if got := identities(); len(got) != 1 || got[0] != "" {
				t.Fatalf("blocked route identities = %v, want one empty identity", got)
			}
			if got := limiterKeys(); len(got) != 0 {
				t.Fatalf("blocked request acquired limiter buckets: %v", got)
			}
			if got := atomic.LoadInt32(hits); got != 0 {
				t.Fatalf("blocked request reached upstream %d times", got)
			}
		})
	}
}

// TestAuthIntegrationMalformedResponses — malformed/protocol verifier
// responses (malformed JSON, unknown status) are NOT classified refusals and
// follow the effective failure mode. pass: exact fallback identity, no
// verdict, upstream reached. block: generic provider-shaped 502, exactly one
// verifier attempt with exact bytes, no verdict, no upstream, no bucket.
func TestAuthIntegrationMalformedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"malformed JSON": `{"status":`,
		"unknown status": `{"status":"maybe"}`,
	} {
		t.Run(name+"/pass", func(t *testing.T) {
			wire := body
			post, identities, verdicts, verifier, limiterKeys, hits := authEnv(t, authEnvOptions{
				wire: func() wasm.ExtensionResult { return wasm.ExtensionValue([]byte(wire)) },
			})
			status, _ := post(map[string]string{"Authorization": "Bearer sk-torana-malformed"}, authChatBody)
			if status != 200 {
				t.Fatalf("status = %d", status)
			}
			assertIdentity(t, identities(), "Bearer sk-torana-malformed")
			assertNoVerdict(t, verdicts())
			if atomic.LoadInt32(hits) != 1 {
				t.Fatalf("upstream hits = %d, want 1", atomic.LoadInt32(hits))
			}
			n, payloads := verifier()
			assertExactVerifyPayloads(t, n, payloads, `{"key":"sk-torana-malformed"}`)
			assertBucketKeys(t, limiterKeys(), hashIdentity("Bearer sk-torana-malformed"))
		})
		t.Run(name+"/block", func(t *testing.T) {
			wire := body
			post, identities, verdicts, verifier, limiterKeys, hits := authEnv(t, authEnvOptions{
				wire:        func() wasm.ExtensionResult { return wasm.ExtensionValue([]byte(wire)) },
				failureMode: "block",
			})
			status, respBody := post(map[string]string{"Authorization": "Bearer sk-torana-malformed"}, authChatBody)
			if status != 502 {
				t.Fatalf("status = %d, want 502; body=%s", status, respBody)
			}
			var errBody struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(respBody, &errBody); err != nil || errBody.Error.Code != "plugin_failure" {
				t.Fatalf("block body = %s", respBody)
			}
			assertNoVerdict(t, verdicts())
			// The failover transport is still invoked to synthesize the block;
			// it must observe EXACTLY ONE route identity and it must be the
			// EMPTY default — the request never acquired an override.
			ids := identities()
			if len(ids) != 1 || ids[0] != "" {
				t.Fatalf("block route identities = %v, want exactly one empty", ids)
			}
			if atomic.LoadInt32(hits) != 0 {
				t.Fatalf("upstream hits = %d, want 0", atomic.LoadInt32(hits))
			}
			n, payloads := verifier()
			assertExactVerifyPayloads(t, n, payloads, `{"key":"sk-torana-malformed"}`)
			assertBucketKeys(t, limiterKeys())
		})
	}
}

// TestAuthIntegrationContractRefusalFailureModes — a contract-class refusal
// follows the effective failure mode. pass: exact fallback identity + exact
// fallback limiter key, no verdict, upstream reached. block: provider-shaped
// 502, exactly one verifier attempt with exact bytes, no verdict, no
// upstream, no bucket.
func TestAuthIntegrationContractRefusalFailureModes(t *testing.T) {
	for name, mode := range map[string]string{"pass": "pass", "block": "block"} {
		t.Run(name, func(t *testing.T) {
			post, identities, verdicts, verifier, limiterKeys, hits := authEnv(t, authEnvOptions{
				wire: func() wasm.ExtensionResult {
					return wasm.ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "%s", "denied")
				},
				failureMode: mode,
			})
			status, respBody := post(map[string]string{"Authorization": "Bearer sk-torana-denied"}, authChatBody)
			assertNoVerdict(t, verdicts())
			n, payloads := verifier()
			assertExactVerifyPayloads(t, n, payloads, `{"key":"sk-torana-denied"}`)
			if mode == "pass" {
				if status != 200 {
					t.Fatalf("status = %d", status)
				}
				assertIdentity(t, identities(), "Bearer sk-torana-denied")
				if atomic.LoadInt32(hits) != 1 {
					t.Fatalf("upstream hits = %d, want 1", atomic.LoadInt32(hits))
				}
				assertBucketKeys(t, limiterKeys(), hashIdentity("Bearer sk-torana-denied"))
			} else {
				if status != 502 {
					t.Fatalf("status = %d, want 502; body=%s", status, respBody)
				}
				var errBody struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(respBody, &errBody); err != nil || errBody.Error.Code != "plugin_failure" {
					t.Fatalf("block body = %s", respBody)
				}
				if atomic.LoadInt32(hits) != 0 {
					t.Fatalf("upstream hits = %d, want 0", atomic.LoadInt32(hits))
				}
				ids := identities()
				if len(ids) != 1 || ids[0] != "" {
					t.Fatalf("block route identities = %v, want exactly one empty", ids)
				}
				assertBucketKeys(t, limiterKeys())
			}
		})
	}
}

// TestAuthIntegrationPrecedenceVerifiesOnce — Authorization wins over
// X-Api-Key and verification happens EXACTLY ONCE, including same-token
// duplication, with the exact typed payload bytes and the golden verdict.
func TestAuthIntegrationPrecedenceVerifiesOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		headers map[string]string
		wantKey string
	}{
		"conflicting keys": {
			headers: map[string]string{
				"Authorization": "Bearer sk-torana-auth",
				"X-Api-Key":     "sk-torana-apikey",
			},
			wantKey: "sk-torana-auth",
		},
		"same token duplicated": {
			headers: map[string]string{
				"Authorization": "Bearer sk-torana-same",
				"X-Api-Key":     "sk-torana-same",
			},
			wantKey: "sk-torana-same",
		},
	} {
		t.Run(name, func(t *testing.T) {
			post, identities, verdicts, verifier, _, _ := authEnv(t, authEnvOptions{
				wire: func() wasm.ExtensionResult { return okProfile("t1", "tm1", "u1") },
			})
			status, _ := post(tc.headers, authChatBody)
			if status != 200 {
				t.Fatalf("status = %d", status)
			}
			n, payloads := verifier()
			assertExactVerifyPayloads(t, n, payloads, `{"key":"`+tc.wantKey+`"}`)
			assertIdentity(t, identities(), goldenIdentityT1Tm1U1)
			assertVerdict(t, verdicts(), goldenIdentityT1Tm1U1)
		})
	}
}
