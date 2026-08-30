package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

func TestHasHook_MatchAfterManifestFix(t *testing.T) {
	m := PluginManifest{
		Name: "test-plugin",
		Hooks: []Hook{
			{Name: "run_before_request"},
		},
	}
	if !hasHook(m, "run_before_request") {
		t.Error("hasHook should match run_before_request after manifest fix")
	}
	if hasHook(m, "on_chat_request") {
		t.Error("hasHook should NOT match on_chat_request — manifests were updated")
	}
}

func TestHasHook_MultipleHooks(t *testing.T) {
	m := PluginManifest{
		Name: "multi-hook",
		Hooks: []Hook{
			{Name: "run_before_request"},
			{Name: "run_after_response"},
			{Name: "run_on_stream_chunk"},
		},
	}
	for _, h := range []string{"run_before_request", "run_after_response", "run_on_stream_chunk"} {
		if !hasHook(m, h) {
			t.Errorf("hasHook should match %s", h)
		}
	}
	if hasHook(m, "on_chat_request") {
		t.Error("hasHook should NOT match on_chat_request")
	}
}

func TestHookNames(t *testing.T) {
	hooks := []Hook{
		{Name: "run_before_request"},
		{Name: "run_after_response"},
	}
	names := hookNames(hooks)
	if len(names) != 2 {
		t.Fatalf("expected 2 hook names, got %d", len(names))
	}
	if names[0] != "run_before_request" || names[1] != "run_after_response" {
		t.Errorf("unexpected hook names: %v", names)
	}
}

func TestManifestResourceDeclarationsAreCapabilityBound(t *testing.T) {
	base := PluginManifest{
		SchemaVersion: 1,
		ID:            "local/resources",
		Name:          "resources",
		Version:       "0.1.0",
		ABIVersion:    "v1",
		FailureMode:   "pass",
		Hooks:         []Hook{{Name: "run_before_request"}},
	}
	tests := []struct {
		name      string
		mutate    func(*PluginManifest)
		wantError string
	}{
		{
			name: "credential needs capability",
			mutate: func(m *PluginManifest) {
				m.Credentials = []CredentialDeclaration{{Slot: "api", Description: "service key", Required: true}}
			},
			wantError: "env.credential_get",
		},
		{
			name: "file operation needs matching capability",
			mutate: func(m *PluginManifest) {
				m.Files = []FileDeclaration{{Path: "usage.jsonl", Operations: []string{"append"}, MaxBytes: 100}}
			},
			wantError: "env.file_append",
		},
		{
			name: "HTTP needs capability",
			mutate: func(m *PluginManifest) {
				m.HTTPEndpoints = []HTTPEndpointDeclaration{{Name: "api", Description: "service", Origin: "https://api.example", Methods: []string{"GET"}}}
			},
			wantError: "env.http_request",
		},
		{
			name: "HTTP plaintext public origin refused",
			mutate: func(m *PluginManifest) {
				m.Permissions = []Permission{{Name: "env.http_request"}}
				m.HTTPEndpoints = []HTTPEndpointDeclaration{{Name: "api", Description: "service", Origin: "http://api.example", Methods: []string{"GET"}}}
			},
			wantError: "literal loopback",
		},
		{
			name: "complete declaration accepted",
			mutate: func(m *PluginManifest) {
				m.Permissions = []Permission{{Name: "env.credential_get"}, {Name: "env.file_append"}, {Name: "env.http_request"}}
				m.Credentials = []CredentialDeclaration{{Slot: "api", Description: "service key", Required: true}}
				m.Files = []FileDeclaration{{Path: "usage.jsonl", Operations: []string{"append"}, MaxBytes: 100}}
				m.HTTPEndpoints = []HTTPEndpointDeclaration{{Name: "api", Description: "service", Origin: "https://api.example", Methods: []string{"GET"}, MaxCallsPerMinute: 5}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			test.mutate(&manifest)
			err := validateManifest(manifest)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestResolvePluginResourcesUsesApprovalNotGuestInput(t *testing.T) {
	manifest := PluginManifest{
		Credentials: []CredentialDeclaration{{Slot: "service", Description: "service key", Required: true}},
		Files:       []FileDeclaration{{Path: "usage.jsonl", Operations: []string{"append"}, MaxBytes: 200, RetainedFiles: 3}},
		HTTPEndpoints: []HTTPEndpointDeclaration{{
			Name: "billing", Description: "billing API", Origin: "https://suggested.example", Methods: []string{"GET", "POST"}, Required: true,
		}},
	}
	resources, err := resolvePluginResources(manifest, Approval{
		Credentials: map[string]string{"service": "operator-id"},
		HTTPEndpoints: map[string]HTTPApproval{"billing": {
			Origin: "https://approved.example", Methods: []string{"POST"}, TimeoutMS: 900,
			MaxRequestBytes: 100, MaxResponseBytes: 300, MaxCallsPerMinute: 4,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resources.Credentials["service"] != "operator-id" {
		t.Fatalf("credentials = %+v", resources.Credentials)
	}
	if file := resources.Files["usage.jsonl"]; file.MaxBytes != 200 || file.RetainedFiles != 3 || !file.Operations["append"] {
		t.Fatalf("file = %+v", file)
	}
	if endpoint := resources.HTTP["billing"]; endpoint.Origin != "https://approved.example" || !endpoint.Methods["POST"] || endpoint.Methods["GET"] || endpoint.MaxCallsPerMinute != 4 {
		t.Fatalf("HTTP endpoint = %+v", endpoint)
	}

	if _, err := resolvePluginResources(manifest, Approval{Credentials: map[string]string{}, HTTPEndpoints: map[string]HTTPApproval{}}); err == nil || !strings.Contains(err.Error(), "required credential") {
		t.Fatalf("missing required binding error = %v", err)
	}
}

func TestDiscoverPlugins_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	bundles, err := DiscoverPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 0 {
		t.Errorf("expected 0 bundles in empty dir, got %d", len(bundles))
	}
}

func TestPipelineDrainRejectsNewRequestsUntilPinnedWorkFinishes(t *testing.T) {
	rt := wasm.NewRuntime(context.Background())
	pp, err := NewPipeline(rt, PluginConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if !pp.TryAcquire() {
		t.Fatal("initial request was not admitted")
	}

	drained := make(chan struct{})
	go func() {
		pp.DrainAndClose()
		close(drained)
	}()

	deadline := time.After(time.Second)
	for !isDraining(pp) {
		select {
		case <-deadline:
			t.Fatal("pipeline never entered draining state")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if pp.TryAcquire() {
		t.Fatal("draining pipeline admitted a new request")
	}
	select {
	case <-drained:
		t.Fatal("pipeline closed while a request was pinned")
	default:
	}

	pp.Release()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("pipeline did not close after pinned work finished")
	}
}

func TestValidateApprovalBindsDigestAndRequestedPermissions(t *testing.T) {
	bundle := PluginBundle{
		Digest: "sha256:installed",
		Manifest: PluginManifest{
			FailureMode: "block",
			Permissions: []Permission{{Name: "env.log"}},
		},
	}

	if _, _, err := validateApproval(bundle, Approval{
		Digest: "sha256:other",
	}); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	if _, _, err := validateApproval(bundle, Approval{
		Digest:      bundle.Digest,
		Permissions: []string{"env.route_request"},
	}); err == nil {
		t.Fatal("unrequested permission was accepted")
	}

	grants, failureMode, err := validateApproval(bundle, Approval{
		Digest:      bundle.Digest,
		Permissions: []string{"env.log"},
	})
	if err != nil {
		t.Fatalf("valid approval: %v", err)
	}
	if len(grants) != 1 || grants[0] != "env.log" {
		t.Fatalf("grants = %v", grants)
	}
	if failureMode != "block" {
		t.Fatalf("failure mode = %q, want manifest recommendation block", failureMode)
	}
}

// All-or-nothing: approving a STRICT SUBSET of a multi-permission manifest is
// rejected — the empty subset of a grant-declaring manifest is exactly that,
// and a plugin enabled without capabilities it declared necessary degrades
// silently. The direct missing-permission regression keeps the equality loop
// honest (deleting it leaves this suite red).
func TestValidateApprovalRejectsStrictSubset(t *testing.T) {
	bundle := PluginBundle{
		Digest: "sha256:installed",
		Manifest: PluginManifest{
			Permissions: []Permission{{Name: "env.log"}, {Name: "ir.model.write"}},
		},
	}

	for _, subset := range [][]string{
		{"env.log"},
		{"ir.model.write"},
		nil, // the empty subset
	} {
		if _, _, err := validateApproval(bundle, Approval{
			Digest:      bundle.Digest,
			Permissions: subset,
		}); err == nil {
			t.Errorf("strict subset %v was accepted", subset)
		}
	}

	// The full declared set is the only valid approval.
	grants, _, err := validateApproval(bundle, Approval{
		Digest:      bundle.Digest,
		Permissions: []string{"env.log", "ir.model.write"},
	})
	if err != nil {
		t.Fatalf("full approval: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("grants = %v", grants)
	}
}

func TestValidateApprovalAllowsOperatorFailureModeOverride(t *testing.T) {
	bundle := PluginBundle{
		Digest: "sha256:installed",
		Manifest: PluginManifest{
			FailureMode: "block",
		},
	}
	_, failureMode, err := validateApproval(bundle, Approval{
		Digest:      bundle.Digest,
		FailureMode: "pass",
	})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if failureMode != "pass" {
		t.Fatalf("failure mode = %q, want operator override pass", failureMode)
	}
}

func TestBundleDigestCoversCodeAndPolicyFiles(t *testing.T) {
	withoutAgent := bundleDigest(
		[]byte(`{"failure_mode":"pass"}`),
		[]byte("wasm"),
		[]byte(`{"type":"object"}`),
		nil,
	)
	base := bundleDigest(
		[]byte(`{"failure_mode":"pass"}`),
		[]byte("wasm"),
		[]byte(`{"type":"object"}`),
		[]byte(`{"schema_version":1}`),
	)
	cases := []string{
		bundleDigest([]byte(`{"failure_mode":"block"}`), []byte("wasm"), []byte(`{"type":"object"}`), []byte(`{"schema_version":1}`)),
		bundleDigest([]byte(`{"failure_mode":"pass"}`), []byte("wasm2"), []byte(`{"type":"object"}`), []byte(`{"schema_version":1}`)),
		bundleDigest([]byte(`{"failure_mode":"pass"}`), []byte("wasm"), []byte(`{"type":"string"}`), []byte(`{"schema_version":1}`)),
		bundleDigest([]byte(`{"failure_mode":"pass"}`), []byte("wasm"), []byte(`{"type":"object"}`), []byte(`{"schema_version":2}`)),
	}
	for _, changed := range cases {
		if changed == base {
			t.Fatal("bundle digest did not change when a consumed file changed")
		}
	}
	if withoutAgent == base {
		t.Fatal("adding agent.json did not change the bundle digest")
	}
}

func TestBundleDigestGoldenVector(t *testing.T) {
	const withoutAgent = "sha256:8743ff3f6066c1859164f246e463e5dcff44f1a4e26cdc2193d62f51fd128ecb"
	const withAgent = "sha256:4a00f9b037bbc9138735d6cbc7fdb54348cd1f54fe99302e73d06d6d3b36e708"
	if got := bundleDigest([]byte("manifest"), []byte("wasm"), []byte("schema"), nil); got != withoutAgent {
		t.Fatalf("legacy digest = %s, want %s", got, withoutAgent)
	}
	if got := bundleDigest([]byte("manifest"), []byte("wasm"), []byte("schema"), []byte("agent")); got != withAgent {
		t.Fatalf("agent digest = %s, want %s", got, withAgent)
	}
}

func TestBundleDigestDistinguishesAbsentAndEmptyOptionalFile(t *testing.T) {
	absent := bundleDigest([]byte("manifest"), []byte("wasm"), []byte("schema"), nil)
	presentEmpty := bundleDigest([]byte("manifest"), []byte("wasm"), []byte("schema"), []byte{})
	if absent == presentEmpty {
		t.Fatal("absent agent.json and a present empty agent.json have the same approval digest")
	}
}

func TestValidateAgentDescriptor(t *testing.T) {
	manifest := PluginManifest{
		Name:        "example",
		Hooks:       []Hook{{Name: "run_on_http_request"}},
		Permissions: []Permission{{Name: "env.serve_http"}},
	}
	valid := AgentDescriptor{
		SchemaVersion: 1,
		Operations: []AgentOperation{{
			ID:           "status",
			Method:       "GET",
			Path:         "/status",
			Description:  "Read plugin status.",
			Risk:         "read",
			Idempotent:   true,
			OutputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	if err := validateAgentDescriptor(valid, manifest); err != nil {
		t.Fatalf("valid descriptor: %v", err)
	}

	invalid := valid
	invalid.Operations = append([]AgentOperation(nil), valid.Operations...)
	invalid.Operations[0].Path = "/../config"
	if err := validateAgentDescriptor(invalid, manifest); err == nil {
		t.Fatal("path traversal was accepted")
	}
	invalid.Operations[0].Path = "/st%61tus"
	if err := validateAgentDescriptor(invalid, manifest); err == nil {
		t.Fatal("non-canonical escaped path was accepted")
	}
	invalid.Operations[0].Path = "/foo/./bar"
	if err := validateAgentDescriptor(invalid, manifest); err == nil {
		t.Fatal("dot path segment was accepted")
	}

	invalid = valid
	invalid.Operations = append([]AgentOperation(nil), valid.Operations...)
	invalid.Operations[0].Method = "POST"
	if err := validateAgentDescriptor(invalid, manifest); err == nil {
		t.Fatal("read-risk POST was accepted")
	}

	manifest.Hooks = nil
	if err := validateAgentDescriptor(valid, manifest); err == nil {
		t.Fatal("descriptor without HTTP hook was accepted")
	}
}

func TestPipelineAgentOperationsComeFromLoadedContract(t *testing.T) {
	pipeline := &PluginPipeline{plugins: []*loadedPlugin{{
		manifest: PluginManifest{ID: "torana/example", Name: "example"},
		digest:   "sha256:approved",
		agent: &AgentDescriptor{
			SchemaVersion: 1,
			Operations: []AgentOperation{{
				ID:           "status",
				Method:       "GET",
				Path:         "/status",
				Description:  "Read status.",
				Risk:         "read",
				Idempotent:   true,
				OutputSchema: json.RawMessage(`{"type":"object"}`),
			}},
		},
	}}}

	loaded := pipeline.AgentPlugins()
	if len(loaded) != 1 || loaded[0].Digest != "sha256:approved" {
		t.Fatalf("loaded agent plugins = %#v", loaded)
	}
	loaded[0].Descriptor.Operations[0].Path = "/changed"

	operation, allowed, found := pipeline.FindAgentOperation("example", "GET", "/status")
	if !found || operation == nil || operation.ID != "status" {
		t.Fatalf("approved operation not found: operation=%#v allowed=%v found=%v", operation, allowed, found)
	}
	if len(allowed) != 1 || allowed[0] != "GET" {
		t.Fatalf("allowed methods = %v, want [GET]", allowed)
	}
	if operation.Path != "/status" {
		t.Fatalf("loaded contract was mutated through snapshot: %q", operation.Path)
	}

	operation, allowed, found = pipeline.FindAgentOperation("example", "POST", "/status")
	if !found || operation != nil || len(allowed) != 1 || allowed[0] != "GET" {
		t.Fatalf("method mismatch = operation=%#v allowed=%v found=%v", operation, allowed, found)
	}
}

func TestValidateManifestContract(t *testing.T) {
	valid := PluginManifest{
		SchemaVersion: 1,
		ID:            "local/example",
		Name:          "example",
		Version:       "0.1.0",
		ABIVersion:    "v1",
		FailureMode:   "pass",
		Hooks:         []Hook{{Name: "run_before_request"}},
		Permissions:   []Permission{{Name: "env.log"}},
	}
	if err := validateManifest(valid); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	for _, unsupported := range []string{"v2", "v3", ""} {
		invalid := valid
		invalid.ABIVersion = unsupported
		if err := validateManifest(invalid); err == nil {
			t.Fatalf("unsupported ABI %q was accepted", unsupported)
		}
	}
	invalid := valid
	invalid.Permissions = []Permission{{Name: "env.not_real"}}
	if err := validateManifest(invalid); err == nil {
		t.Fatal("unknown permission was accepted")
	}
	invalid = valid
	invalid.MinimumToranaVersion = "99.0.0"
	if err := validateManifest(invalid); err != nil {
		t.Fatalf("valid optional host version bound: %v", err)
	}
	if err := validateHostCompatibility(invalid, "dev"); err != nil {
		t.Fatalf("unversioned host must skip the optional bound: %v", err)
	}
	if err := validateHostCompatibility(invalid, "v0.0.0-20260808065220-fb5ce695e2d4"); err != nil {
		t.Fatalf("development pseudo-version must skip the optional bound: %v", err)
	}
	if err := validateHostCompatibility(invalid, "98.0.0"); err == nil {
		t.Fatal("known host below the minimum version was accepted")
	}
	if err := validateHostCompatibility(invalid, "99.0.0"); err != nil {
		t.Fatalf("known host at the minimum version was rejected: %v", err)
	}
	invalid.MaximumToranaVersion = "100.0.0"
	if err := validateHostCompatibility(invalid, "101.0.0"); err == nil {
		t.Fatal("known host above the maximum version was accepted")
	}
	invalid = valid
	invalid.MinimumToranaVersion = "not-semver"
	if err := validateManifest(invalid); err == nil {
		t.Fatal("malformed minimum_torana_version was accepted")
	}
	invalid = valid
	invalid.MinimumToranaVersion = "2.0.0"
	invalid.MaximumToranaVersion = "1.0.0"
	if err := validateManifest(invalid); err == nil {
		t.Fatal("inverted host version range was accepted")
	}
	invalid = valid
	invalid.RequiresUpstream = []string{valid.ID}
	if err := validateManifest(invalid); err == nil {
		t.Fatal("self dependency was accepted")
	}
	for _, tc := range []struct {
		name      string
		conflicts []string
		requires  []string
	}{
		{name: "empty", conflicts: []string{""}},
		{name: "whitespace", conflicts: []string{" \t"}},
		{name: "self", conflicts: []string{valid.ID}},
		{name: "duplicate", conflicts: []string{"local/other", "local/other"}},
		{name: "also required", conflicts: []string{"local/other"}, requires: []string{"local/other"}},
	} {
		t.Run("conflicts_with rejects "+tc.name, func(t *testing.T) {
			candidate := valid
			candidate.ConflictsWith = tc.conflicts
			candidate.RequiresUpstream = tc.requires
			if err := validateManifest(candidate); err == nil {
				t.Fatalf("invalid manifest accepted: %+v", candidate)
			}
		})
	}
	oneWay := valid
	oneWay.ConflictsWith = []string{"local/other"}
	if err := validateManifest(oneWay); err != nil {
		t.Fatalf("valid one-way conflict declaration: %v", err)
	}
}

func TestRequiresUpstreamRejectsMissingOrLaterDependencyBeforeLoadingCode(t *testing.T) {
	root := t.TempDir()
	writeBundle := func(dirName, id, name string, requires []string) {
		t.Helper()
		dir := filepath.Join(root, dirName)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := PluginManifest{
			SchemaVersion:    1,
			ID:               id,
			Name:             name,
			Version:          "0.1.0",
			ABIVersion:       "v1",
			FailureMode:      "pass",
			Repository:       "https://github.com/torana-edge/torana-plugins",
			RequiresUpstream: requires,
		}
		raw, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		// Deliberately not valid WASM: ordering must fail before code loading.
		if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), []byte("not wasm"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBundle("intent", "torana/intent", "intent", nil)
	writeBundle("compactor", "torana/compactor", "compactor", []string{"torana/intent"})

	bundles, err := DiscoverPlugins(root)
	if err != nil {
		t.Fatal(err)
	}
	approvals := make(map[string]Approval)
	for _, bundle := range bundles {
		approvals[bundle.Manifest.Name] = Approval{Digest: bundle.Digest}
	}
	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	_, err = NewPipeline(rt, PluginConfig{
		Dir:       root,
		Order:     []string{"compactor", "intent"},
		Approvals: approvals,
		Strict:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires approved plugin id") {
		t.Fatalf("dependency misordering error = %v", err)
	}

	_, err = NewPipeline(rt, PluginConfig{
		Dir:       root,
		Order:     []string{"intent", "compactor"},
		Approvals: approvals,
		Strict:    false,
	})
	if err == nil || !strings.Contains(err.Error(), "requires plugin id") ||
		!strings.Contains(err.Error(), "load successfully") {
		t.Fatalf("unavailable dependency error = %v", err)
	}
}

func TestExternalBundlesConformToCurrentHost(t *testing.T) {
	root := os.Getenv("TORANA_PLUGIN_BUNDLES_DIR")
	if root == "" {
		t.Skip("TORANA_PLUGIN_BUNDLES_DIR is unset")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read bundles: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			runtime := wasm.NewRuntime(context.Background())
			defer runtime.Close()
			pipeline, err := NewPipeline(runtime, PluginConfig{
				Dir:             root,
				Order:           []string{entry.Name()},
				AllowUnapproved: true,
				Strict:          true,
			})
			if err != nil {
				t.Fatalf("host conformance: %v", err)
			}
			if pipeline.Len() != 1 {
				t.Fatalf("loaded plugins = %d, want 1", pipeline.Len())
			}
		})
	}
}

func isDraining(pp *PluginPipeline) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return pp.draining
}

func TestDiscoverPlugins_ValidPlugin(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "test-plugin")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := PluginManifest{
		Name:        "test-plugin",
		Version:     "0.1.0",
		Description: "test",
		Hooks: []Hook{
			{Name: "run_before_request"},
		},
	}
	mBytes, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), mBytes, 0644); err != nil {
		t.Fatal(err)
	}
	// Write a minimal valid WASM file (magic bytes + version).
	// This won't execute, but DiscoverPlugins only reads the file — it doesn't instantiate.
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.wasm"), wasmBytes, 0644); err != nil {
		t.Fatal(err)
	}
	bundles, err := DiscoverPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 {
		t.Fatalf("expected 1 bundle, got %d", len(bundles))
	}
	if bundles[0].Manifest.Name != "test-plugin" {
		t.Errorf("unexpected plugin name: %s", bundles[0].Manifest.Name)
	}
}

func TestDiscoverPluginsRejectsDuplicateIdentity(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"first", "second"} {
		pluginDir := filepath.Join(root, directory)
		if err := os.Mkdir(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := PluginManifest{ID: "community/example", Name: "example"}
		manifestBytes, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), manifestBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.wasm"), []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := DiscoverPlugins(root); err == nil {
		t.Fatal("duplicate plugin name and ID were accepted")
	}
}

// TestPipelineRunBeforeRequest exercises the full dispatch path (manifest → discovery →
// pipeline → hook call) using a real compiled WASM plugin, rather than calling
// CallRequest directly. This catches manifest/dispatch mismatches that the
// existing direct-call tests miss.

func TestPipelineRunBeforeRequest_FullDispatch(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")

	ctx := context.Background()
	runtime := wasm.NewRuntime(ctx)
	defer runtime.Close()

	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir:             fixturesDir,
		Order:           []string{"test-mutator"},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pipeline.Len() != 1 {
		t.Fatalf("fixture not loaded (loaded=%d)", pipeline.Len())
	}

	chat := &engine.ChatRequest{
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
		Tools: []engine.ToolDef{{
			Name:       "read",
			Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
	result, err := pipeline.RunBeforeRequest(ctx, 1, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// The fixture mutates both a message and a tool definition, so a single
	// dispatch proves the host carried the whole request across the boundary
	// and merged the plugin's version back — not merely that a hook ran.
	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}
	if got := result.Tools[0].Description; got != "described by test-mutator" {
		t.Errorf("tool definition did not survive the dispatch path: description = %q", got)
	}
	if len(result.Messages) != 1 || !strings.HasSuffix(engineText(result.Messages[0]), "[seen by test-mutator]") {
		t.Errorf("message did not survive the dispatch path: %+v", result.Messages)
	}
}

func TestRunBeforeRequestTrackedReportsOnlyAcceptedReplacement(t *testing.T) {
	t.Run("empty pipeline", func(t *testing.T) {
		pipeline := newTestPipeline(t, fixturesDir, nil)
		in := &engine.ChatRequest{
			Model:    "m",
			Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
		}
		out, changed, err := pipeline.RunBeforeRequestTracked(context.Background(), 1, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatal("empty pipeline reported a request replacement")
		}
		if out != in {
			t.Fatal("unchanged path did not preserve the accepted engine request")
		}
	})

	t.Run("real replacement", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
		pipeline := newTestPipeline(t, fixturesDir, []string{"test-mutator"})
		in := &engine.ChatRequest{
			Model:    "m",
			Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
			Tools: []engine.ToolDef{{
				Name:       "read",
				Parameters: mustReq(`{"type":"object"}`),
			}},
		}
		out, changed, err := pipeline.RunBeforeRequestTracked(context.Background(), 2, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("accepted plugin replacement was not reported")
		}
		if out == in || out.Tools[0].Description != "described by test-mutator" {
			t.Fatalf("replacement was not applied: %+v", out)
		}
	})
}

// mustReq panics on invalid raw: test fixtures are trusted, and a fixture
// that no longer parses must fail loudly.
func engineText(m engine.Message) string {
	var out string
	for _, b := range m.Blocks {
		if b.Text != nil {
			out += b.Text.Text
		}
	}
	return out
}

func mustReq(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}
