package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/wasm"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/proto"
)

// ============================================================================
// Manifest
// ============================================================================

type Permission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Hook struct {
	Name string `json:"name"`
}

type PluginManifest struct {
	SchemaVersion        int          `json:"schema_version,omitempty"`
	ID                   string       `json:"id,omitempty"`
	Name                 string       `json:"name"`
	Version              string       `json:"version"`
	ABIVersion           string       `json:"abi_version,omitempty"`
	MinimumToranaVersion string       `json:"minimum_torana_version,omitempty"`
	MaximumToranaVersion string       `json:"maximum_torana_version,omitempty"`
	FailureMode          string       `json:"failure_mode,omitempty"`
	Repository           string       `json:"repository,omitempty"`
	Description          string       `json:"description"`
	Hooks                []Hook       `json:"hooks"`
	Permissions          []Permission `json:"permissions"`
	// RequiresUpstream lists stable plugin IDs that must be approved and
	// loaded earlier in the operator's configured order.
	RequiresUpstream []string `json:"requires_upstream,omitempty"`
}

type ConfigField struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"` // "string" | "number" | "boolean" | "enum"
	Label   string   `json:"label"`
	Default any      `json:"default,omitempty"`
	Options []string `json:"options,omitempty"` // enum only
	Help    string   `json:"help,omitempty"`
	// Source names a live host resource whose current values the control plane
	// offers as a picker beside the input, e.g. "conversations". It is advisory
	// UI metadata: the value is still an ordinary string, an unknown source
	// simply renders no picker, and nothing here constrains what may be saved.
	//
	// Generic on purpose. The control plane resolves the name against its own
	// table of sources, so no plugin is ever named in the rendering logic.
	Source string `json:"source,omitempty"`
}

type ConfigSchema struct {
	Fields []ConfigField `json:"fields"`
	// Raw is the complete JSON Schema document used for validation. Fields is
	// only the scalar projection rendered by the control plane.
	Raw json.RawMessage `json:"-"`
	// compiledSchema is prepared once while loading the bundle.
	compiledSchema any
}

// AgentOperation describes one stable JSON operation a plugin contributes to
// Torana's agent-facing control plane. The descriptor is language-neutral and
// is loaded from agent.json beside the plugin bundle.
type AgentOperation struct {
	ID           string          `json:"id"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Description  string          `json:"description"`
	Risk         string          `json:"risk"` // read | write | destructive
	Idempotent   bool            `json:"idempotent"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

type AgentDescriptor struct {
	SchemaVersion int              `json:"schema_version"`
	Description   string           `json:"description,omitempty"`
	Operations    []AgentOperation `json:"operations"`
}

// ============================================================================
// Discovery
// ============================================================================

type PluginBundle struct {
	Manifest  PluginManifest
	WASMBytes []byte
	Digest    string
	Schema    *ConfigSchema
	Agent     *AgentDescriptor
}

// supportedHooks and supportedPermissions are derived from the SDK's published
// v1 vocabulary rather than restated here. Which capability strings exist is an
// ABI concern, and a second copy is how the official plugin repository's
// validator ended up rejecting capabilities this host accepts.
//
// A host may expose fewer than the ABI defines; it must not invent names
// outside it.
var supportedHooks = setOf(sdk.Hooks)

var supportedPermissions = setOf(sdk.Permissions)

func setOf(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

var agentOperationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var agentOperationPathPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9._~-]+/?)*$`)

func validateAgentDescriptor(descriptor AgentDescriptor, manifest PluginManifest) error {
	if !agentOperationIDPattern.MatchString(manifest.Name) {
		return fmt.Errorf("agent descriptor: plugin name %q is not a safe path segment", manifest.Name)
	}
	if descriptor.SchemaVersion != 1 {
		return fmt.Errorf("agent descriptor: unsupported schema_version %d", descriptor.SchemaVersion)
	}
	if len(descriptor.Operations) == 0 || len(descriptor.Operations) > 64 {
		return fmt.Errorf("agent descriptor: operations must contain 1 to 64 entries")
	}
	if !hasHook(manifest, "run_on_http_request") {
		return fmt.Errorf("agent descriptor: run_on_http_request hook is required")
	}
	hasServeHTTP := false
	for _, permission := range manifest.Permissions {
		if permission.Name == "env.serve_http" {
			hasServeHTTP = true
			break
		}
	}
	if !hasServeHTTP {
		return fmt.Errorf("agent descriptor: env.serve_http permission is required")
	}

	seenIDs := make(map[string]struct{}, len(descriptor.Operations))
	seenRoutes := make(map[string]struct{}, len(descriptor.Operations))
	for index := range descriptor.Operations {
		operation := &descriptor.Operations[index]
		operation.Method = strings.ToUpper(strings.TrimSpace(operation.Method))
		if !agentOperationIDPattern.MatchString(operation.ID) {
			return fmt.Errorf("agent descriptor: invalid operation id %q", operation.ID)
		}
		if _, duplicate := seenIDs[operation.ID]; duplicate {
			return fmt.Errorf("agent descriptor: duplicate operation id %q", operation.ID)
		}
		seenIDs[operation.ID] = struct{}{}
		switch operation.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return fmt.Errorf("agent descriptor: operation %q has unsupported method %q", operation.ID, operation.Method)
		}
		if operation.Path == "" || !strings.HasPrefix(operation.Path, "/") ||
			!agentOperationPathPattern.MatchString(operation.Path) ||
			strings.HasSuffix(operation.Path, "/.") || strings.Contains(operation.Path, "/./") ||
			strings.HasPrefix(operation.Path, "//") || strings.Contains(operation.Path, "..") ||
			strings.ContainsAny(operation.Path, "?#") {
			return fmt.Errorf("agent descriptor: operation %q has invalid path %q", operation.ID, operation.Path)
		}
		routeKey := operation.Method + " " + operation.Path
		if _, duplicate := seenRoutes[routeKey]; duplicate {
			return fmt.Errorf("agent descriptor: duplicate route %s", routeKey)
		}
		seenRoutes[routeKey] = struct{}{}
		if strings.TrimSpace(operation.Description) == "" {
			return fmt.Errorf("agent descriptor: operation %q requires a description", operation.ID)
		}
		switch operation.Risk {
		case "read":
			if operation.Method != http.MethodGet {
				return fmt.Errorf("agent descriptor: operation %q marks a mutating method as read risk", operation.ID)
			}
		case "write":
			if operation.Method == http.MethodGet || operation.Method == http.MethodDelete {
				return fmt.Errorf("agent descriptor: operation %q has inconsistent write risk", operation.ID)
			}
		case "destructive":
			if operation.Method == http.MethodGet {
				return fmt.Errorf("agent descriptor: operation %q marks GET as destructive", operation.ID)
			}
		default:
			return fmt.Errorf("agent descriptor: operation %q has invalid risk %q", operation.ID, operation.Risk)
		}
		if err := validateJSONSchemaObject(operation.InputSchema, true); err != nil {
			return fmt.Errorf("agent descriptor: operation %q input_schema: %w", operation.ID, err)
		}
		if err := validateJSONSchemaObject(operation.OutputSchema, false); err != nil {
			return fmt.Errorf("agent descriptor: operation %q output_schema: %w", operation.ID, err)
		}
	}
	return nil
}

func validateJSONSchemaObject(raw json.RawMessage, optional bool) error {
	if len(raw) == 0 {
		if optional {
			return nil
		}
		return fmt.Errorf("is required")
	}
	return ValidateAgentSchema(raw)
}

func validateManifest(manifest PluginManifest) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", manifest.SchemaVersion)
	}
	if manifest.ID == "" || manifest.Name == "" {
		return fmt.Errorf("id and name are required")
	}
	// This host dispatches v2 exclusively: one run_hook export, HookInput in,
	// HookResult out. A v1 guest exports per-hook functions this host never
	// calls, so accepting its manifest would load a plugin that can never run
	// — the failure would surface as "my plugin does nothing" rather than as a
	// version mismatch anyone can act on.
	if manifest.ABIVersion != "v2" {
		return fmt.Errorf("unsupported abi_version %q: this host speaks ABI v2 "+
			"(single run_hook export); rebuild the plugin against a v2 SDK",
			manifest.ABIVersion)
	}
	if _, err := parseSemver(manifest.Version); err != nil {
		return fmt.Errorf("invalid version: %w", err)
	}
	if manifest.MinimumToranaVersion != "" {
		if _, err := parseSemver(manifest.MinimumToranaVersion); err != nil {
			return fmt.Errorf("invalid minimum_torana_version: %w", err)
		}
	}
	if manifest.MaximumToranaVersion != "" {
		if _, err := parseSemver(manifest.MaximumToranaVersion); err != nil {
			return fmt.Errorf("invalid maximum_torana_version: %w", err)
		}
	}
	if manifest.MinimumToranaVersion != "" && manifest.MaximumToranaVersion != "" &&
		compareSemver(manifest.MinimumToranaVersion, manifest.MaximumToranaVersion) > 0 {
		return fmt.Errorf("minimum_torana_version must not exceed maximum_torana_version")
	}
	if manifest.FailureMode != "pass" && manifest.FailureMode != "block" {
		return fmt.Errorf("failure_mode must be pass or block")
	}
	if strings.HasPrefix(manifest.ID, "torana/") && manifest.Repository == "" {
		return fmt.Errorf("official plugin repository is required")
	}
	seenHooks := make(map[string]struct{}, len(manifest.Hooks))
	for _, hook := range manifest.Hooks {
		if _, ok := supportedHooks[hook.Name]; !ok {
			return fmt.Errorf("unsupported hook %q", hook.Name)
		}
		if _, duplicate := seenHooks[hook.Name]; duplicate {
			return fmt.Errorf("duplicate hook %q", hook.Name)
		}
		seenHooks[hook.Name] = struct{}{}
	}
	seenPermissions := make(map[string]struct{}, len(manifest.Permissions))
	for _, permission := range manifest.Permissions {
		if _, ok := supportedPermissions[permission.Name]; !ok {
			return fmt.Errorf("unsupported permission %q", permission.Name)
		}
		if _, duplicate := seenPermissions[permission.Name]; duplicate {
			return fmt.Errorf("duplicate permission %q", permission.Name)
		}
		seenPermissions[permission.Name] = struct{}{}
	}
	seenRequirements := make(map[string]struct{}, len(manifest.RequiresUpstream))
	for _, requiredID := range manifest.RequiresUpstream {
		if strings.TrimSpace(requiredID) == "" {
			return fmt.Errorf("requires_upstream entries must be non-empty plugin ids")
		}
		if requiredID == manifest.ID {
			return fmt.Errorf("requires_upstream cannot reference the plugin itself")
		}
		if _, duplicate := seenRequirements[requiredID]; duplicate {
			return fmt.Errorf("duplicate requires_upstream plugin id %q", requiredID)
		}
		seenRequirements[requiredID] = struct{}{}
	}
	return nil
}

func canonicalSemver(raw string) string {
	if strings.HasPrefix(raw, "v") {
		return raw
	}
	return "v" + raw
}

func parseSemver(raw string) (string, error) {
	version := canonicalSemver(raw)
	if !semver.IsValid(version) {
		return "", fmt.Errorf("%q is not a valid semantic version", raw)
	}
	return version, nil
}

func compareSemver(left, right string) int {
	return semver.Compare(canonicalSemver(left), canonicalSemver(right))
}

// validateHostCompatibility applies optional product-version bounds only when
// the host was built from a release tag. Development builds identify
// themselves with "dev", a commit SHA, or a Go pseudo-version, so compatibility
// remains governed by abi_version, hooks, and permissions until Edge has a
// release.
func validateHostCompatibility(manifest PluginManifest, hostVersion string) error {
	rawHostVersion := hostVersion
	if module.IsPseudoVersion(canonicalSemver(rawHostVersion)) {
		if manifest.MinimumToranaVersion != "" || manifest.MaximumToranaVersion != "" {
			log.Printf("[plugin] %s: host version %q is a development pseudo-version; skipping minimum_torana_version/maximum_torana_version checks", manifest.Name, rawHostVersion)
		}
		return nil
	}
	hostVersion, err := parseSemver(rawHostVersion)
	if err != nil {
		if manifest.MinimumToranaVersion != "" || manifest.MaximumToranaVersion != "" {
			log.Printf("[plugin] %s: host version %q is not semantic; skipping minimum_torana_version/maximum_torana_version checks", manifest.Name, rawHostVersion)
		}
		return nil
	}
	if manifest.MinimumToranaVersion != "" &&
		semver.Compare(hostVersion, canonicalSemver(manifest.MinimumToranaVersion)) < 0 {
		return fmt.Errorf("requires Torana Edge >= %s (running %s)", manifest.MinimumToranaVersion, hostVersion)
	}
	if manifest.MaximumToranaVersion != "" &&
		semver.Compare(hostVersion, canonicalSemver(manifest.MaximumToranaVersion)) > 0 {
		return fmt.Errorf("requires Torana Edge <= %s (running %s)", manifest.MaximumToranaVersion, hostVersion)
	}
	return nil
}

func DiscoverPlugins(pluginsDir string) ([]PluginBundle, error) {
	if pluginsDir == "" {
		pluginsDir = "./plugins"
	}
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var bundles []PluginBundle
	seenNames := make(map[string]string)
	seenIDs := make(map[string]string)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pluginDir := filepath.Join(pluginsDir, e.Name())
		bundle, err := loadBundle(pluginDir)
		if err != nil {
			log.Printf("[plugin] skipping %s: %v", e.Name(), err)
			continue
		}
		if firstDir, duplicate := seenNames[bundle.Manifest.Name]; duplicate {
			return nil, fmt.Errorf("duplicate plugin name %q in %s and %s", bundle.Manifest.Name, firstDir, e.Name())
		}
		if bundle.Manifest.ID != "" {
			if firstDir, duplicate := seenIDs[bundle.Manifest.ID]; duplicate {
				return nil, fmt.Errorf("duplicate plugin id %q in %s and %s", bundle.Manifest.ID, firstDir, e.Name())
			}
			seenIDs[bundle.Manifest.ID] = e.Name()
		}
		seenNames[bundle.Manifest.Name] = e.Name()
		bundles = append(bundles, *bundle)
	}
	return bundles, nil
}

func loadBundle(dir string) (*PluginBundle, error) {
	manifestPath := filepath.Join(dir, "plugin.json")
	mBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest PluginManifest
	if err := json.Unmarshal(mBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	wasmPath := filepath.Join(dir, "plugin.wasm")
	wBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm: %w", err)
	}
	schemaPath := filepath.Join(dir, "schema.json")
	var schema *ConfigSchema
	var schemaBytes []byte
	if sBytes, err := os.ReadFile(schemaPath); err == nil {
		schemaBytes = sBytes
		var s ConfigSchema
		if err := json.Unmarshal(sBytes, &s); err != nil {
			return nil, fmt.Errorf("parse schema: %w", err)
		}
		// A JSON Schema document unmarshals into ConfigSchema without error and
		// yields no fields, which used to mean the control plane silently
		// rendered no form. Derive the fields instead — see
		// jsonschema_config.go.
		if len(s.Fields) == 0 {
			if derived := deriveConfigSchema(sBytes); derived != nil {
				s = *derived
			}
		}
		if isJSONSchema(sBytes) {
			s.Raw = append(json.RawMessage(nil), sBytes...)
			if err := prepareConfigSchema(&s); err != nil {
				return nil, fmt.Errorf("compile schema: %w", err)
			}
		}
		schema = &s
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	agentPath := filepath.Join(dir, "agent.json")
	var agent *AgentDescriptor
	var agentBytes []byte
	if aBytes, err := os.ReadFile(agentPath); err == nil {
		agentBytes = aBytes
		var descriptor AgentDescriptor
		if err := json.Unmarshal(aBytes, &descriptor); err != nil {
			return nil, fmt.Errorf("parse agent descriptor: %w", err)
		}
		if err := validateAgentDescriptor(descriptor, manifest); err != nil {
			return nil, err
		}
		agent = &descriptor
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read agent descriptor: %w", err)
	}
	warnIfStale(dir, wasmPath, manifest.Name)
	digest := bundleDigest(mBytes, wBytes, schemaBytes, agentBytes)
	return &PluginBundle{
		Manifest:  manifest,
		WASMBytes: wBytes,
		Digest:    digest,
		Schema:    schema,
		Agent:     agent,
	}, nil
}

// ValidateBundleDir loads and validates one complete bundle exactly as the
// runtime will before an installer activates it.
func ValidateBundleDir(dir string) (*PluginBundle, error) {
	bundle, err := loadBundle(dir)
	if err != nil {
		return nil, err
	}
	if err := validateManifest(bundle.Manifest); err != nil {
		return nil, err
	}
	return bundle, nil
}

// ValidateManifestDir checks plugin.json alone, without requiring a compiled
// plugin.wasm. `torana plugin lint` uses it so an author can find a manifest
// mistake before paying for a WASM build — ValidateBundleDir needs the binary,
// which means the first manifest check would otherwise happen at install.
func ValidateManifestDir(dir string) (PluginManifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return PluginManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest PluginManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return PluginManifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return manifest, validateManifest(manifest)
}

// bundleDigest covers every executable or policy-bearing file consumed by the
// runtime. Length-prefixing keeps the digest unambiguous. A change to code,
// requested permissions, failure behavior, hooks, configuration schema, or
// advertised agent contract therefore invalidates the operator's approval.
// BundleDigestForDir computes the approval digest for an on-disk bundle. It is
// the single source of truth shared with `torana plugin install`, which must
// print exactly the digest an operator will later approve. Reimplementing this
// anywhere else reintroduces a drift bug that is silent by construction — the
// two values simply never match and nothing errors.
func BundleDigestForDir(dir string) (string, error) {
	read := func(name string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return data, err
	}
	manifestBytes, err := read("plugin.json")
	if err != nil {
		return "", err
	}
	wasmBytes, err := read("plugin.wasm")
	if err != nil {
		return "", err
	}
	schemaBytes, err := read("schema.json")
	if err != nil {
		return "", err
	}
	agentBytes, err := read("agent.json")
	if err != nil {
		return "", err
	}
	return bundleDigest(manifestBytes, wasmBytes, schemaBytes, agentBytes), nil
}

func bundleDigest(manifestBytes, wasmBytes, schemaBytes, agentBytes []byte) string {
	h := sha256.New()
	parts := [][]byte{manifestBytes, wasmBytes, schemaBytes}
	// Preserve existing approvals for bundles that do not ship agent.json.
	// Adding the optional file appends a fourth length-delimited digest part.
	if agentBytes != nil {
		parts = append(parts, agentBytes)
	}
	for _, part := range parts {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(part)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

// warnIfStale logs a warning when plugin.wasm is older than any Go source
// in the plugin directory. Stale binaries silently running outdated logic
// caused a production incident — binaries are build artifacts, installed with
// `torana plugin install` (fixtures: `make testdata`).
func warnIfStale(dir, wasmPath, name string) {
	wasmInfo, err := os.Stat(wasmPath)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(wasmInfo.ModTime()) {
			// Operator-facing, so it has to name a command that exists.
			// Plugins are no longer built from a tree in this repository, and
			// the previous message pointed at a Makefile target deleted along
			// with that tree.
			log.Printf("[plugin] %s: plugin.wasm is older than %s — reinstall with 'torana plugin install %s'", name, e.Name(), name)
			return
		}
	}
}

// ============================================================================
// Pipeline
// ============================================================================

type PluginPipeline struct {
	plugins []*loadedPlugin
	runtime *wasm.Runtime

	// skipped records plugins named in config.Order that were not loaded
	// because the operator has not approved their current bundle digest.
	// A missing approval is operator state, not a malformed configuration:
	// the plugin is skipped (its code never runs) and the condition is
	// reported, rather than failing the whole reload. Every other Strict
	// failure — invalid manifest, duplicate order, ordering violation,
	// failed load — remains a hard error.
	skipped []SkippedPlugin

	// candidateValidator is the CONSTRUCTION-BOUND per-plugin candidate
	// validator (see PluginConfig.CandidateValidator): immutable after
	// NewPipeline, so requests can never race a mutation.
	candidateValidator func(topo engine.TopologyFacts, current, replacement *pbv2.ChatRequest) error

	mu        sync.Mutex
	active    int
	draining  bool
	drained   chan struct{}
	closed    chan struct{}
	drainOnce sync.Once

	// streamKinds tracks, per request, which content block is ACTUALLY open at
	// each index across RunOnStreamChunk calls, so a plugin-passed
	// ContentBlockStop converts back to the engine event matching the block
	// it closes (ToolCallEnd for tool blocks, BlockStop for text/thinking/,
	// provider) and unknown/mismatched/duplicate/reused topology errors
	// instead of being guessed. The v2 wire's stop carries only an index;
	// without this, every plugin-passed text/thinking stop would come back
	// as ToolCallEnd and the lossless block topology would not survive
	// plugins. Entries live for the request and are dropped by EndRequest.
	streamKinds map[uint64]*pbconv.BlockKindTracker

	// streamVerify holds the per-request stream-signature enforcement state
	// (accepted/returned buffers, per-plugin discipline walkers, terminal
	// flag) for requests processed through RunOnStreamChunkVerified. Entries
	// live for the request and are dropped by EndRequest, in the same place
	// as streamKinds, so the enforcement state cannot outlive the request
	// metadata it describes.
	streamVerify map[uint64]*streamVerifierState

	// requestPlugins records the first-fire order of plugins for each pinned
	// request. It is request-scoped operator telemetry, not execution state;
	// EndRequest deletes it with the other request-owned maps.
	requestPlugins map[uint64]*requestPluginInvocations
}

type requestPluginInvocations struct {
	names []string
	seen  map[string]struct{}
}

// SkippedPlugin describes an enabled plugin that was not loaded because it
// lacks an operator approval bound to its current bundle digest.
type SkippedPlugin struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Reason string `json:"reason"`
}

// Skipped returns the plugins that were enabled in configuration but not
// loaded for want of a digest-bound approval. Callers surface this so an
// operator can see what needs approving instead of silently running a
// shorter pipeline than they configured.
func (p *PluginPipeline) Skipped() []SkippedPlugin {
	if p == nil {
		return nil
	}
	return p.skipped
}

type loadedPlugin struct {
	manifest    PluginManifest
	digest      string
	agent       *AgentDescriptor
	plugin      *wasm.Plugin
	failureMode string
}

// LoadedAgentPlugin is the immutable, approved agent contract attached to the
// exact plugin bundle currently loaded in a pipeline.
type LoadedAgentPlugin struct {
	Manifest   PluginManifest
	Digest     string
	Descriptor AgentDescriptor
}

// LoadedPluginStatus describes the exact bundle currently serving traffic.
type LoadedPluginStatus struct {
	Name       string
	Digest     string
	ServesHTTP bool
	Agent      *AgentDescriptor
}

func NewPipeline(runtime *wasm.Runtime, config PluginConfig) (*PluginPipeline, error) {
	return reloadPipeline(runtime, config)
}

func reloadPipeline(runtime *wasm.Runtime, config PluginConfig) (*PluginPipeline, error) {
	bundles, err := DiscoverPlugins(config.Dir)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]PluginBundle)
	for _, b := range bundles {
		byName[b.Manifest.Name] = b
	}
	var loaded []*loadedPlugin
	var skipped []SkippedPlugin
	order := config.Order
	seenOrder := make(map[string]struct{}, len(order))
	for _, name := range order {
		if _, duplicate := seenOrder[name]; duplicate {
			return nil, fmt.Errorf("plugin order contains duplicate %q", name)
		}
		seenOrder[name] = struct{}{}
	}
	// Enforce ordering constraint: route-capable plugins (env.route_request)
	// must precede compaction economic-gate plugins (env.host_call.torana_evaluate_compaction).
	var seenCompactionGate bool
	approvedUpstream := make(map[string]struct{})
	loadedUpstream := make(map[string]struct{})
	for _, name := range order {
		bundle, ok := byName[name]
		if !ok {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q is missing or malformed", name)
			}
			continue
		}
		if !config.AllowUnapproved {
			if err := validateManifest(bundle.Manifest); err != nil {
				return nil, fmt.Errorf("enabled plugin %q has invalid manifest: %w", name, err)
			}
			if err := validateHostCompatibility(bundle.Manifest, config.HostVersion); err != nil {
				return nil, fmt.Errorf("enabled plugin %q is incompatible: %w", name, err)
			}
		}
		approval, approved := config.approvalFor(bundle)
		if !approved {
			// Not a configuration error — see PluginPipeline.skipped.
			continue
		}
		grants, _, err := validateApproval(bundle, approval)
		if err != nil {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q has invalid approval: %w", name, err)
			}
			continue
		}
		if !config.AllowUnapproved {
			for _, requiredID := range bundle.Manifest.RequiresUpstream {
				if _, ok := approvedUpstream[requiredID]; !ok {
					return nil, fmt.Errorf("ordering constraint violation: plugin %q requires approved plugin id %q earlier in plugins.order", name, requiredID)
				}
			}
		}
		var hasRoute, hasCompactionGate bool
		for _, grant := range grants {
			if grant == "env.route_request" {
				hasRoute = true
			}
			if grant == "env.host_call.torana_evaluate_compaction" {
				hasCompactionGate = true
			}
		}
		if hasRoute && seenCompactionGate {
			return nil, fmt.Errorf("ordering constraint violation: route-capable plugin %q (grant env.route_request) must precede compaction economic-gate plugins (grant env.host_call.torana_evaluate_compaction)", name)
		}
		if hasCompactionGate {
			seenCompactionGate = true
		}
		if !config.AllowUnapproved {
			approvedUpstream[bundle.Manifest.ID] = struct{}{}
		}
	}
	for _, name := range order {
		bundle, ok := byName[name]
		if !ok {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q is missing or malformed", name)
			}
			log.Printf("[plugin] %s not found in plugins dir, skipping", name)
			continue
		}
		approval, approved := config.approvalFor(bundle)
		if !approved {
			skipped = append(skipped, SkippedPlugin{
				Name:   name,
				Digest: bundle.Digest,
				Reason: "no operator approval for this bundle digest",
			})
			log.Printf("[plugin] %s: not loaded — no operator approval for digest %s. "+
				"Open %s in the control plane at /_torana/, review its requested "+
				"capabilities, and approve this digest to enable it.", name, bundle.Digest, name)
			continue
		}
		if !config.AllowUnapproved {
			for _, requiredID := range bundle.Manifest.RequiresUpstream {
				if _, ok := loadedUpstream[requiredID]; !ok {
					return nil, fmt.Errorf("dependency unavailable: plugin %q requires plugin id %q to load successfully earlier in plugins.order", name, requiredID)
				}
			}
		}
		grants, failureMode, err := validateApproval(bundle, approval)
		if err != nil {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q has invalid approval: %w", name, err)
			}
			log.Printf("[plugin] %s: invalid approval: %v — skipping", name, err)
			continue
		}
		pl, err := runtime.LoadPlugin(name, bundle.WASMBytes)
		if err != nil {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q failed to load: %w", name, err)
			}
			log.Printf("[plugin] %s: %v — skipping", name, err)
			continue
		}
		pl.SetGrants(grants)
		if raw, ok := config.Config[name]; ok && len(raw) > 0 {
			pl.SetConfig(string(raw))
		}
		// Validate that every declared hook is actually exported by the WASM module.
		// A post-load rejection must UNLOAD the plugin: it was already published
		// into the runtime, and retaining it would keep its instance and compiled
		// handle (a shared-cache reference) alive for the pipeline's lifetime.
		// Unload retains reachability until quiescence, removes it, and then
		// releases everything exactly once; strict whole-pipeline failure is
		// cleaned when the caller closes the runtime.
		declaredHooks, hookErr := manifestHooks(bundle.Manifest.Hooks)
		if hookErr != nil {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q: %w", name, hookErr)
			}
			if unloadErr := runtime.UnloadPlugin(name); unloadErr != nil {
				log.Printf("[plugin] %s: unload after rejection: %v", name, unloadErr)
			}
			log.Printf("[plugin] %s: %v — skipping", name, hookErr)
			continue
		}
		if err := pl.ValidateHooks(context.Background(), declaredHooks); err != nil {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q failed hook validation: %w", name, err)
			}
			if unloadErr := runtime.UnloadPlugin(name); unloadErr != nil {
				log.Printf("[plugin] %s: unload after rejection: %v", name, unloadErr)
			}
			log.Printf("[plugin] %s: hook validation failed: %v — skipping", name, err)
			continue
		}
		var loadedAgent *AgentDescriptor
		if bundle.Agent != nil {
			for _, grant := range grants {
				if grant == "env.serve_http" {
					loadedAgent = cloneAgentDescriptor(bundle.Agent)
					break
				}
			}
		}
		loaded = append(loaded, &loadedPlugin{
			manifest:    bundle.Manifest,
			digest:      bundle.Digest,
			agent:       loadedAgent,
			plugin:      pl,
			failureMode: failureMode,
		})
		if !config.AllowUnapproved {
			loadedUpstream[bundle.Manifest.ID] = struct{}{}
		}
		log.Printf("[plugin] %s ready — hooks: %v", name, hookNames(bundle.Manifest.Hooks))
		// run_after_response mutations are applied on the non-streaming JSON
		// path but are OBSERVATIONAL on streaming responses (the stream is
		// already written when the hook fires) — see docs/PLUGIN_IMPLEMENTATION_
		// GUIDE.md §5. Warn once at load so a plugin that expects to rewrite
		// streamed responses isn't silently a no-op.
		if hasHook(bundle.Manifest, "run_after_response") {
			log.Printf("[plugin] %s: run_after_response mutations are observational on streaming responses (metrics/audit OK; response rewrites are dropped mid-stream)", name)
		}
	}
	return &PluginPipeline{
		plugins:            loaded,
		runtime:            runtime,
		skipped:            skipped,
		drained:            make(chan struct{}),
		closed:             make(chan struct{}),
		streamKinds:        make(map[uint64]*pbconv.BlockKindTracker),
		streamVerify:       make(map[uint64]*streamVerifierState),
		requestPlugins:     make(map[uint64]*requestPluginInvocations),
		candidateValidator: config.CandidateValidator,
	}, nil
}

// TryAcquire pins a pipeline for a new HTTP request. Once draining begins it
// rejects new work, preventing a request that observed the old atomic pointer
// from acquiring a runtime after that runtime has been closed.
func (pp *PluginPipeline) TryAcquire() bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.draining {
		return false
	}
	pp.active++
	return true
}

// Acquire/Release protect individual hook calls. A request that already owns
// a pipeline may make nested hook calls while it is draining, so Acquire is
// deliberately not gated by draining; new request admission uses TryAcquire.
func (pp *PluginPipeline) Acquire() {
	pp.mu.Lock()
	pp.active++
	pp.mu.Unlock()
}

func (pp *PluginPipeline) Release() {
	pp.mu.Lock()
	pp.active--
	if pp.active == 0 && pp.draining {
		close(pp.drained)
	}
	pp.mu.Unlock()
}

// Len returns the number of successfully loaded plugins.
func (pp *PluginPipeline) Len() int { return len(pp.plugins) }

// AgentPlugins returns the agent contracts for the exact approved bundles in
// this pipeline. Reading contracts from the live pipeline prevents a changed,
// unapproved agent.json on disk from being advertised or routed to old code.
func (pp *PluginPipeline) AgentPlugins() []LoadedAgentPlugin {
	plugins := make([]LoadedAgentPlugin, 0, len(pp.plugins))
	for _, lp := range pp.plugins {
		if lp.agent == nil {
			continue
		}
		plugins = append(plugins, LoadedAgentPlugin{
			Manifest:   lp.manifest,
			Digest:     lp.digest,
			Descriptor: *cloneAgentDescriptor(lp.agent),
		})
	}
	return plugins
}

// LoadedPlugins returns immutable status for the exact bundles in this
// pipeline, allowing operator surfaces to distinguish configured disk state
// from the code that remains live after a rejected hot reload.
func (pp *PluginPipeline) LoadedPlugins() []LoadedPluginStatus {
	plugins := make([]LoadedPluginStatus, 0, len(pp.plugins))
	for _, lp := range pp.plugins {
		plugins = append(plugins, LoadedPluginStatus{
			Name:       lp.manifest.Name,
			Digest:     lp.digest,
			ServesHTTP: hasHook(lp.manifest, "run_on_http_request") && lp.plugin.HasGrant("env.serve_http"),
			Agent:      cloneAgentDescriptor(lp.agent),
		})
	}
	return plugins
}

// FindAgentOperation resolves an operation only from the exact approved
// contract currently loaded alongside the plugin code.
func (pp *PluginPipeline) FindAgentOperation(pluginName, method, path string) (*AgentOperation, []string, bool) {
	for _, lp := range pp.plugins {
		if lp.manifest.Name != pluginName || lp.agent == nil {
			continue
		}
		var allowed []string
		for index := range lp.agent.Operations {
			operation := &lp.agent.Operations[index]
			if operation.Path != path {
				continue
			}
			allowed = append(allowed, operation.Method)
			if operation.Method == method {
				operationCopy := cloneAgentOperation(*operation)
				return &operationCopy, allowed, true
			}
		}
		return nil, allowed, true
	}
	return nil, nil, false
}

func cloneAgentDescriptor(descriptor *AgentDescriptor) *AgentDescriptor {
	if descriptor == nil {
		return nil
	}
	descriptorCopy := *descriptor
	descriptorCopy.Operations = make([]AgentOperation, len(descriptor.Operations))
	for index, operation := range descriptor.Operations {
		descriptorCopy.Operations[index] = cloneAgentOperation(operation)
	}
	return &descriptorCopy
}

func cloneAgentOperation(operation AgentOperation) AgentOperation {
	operation.InputSchema = append(json.RawMessage(nil), operation.InputSchema...)
	operation.OutputSchema = append(json.RawMessage(nil), operation.OutputSchema...)
	return operation
}

// EndRequest drops all request-scoped plugin state for a finished request.
func (pp *PluginPipeline) EndRequest(reqID uint64) {
	pp.runtime.EndRequest(reqID)
	pp.dropRequestTracking(reqID)
}

func (pp *PluginPipeline) dropRequestTracking(reqID uint64) {
	pp.mu.Lock()
	delete(pp.streamKinds, reqID)
	delete(pp.streamVerify, reqID)
	delete(pp.requestPlugins, reqID)
	pp.mu.Unlock()
}

func (pp *PluginPipeline) recordInvocation(reqID uint64, name string) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.requestPlugins == nil {
		pp.requestPlugins = make(map[uint64]*requestPluginInvocations)
	}
	invocations := pp.requestPlugins[reqID]
	if invocations == nil {
		invocations = &requestPluginInvocations{seen: make(map[string]struct{})}
		pp.requestPlugins[reqID] = invocations
	}
	if _, exists := invocations.seen[name]; exists {
		return
	}
	invocations.seen[name] = struct{}{}
	invocations.names = append(invocations.names, name)
}

// InvokedPlugins returns the first-fire order for the exact request and exact
// pinned pipeline generation. The returned slice is detached from live state.
func (pp *PluginPipeline) InvokedPlugins(reqID uint64) []string {
	if pp == nil {
		return nil
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	invocations := pp.requestPlugins[reqID]
	if invocations == nil {
		return nil
	}
	return append([]string(nil), invocations.names...)
}

// Verdicts returns what plugins asked the host to do about this request.
//
// The grant was already checked per-plugin at the host call, so callers must
// NOT re-check it pipeline-wide. v1 asked "does any loaded plugin hold
// env.block_request?" — which meant one approved blocker let every other
// plugin's verdict through. Attribution now travels with the verdict.
func (pp *PluginPipeline) Verdicts(reqID uint64) *wasm.RequestVerdicts {
	if pp == nil {
		return nil
	}
	return pp.runtime.VerdictsFor(reqID)
}

// HasGrant reports whether any loaded plugin has actually been granted the
// named permission by the operator. Manifest requests alone confer no access.
func (pp *PluginPipeline) HasGrant(perm string) bool {
	for _, lp := range pp.plugins {
		if lp.plugin.HasGrant(perm) {
			return true
		}
	}
	return false
}

// DrainAndClose rejects future request admission, waits for active work, then
// closes the runtime exactly once. It does not use WaitGroup because Add racing
// with Wait at a zero count can close the runtime before a request is pinned.
func (pp *PluginPipeline) DrainAndClose() {
	pp.drainOnce.Do(func() {
		pp.mu.Lock()
		pp.draining = true
		if pp.active == 0 {
			close(pp.drained)
		}
		pp.mu.Unlock()
		go func() {
			<-pp.drained
			if err := pp.runtime.Close(); err != nil {
				log.Printf("[plugin] close old runtime: %v", err)
			}
			close(pp.closed)
		}()
	})
	<-pp.closed
}

// RunBeforeRequest calls every plugin that implements run_before_request.
//
// rawHeaders is the caller's incoming header map, UNTRUSTED input: the
// pipeline snapshots it at entry and applies its own fixed allowlist
// selection. The chat surface projects the five credential/identity headers
// into the host-owned _request_headers meta field PER PLUGIN: the field is
// injected only while the exact granted plugin's hook input is encoded, and
// the exact pre-injection ToranaMetaJson bytes are restored before the next
// plugin sees the request. Nil means no headers.
func (pp *PluginPipeline) RunBeforeRequest(ctx context.Context, reqID uint64, chat *engine.ChatRequest, rawHeaders map[string][]string) (*engine.ChatRequest, error) {
	out, _, err := pp.RunBeforeRequestTracked(ctx, reqID, chat, rawHeaders)
	return out, err
}

// RunBeforeRequestTracked is RunBeforeRequest plus an explicit replacement
// signal. The boolean is true only when a plugin returned an accepted request
// replacement. Host callers use it to distinguish observational/pass-only
// hooks from provider-visible request rewrites without comparing or
// re-marshalling the whole request. Side effects and verdicts do not set it.
func (pp *PluginPipeline) RunBeforeRequestTracked(ctx context.Context, reqID uint64, chat *engine.ChatRequest, rawHeaders map[string][]string) (*engine.ChatRequest, bool, error) {
	pp.Acquire()
	defer pp.Release()

	// The accepted request: its typed host-only TOPOLOGY facts (variant,
	// Code Assist flag, Responses layout) are restored onto the plugin
	// replacement below — they are never in the ABI.
	accepted := chat
	acceptedTopo := engine.TopologyFacts{
		CodeAssist:           accepted.CodeAssist,
		OpenAIVariant:        accepted.OpenAIVariant,
		ResponsesInputLayout: accepted.ResponsesInputLayout,
	}

	headers := snapshotHeaders(rawHeaders)
	// The accepted-input closure: the engine request is checked against the
	// SDK replacement domain BEFORE the first hook — zero/multi-arm blocks,
	// nested conflicts, non-finite floats, and out-of-range max tokens are
	// refused here, never silently truncated by the conversion. This runs
	// even with zero plugins (the no-plugin path): a request outside the
	// closed domain is a host-local failure, never a silent fact drop.
	current, err := pbconv.ToPBChatRequestChecked(chat)
	if err != nil {
		return nil, false, fmt.Errorf("invalid engine request: %w", err)
	}
	modified := false
	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_before_request") {
			continue
		}
		next, stop, err := pp.runBeforeRequestPlugin(ctx, reqID, lp, current, headers, acceptedTopo)
		if err != nil {
			return chat, false, err
		}
		if next != nil {
			current = next
			modified = true
		}
		if stop {
			break
		}
	}

	if !modified {
		// No plugin produced output — skip the pb round-trip entirely.
		return chat, false, nil
	}
	chat, convErr := pbconv.FromPBChatRequest(current)
	if convErr != nil {
		// The replacement path's PB always passed SDK ValidateReplacement
		// (or came from the checked boundary), so this is a defensive
		// backstop.
		return nil, false, fmt.Errorf("convert replacement: %w", convErr)
	}
	// The typed host-only TOPOLOGY facts survive the replacement: they are
	// never in the ABI, so the plugin round-trip cannot carry them — the
	// host restores them from the accepted request. A plugin can neither
	// forge nor lose the variant/layout facts.
	// EXACT restoration: the accepted request is the sole authority for
	// the host-only topology facts.
	chat.CodeAssist = accepted.CodeAssist
	chat.OpenAIVariant = accepted.OpenAIVariant
	chat.ResponsesInputLayout = accepted.ResponsesInputLayout
	return chat, true, nil
}

// runBeforeRequestPlugin dispatches ONE plugin's run_before_request hook with
// the per-plugin header projection: _request_headers is injected only for the
// exact granted plugin (from the pristine entry snapshot) and the exact
// pre-injection ToranaMetaJson bytes are restored on EVERY exit path before
// the request chains onward or returns.
//
// Return values: next = accepted replacement (nil = none; the only way the
// request changes), stop = a recorded block short-circuits the chain, err =
// an immediate error for the caller (failure-mode block paths).
func (pp *PluginPipeline) runBeforeRequestPlugin(ctx context.Context, reqID uint64, lp *loadedPlugin, current *pbv2.ChatRequest, headers map[string][]string, topo engine.TopologyFacts) (next *pbv2.ChatRequest, stop bool, err error) {
	// The per-plugin projection. The grant is checked on the exact executable
	// plugin object (lp.plugin), never on a manifest declaration and never
	// pipeline-wide.
	savedMeta := current.ToranaMetaJson
	if len(headers) > 0 && lp.plugin.HasGrant("env.request_headers") {
		injectRequestHeaders(current, projectChatHeaders(headers))
	}
	defer func() {
		// Byte-exact restoration on the request that chains (current) and on
		// any accepted replacement (next), on every exit path: successful
		// pass, accepted replacement, refused replacement, malformed result,
		// trap, block short-circuit, and immediate failure-mode return.
		restoreRequestHeaders(current, savedMeta)
		restoreRequestHeaders(next, savedMeta)
	}()

	inBytes, err := encodeHookInput(reqID, requestPayload{req: current})
	if err != nil {
		return nil, false, err
	}
	var outBytes []byte
	pp.recordInvocation(reqID, lp.manifest.Name)
	if err := lp.plugin.CallRequest(ctx, pbv2.Hook_HOOK_BEFORE_REQUEST, reqID, inBytes, &outBytes); err != nil {
		log.Printf("[plugin] %s run_before_request: %v", lp.manifest.Name, err)
		pp.discardTrapped(reqID, lp.manifest.Name)
		// The block check must happen on EVERY exit from this iteration,
		// not only the successful one. A plugin that blocks and then traps
		// leaves the block standing, and continuing here would hand the
		// request to every downstream plugin anyway — exactly the
		// PII-keeps-flowing problem short-circuiting exists to stop.
		if pp.blocked(reqID) {
			return nil, true, nil
		}
		if lp.failureMode == "block" {
			return nil, false, fmt.Errorf("plugin %s blocked request after failure: %w", lp.manifest.Name, err)
		}
		return nil, false, nil
	}
	res, err := decodeHookResult(outBytes, pbv2.Hook_HOOK_BEFORE_REQUEST)
	if err != nil {
		// A malformed or misdispatched action is the plugin's fault and is
		// refused whole rather than partly applied. A handwritten guest can
		// issue host calls and THEN return an invalid frame, so this path
		// gets the same treatment as a trap: its non-block verdicts are
		// discarded and a recorded block still short-circuits.
		log.Printf("[plugin] %s run_before_request: invalid result: %v", lp.manifest.Name, err)
		pp.discardTrapped(reqID, lp.manifest.Name)
		if pp.blocked(reqID) {
			return nil, true, nil
		}
		if lp.failureMode == "block" {
			return nil, false, fmt.Errorf("plugin %s returned an invalid result: %w", lp.manifest.Name, err)
		}
		return nil, false, nil
	}
	var replacement *pbv2.ChatRequest
	if res != nil {
		replacement = res.GetReplaceRequest()
		if replacement != nil {
			// Write-grant verification: the plugin may change only the
			// sections its operator granted it, and may never touch
			// host-owned facts or provenance. A fully-granted plugin
			// skips only the section comparison — grants authorise
			// SECTIONS, never host facts or provider-signature bindings,
			// so the all-grants fast path still runs the unconditional
			// invariants (torana_meta_json, signature provenance).
			// The injected _request_headers field is host-owned meta, so a
			// replacement that deletes or forges it fails the byte-identity
			// check below.
			var verr error
			if holdsAllRequestGrants(lp.plugin) {
				verr = verifyFastPath(current, replacement)
			} else {
				verr = verifyRequestMutation(current, replacement, lp.plugin.HasGrant)
			}
			if verr != nil {
				log.Printf("[plugin] %s run_before_request: rejected invalid replacement: %v",
					lp.manifest.Name, verr)
				// A refused replacement gets the same treatment as a trap:
				// non-block verdicts recorded before it are discarded — a
				// respond or route chosen by code that then produced an
				// invalid replacement is not trustworthy — while a
				// recorded block still fails closed and short-circuits in
				// the check below.
				pp.discardTrapped(reqID, lp.manifest.Name)
				if lp.failureMode == "block" {
					return nil, false, fmt.Errorf("plugin %s returned an invalid request replacement: %w",
						lp.manifest.Name, verr)
				}
				// allow: skip this plugin's replacement; the previous current
				// stays so the invalid output never chains downstream.
				replacement = nil
			}
		}
		if replacement != nil && pp.candidateValidator != nil {
			// Provider-specific output invalidity (after the generic/grant
			// validation): attributed to THIS plugin, with the settled
			// failure semantics — pass rolls back to the accepted input,
			// block produces the plugin refusal.
			if cerr := pp.candidateValidator(topo, current, replacement); cerr != nil {
				log.Printf("[plugin] %s run_before_request: rejected provider-invalid replacement: %v",
					lp.manifest.Name, cerr)
				pp.discardTrapped(reqID, lp.manifest.Name)
				if lp.failureMode == "block" {
					return nil, false, fmt.Errorf("plugin %s returned a provider-invalid request replacement: %w",
						lp.manifest.Name, cerr)
				}
				replacement = nil
			}
		}
		if replacement != nil {
			// The projection exists ONLY for the exact guest call: restore
			// the exact pre-injection metadata on the accepted replacement
			// BEFORE the host mutation observer sees it, so temporary
			// credentials never cross that boundary. (The defer below is
			// defense in depth for every other exit.)
			restoreRequestHeaders(replacement, savedMeta)
			// ObserveRequestMutation wants the request bytes, not the
			// envelope, so it is re-marshalled from the accepted action.
			if raw, err := proto.Marshal(replacement); err == nil {
				pp.runtime.ObserveRequestMutation(ctx, raw)
			}
			// The block check runs on EVERY exit: a plugin that blocks AND
			// replaces must not let any downstream plugin observe the
			// rejected request. The replacement is kept (block wins at the
			// transport), but the chain stops here.
			return replacement, pp.blocked(reqID), nil
		}
	}

	// Block short-circuits. v1 could not know a block had happened until
	// every plugin had run, so a rejected request was still handed to the
	// compactor and the warmer. A replacement from the blocking plugin is
	// kept but never sent upstream: block wins, and forcing an author to
	// discard edits before blocking would be a new footgun.
	if pp.blocked(reqID) {
		return nil, true, nil
	}
	return nil, false, nil
}

// RunAfterResponse calls every plugin that implements run_after_response.
//
// mutable says whether a returned replacement will be applied. It is false on
// the streamed and upstream-error paths, where the bytes have already gone to
// the caller or there is no body to rewrite. v1 discarded those replacements
// silently, so a plugin learned its edits had no effect only by observing that
// they had no effect.
func (pp *PluginPipeline) RunAfterResponse(ctx context.Context, reqID uint64, resp *engine.ChatResponse, mutable bool) (*engine.ChatResponse, error) {
	pp.Acquire()
	defer pp.Release()

	if resp == nil {
		return nil, nil
	}
	current := pbconv.ToPBChatResponse(resp)
	modified := false
	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_after_response") {
			continue
		}
		inBytes, err := encodeHookInput(reqID, responsePayload{resp: current, mutable: mutable})
		if err != nil {
			return resp, err
		}
		var outBytes []byte
		pp.recordInvocation(reqID, lp.manifest.Name)
		if err := lp.plugin.CallRequest(ctx, pbv2.Hook_HOOK_AFTER_RESPONSE, reqID, inBytes, &outBytes); err != nil {
			log.Printf("[plugin] %s run_after_response: %v", lp.manifest.Name, err)
			if lp.failureMode == "block" {
				return resp, fmt.Errorf("plugin %s blocked response after failure: %w", lp.manifest.Name, err)
			}
			continue
		}
		res, err := decodeHookResult(outBytes, pbv2.Hook_HOOK_AFTER_RESPONSE)
		if err != nil {
			log.Printf("[plugin] %s run_after_response: invalid result: %v", lp.manifest.Name, err)
			if lp.failureMode == "block" {
				return resp, fmt.Errorf("plugin %s returned an invalid result: %w", lp.manifest.Name, err)
			}
			continue
		}
		if res == nil {
			continue
		}
		if replacement := res.GetReplaceResponse(); replacement != nil {
			if !mutable {
				// Announced up front via HookInput.Mutable, so this is a
				// plugin ignoring the signal rather than a surprise. Say so
				// once instead of discarding it silently as v1 did.
				log.Printf("[plugin] %s returned a response replacement on an immutable path; discarding",
					lp.manifest.Name)
				continue
			}
			// The SDK validated the replacement's absolute well-formedness in
			// isolation, but a replacement is a MUTATION of the accepted
			// response: content presence and tool-call cardinality are
			// relative constraints only the host can enforce. Reject the
			// whole replacement before it can become the next plugin's input
			// or reach the response body.
			if err := validateResponseReplacement(current, replacement); err != nil {
				log.Printf("[plugin] %s run_after_response: rejected invalid replacement: %v",
					lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return resp, fmt.Errorf("plugin %s returned an invalid response replacement: %w",
						lp.manifest.Name, err)
				}
				// allow: skip this plugin's replacement; the previous current
				// stays so the invalid output never chains downstream.
				continue
			}
			// The replacement is accepted as a whole. A tool call that left
			// its provider token untouched over changed content is valid
			// (the apply block clears the wire token), but the pipeline must
			// not hand the next plugin a signature over content the provider
			// never signed — normalize before chaining.
			clearStaleSignatures(current, replacement)
			current = replacement
			modified = true
		}
	}

	if !modified {
		return resp, nil
	}
	return pbconv.FromPBChatResponse(current), nil
}

// RunOnStreamChunk calls every plugin that implements run_on_stream_chunk.
//
// Each plugin sees every event produced by the previous plugin in the chain.
// A zero-byte return passes the event through unchanged. Otherwise the action
// is either Suppress (drop it) or EmitEvents (replace it, or fan out to many).
//
// v2 removed the `handled` flag: suppression is an action rather than
// "handled=true with an empty list", so emitting nothing and passing through
// are no longer the same bytes on the wire.
func (pp *PluginPipeline) RunOnStreamChunk(ctx context.Context, reqID uint64, chunk *engine.StreamEvent) ([]engine.StreamEvent, error) {
	pp.Acquire()
	defer pp.Release()

	pp.mu.Lock()
	tracker := pp.streamKinds[reqID]
	if tracker == nil {
		tracker = &pbconv.BlockKindTracker{}
		pp.streamKinds[reqID] = tracker
	}
	pp.mu.Unlock()

	current := []*pbv2.StreamEvent{pbconv.ToPBStreamEvent(chunk)}

	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_on_stream_chunk") {
			continue
		}
		next := make([]*pbv2.StreamEvent, 0, len(current))
		for _, ev := range current {
			evBytes, err := encodeHookInput(reqID, streamPayload{ev: ev})
			if err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk encode: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after encode failure: %w", lp.manifest.Name, err)
				}
				next = append(next, ev)
				continue
			}
			var outBytes []byte
			pp.recordInvocation(reqID, lp.manifest.Name)
			if err := lp.plugin.CallRequest(ctx, pbv2.Hook_HOOK_ON_STREAM_CHUNK, reqID, evBytes, &outBytes); err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after failure: %w", lp.manifest.Name, err)
				}
				next = append(next, ev)
				continue
			}
			res, err := decodeHookResult(outBytes, pbv2.Hook_HOOK_ON_STREAM_CHUNK)
			if err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk: invalid result: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after invalid output: %w", lp.manifest.Name, err)
				}
				next = append(next, ev)
				continue
			}
			if res == nil {
				next = append(next, ev) // pass-through
				continue
			}
			if res.GetSuppress() != nil {
				// Deliberately emit nothing. Distinct on the wire from
				// pass-through, so an assembler buffering fragments can say
				// "not yet" without the host replaying the fragment.
				continue
			}
			if emit := res.GetEmitEvents(); emit != nil {
				// Validation already refused an empty or malformed list, so
				// this is a real replacement or fan-out.
				next = append(next, emit.Events...)
				continue
			}
			next = append(next, ev)
		}
		current = next
	}

	out := make([]engine.StreamEvent, 0, len(current))
	for _, ev := range current {
		// Kind-aware conversion: the tracker remembers which content block is
		// ACTUALLY open at each index (recorded from the converted starts,
		// which are what the rest of the host consumes), so a
		// ContentBlockStop becomes ToolCallEnd or BlockStop to match the
		// block it closes. A pass-through stream therefore survives plugins
		// with its block topology intact.
		converted, err := tracker.FromPBStreamEvent(ev)
		if err != nil {
			// The v2 ABI declares unknown/mismatched/duplicate/reused
			// topology invalid: a plugin emitted a stop with no open block at
			// its index, or a start at an index that is already open. The
			// conversion must never guess a kind, so this is a hard error —
			// on the streaming path the caller terminates the stream rather
			// than deliver a silently reclassified event.
			log.Printf("[plugin] stream topology error: %v", err)
			return nil, fmt.Errorf("plugin stream topology: %w", err)
		}
		out = append(out, *converted)
	}
	return out, nil
}

// ErrServeHTTPForbidden is returned by RunOnHTTPRequest when the named plugin
// exists and declares the run_on_http_request hook but does NOT hold the
// env.serve_http permission. The proxy route handler maps this to 403.
var ErrServeHTTPForbidden = fmt.Errorf("plugin does not hold env.serve_http permission")

// RunOnHTTPRequest dispatches an HTTP request to a single named plugin's
// run_on_http_request hook. It is used by the /_torana/plugin/<name>/* proxy
// route so plugins can serve their own HTTP UI/API namespace.
//
// Return values:
//
//	(nil, nil)                   — plugin not found, or does not declare
//	                               run_on_http_request; caller should 404.
//	(nil, ErrServeHTTPForbidden) — plugin exists and has the hook but lacks
//	                               the env.serve_http grant; caller → 403.
//	(*HttpResponse, nil)         — plugin returned a response; caller writes it.
//	(nil, other error)           — internal dispatch error; caller → 503.
//
// httpReq is built directly from net/http — it does not cross the engine IR.
func (pp *PluginPipeline) RunOnHTTPRequest(ctx context.Context, reqID uint64, pluginName string, httpReq *pbv2.HttpRequest, rawHeaders map[string][]string) (*pbv2.HttpResponse, error) {
	pp.Acquire()
	defer pp.Release()

	// Header policy runs HERE, at the dispatch boundary, against the exact
	// executing plugin's approved grants. rawHeaders is UNTRUSTED caller
	// input, snapshotted at entry; a caller-supplied prefiltered headers_json
	// is never authoritative. Filtering operates on a clone, so the caller's
	// httpReq argument stays byte-identical.
	headers := snapshotHeaders(rawHeaders)

	// Find the named plugin.
	var target *loadedPlugin
	for _, lp := range pp.plugins {
		if lp.manifest.Name == pluginName {
			target = lp
			break
		}
	}
	if target == nil {
		return nil, nil // not found
	}

	// Plugin must declare the hook.
	if !hasHook(target.manifest, "run_on_http_request") {
		return nil, nil // not serving HTTP
	}

	// Enforce env.serve_http capability.
	if !target.plugin.HasGrant("env.serve_http") {
		return nil, ErrServeHTTPForbidden
	}

	// The three-class filter: operational headers always, credential headers
	// only when the exact executing plugin holds the approved
	// env.request_headers grant, everything else never.
	cloned := proto.Clone(httpReq).(*pbv2.HttpRequest)
	filtered := filterHTTPHeaders(headers, target.plugin.HasGrant("env.request_headers"))
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: encode filtered headers: %w", pluginName, err)
	}
	cloned.HeadersJson = encoded

	inBytes, err := encodeHookInput(reqID, httpPayload{req: cloned})
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", pluginName, err)
	}

	var outBytes []byte
	if err := target.plugin.CallRequest(ctx, pbv2.Hook_HOOK_ON_HTTP_REQUEST, reqID, inBytes, &outBytes); err != nil {
		return nil, fmt.Errorf("plugin %s: run_on_http_request: %w", pluginName, err)
	}

	res, err := decodeHookResult(outBytes, pbv2.Hook_HOOK_ON_HTTP_REQUEST)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: invalid http result: %w", pluginName, err)
	}
	// Pass-through: the plugin did not serve this request. v1 needed a
	// `handled` flag here because an all-defaults HttpResponse marshals to
	// zero bytes and was therefore indistinguishable from declining. v2 makes
	// serving an action, so the absence of one IS declining.
	if res == nil {
		return nil, nil
	}
	return res.GetServeHttp(), nil
}

// TickOutcome is one plugin's report from a single tick.
type TickOutcome struct {
	Plugin  string
	Actions int
	Note    string
}

// RunOnTick calls every plugin that declares run_on_tick and holds the
// env.background_tick grant, returning what each reported.
//
// Two things differ from the request-driven hooks, both deliberate:
//
// The grant is checked per plugin here rather than being enforced by the
// caller, because there is no caller — a tick originates inside the host, so
// there is no request whose permissions could stand in for the plugin's own.
//
// failure_mode is deliberately not honoured. It selects whether a failing plugin
// blocks or passes the request, and on a tick there is no request to block; a
// plugin that traps has failed to do its own background work and cannot
// implicate anyone else's. Errors are logged and iteration continues, so one
// broken plugin cannot silently stop every other plugin's timer.
func (pp *PluginPipeline) RunOnTick(ctx context.Context, reqID uint64, tick *pbv2.TickRequest) []TickOutcome {
	pp.Acquire()
	defer pp.Release()

	inBytes, err := encodeHookInput(reqID, tickPayload{tick: tick})
	if err != nil {
		log.Printf("[plugin] tick: %v", err)
		return nil
	}

	var outcomes []TickOutcome
	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_on_tick") {
			continue
		}
		if !lp.plugin.HasGrant("env.background_tick") {
			// Not an error worth logging every tick: an unapproved capability
			// is ordinary operator state, and the plugin is already listed as
			// requesting it in the control plane.
			continue
		}
		var outBytes []byte
		if err := lp.plugin.CallRequest(ctx, pbv2.Hook_HOOK_ON_TICK, reqID, inBytes, &outBytes); err != nil {
			log.Printf("[plugin] %s run_on_tick: %v", lp.manifest.Name, err)
			continue
		}
		res, err := decodeHookResult(outBytes, pbv2.Hook_HOOK_ON_TICK)
		if err != nil {
			log.Printf("[plugin] %s run_on_tick: invalid result: %v", lp.manifest.Name, err)
			continue
		}
		// Pass-through means an idle tick: nothing was done, so there is
		// nothing to report. v1 needed a `handled` flag because an
		// all-defaults TickResult marshals to zero bytes.
		if res == nil {
			continue
		}
		outcome := res.GetTickOutcome()
		if outcome == nil {
			continue
		}
		outcomes = append(outcomes, TickOutcome{
			Plugin:  lp.manifest.Name,
			Actions: int(outcome.Actions),
			Note:    outcome.Note,
		})
	}
	return outcomes
}

// TicksEnabled reports whether any loaded plugin both declares run_on_tick and
// holds the grant, so the host can skip scheduling entirely when nothing wants
// it.
func (pp *PluginPipeline) TicksEnabled() bool {
	if pp == nil {
		return false
	}
	pp.Acquire()
	defer pp.Release()
	for _, lp := range pp.plugins {
		if hasHook(lp.manifest, "run_on_tick") && lp.plugin.HasGrant("env.background_tick") {
			return true
		}
	}
	return false
}

// ============================================================================
// Plugin config
// ============================================================================

type PluginConfig struct {
	Dir       string                     `json:"dir"`
	Order     []string                   `json:"order"`
	Config    map[string]json.RawMessage `json:"config"`
	Approvals map[string]Approval        `json:"approvals,omitempty"`
	// CandidateValidator runs AFTER the write-grant verification on every
	// accepted replacement candidate (per-plugin result transaction):
	// provider-specific output invalidity (e.g. a Code Assist envelope
	// smuggling canonical members) is attributed to the exact plugin —
	// pass rolls back to the accepted input, block produces the plugin
	// refusal. Set at CONSTRUCTION (immutable, race-free); the closure
	// captures the format policy, the accepted host TOPOLOGY arrives per
	// request via RunBeforeRequest. Format policy stays out of the
	// pipeline core.
	CandidateValidator func(topo engine.TopologyFacts, current, replacement *pbv2.ChatRequest) error
	// HostVersion is build metadata supplied by the executable. It is never
	// persisted in operator configuration.
	HostVersion string `json:"-"`

	// AllowUnapproved is for in-repository conformance tests and plugin
	// development only. It converts manifest requests into grants and is not a
	// security boundary. Production configuration never exposes this switch:
	// production grants come only from a digest-bound operator approval.
	AllowUnapproved bool `json:"-"`
	// Strict rejects an explicitly enabled plugin that cannot be loaded instead
	// of silently persisting a partially-active pipeline.
	Strict bool `json:"-"`
}

// Approval is operator-owned state, intentionally separate from plugin.json.
// It binds granted capabilities and the effective failure mode to one exact
// WASM digest. Updating the artifact therefore requires explicit reapproval.
type Approval struct {
	Digest      string   `json:"digest"`
	Permissions []string `json:"permissions"`
	FailureMode string   `json:"failure_mode,omitempty"`
}

func (c PluginConfig) approvalFor(bundle PluginBundle) (Approval, bool) {
	if c.AllowUnapproved {
		permissions := make([]string, 0, len(bundle.Manifest.Permissions))
		for _, permission := range bundle.Manifest.Permissions {
			permissions = append(permissions, permission.Name)
		}
		return Approval{
			Digest:      bundle.Digest,
			Permissions: permissions,
			FailureMode: bundle.Manifest.FailureMode,
		}, true
	}
	keys := []string{bundle.Manifest.ID, bundle.Manifest.Name}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if approval, ok := c.Approvals[key]; ok {
			return approval, true
		}
	}
	return Approval{}, false
}

func validateApproval(bundle PluginBundle, approval Approval) ([]string, string, error) {
	if approval.Digest == "" || approval.Digest != bundle.Digest {
		return nil, "", fmt.Errorf("digest mismatch: approved %q, installed %q", approval.Digest, bundle.Digest)
	}
	requested := make(map[string]struct{}, len(bundle.Manifest.Permissions))
	for _, permission := range bundle.Manifest.Permissions {
		requested[permission.Name] = struct{}{}
	}
	grants := make([]string, 0, len(approval.Permissions))
	for _, permission := range approval.Permissions {
		if _, ok := requested[permission]; !ok {
			return nil, "", fmt.Errorf("permission %q was not requested by manifest", permission)
		}
		grants = append(grants, permission)
	}
	// All-or-nothing: every permission the manifest declares must be in the
	// approval, or the plugin cannot be enabled. Approving a SUBSET of the
	// declared set would enable a plugin WITHOUT capabilities it declared
	// necessary — the empty subset of a grant-declaring manifest is exactly
	// that — producing degraded or silently ineffective behaviour instead of
	// failing loudly. The approval's permission set must therefore equal the
	// declared set (empty == empty is fine for grantless fixtures).
	approved := make(map[string]struct{}, len(approval.Permissions))
	for _, permission := range approval.Permissions {
		approved[permission] = struct{}{}
	}
	// Iterate in manifest order so the first missing permission reported to
	// an operator is stable across runs.
	for _, permission := range bundle.Manifest.Permissions {
		if _, ok := approved[permission.Name]; !ok {
			return nil, "", fmt.Errorf("permission %q requested by manifest was not approved", permission.Name)
		}
	}
	failureMode := approval.FailureMode
	if failureMode == "" {
		failureMode = bundle.Manifest.FailureMode
	}
	if failureMode == "" {
		failureMode = "pass"
	}
	if failureMode != "pass" && failureMode != "block" {
		return nil, "", fmt.Errorf("failure_mode must be pass or block, got %q", failureMode)
	}
	return grants, failureMode, nil
}

func hasHook(m PluginManifest, hook string) bool {
	for _, h := range m.Hooks {
		if h.Name == hook {
			return true
		}
	}
	return false
}

func hookNames(hooks []Hook) []string {
	var names []string
	for _, h := range hooks {
		names = append(names, h.Name)
	}
	return names
}

// ============================================================================
// Hot-Reload (fsnotify)
// ============================================================================

// WatchPlugins starts a file watcher on the plugins directory. When a
// .wasm or plugin.json file changes (or is removed), it calls reloadFn with
// a freshly built pipeline. The reloadFn should atomically swap the active
// pipeline.
//
// configFn is consulted at reload time so config hot-reloads (plugin order,
// per-plugin config) take effect without restarting the watcher. runtimeFn
// builds each reload's runtime — the caller wires host callbacks (offload,
// savings) there; a bare runtime would silently lose them.
func WatchPlugins(ctx context.Context, dir string, configFn func() PluginConfig, runtimeFn func() *wasm.Runtime, reloadFn func(pipeline *PluginPipeline), errorFn func(error), done func()) error {
	if dir == "" {
		dir = "./plugins"
	}

	// Create the directory if it is absent. A fresh install has no ./plugins —
	// nothing creates it but `torana plugin install`, and a git clone does not
	// carry it — so Torana refused to start at all until the operator had
	// installed a plugin they may not want. Creating it is what the operator
	// would have to do by hand, and it makes the watch below meaningful
	// immediately: a plugin installed later is seen without a restart.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create plugins directory %s: %w", dir, err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}

	// Watch the plugins directory and all subdirectories recursively. Initial
	// registration errors are fatal: returning success with no active watches
	// would make later installs or updates silently invisible.
	addRecursive := func(root string) error {
		return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				if err := w.Add(path); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := addRecursive(dir); err != nil {
		_ = w.Close()
		return fmt.Errorf("watch plugins directory %s: %w", dir, err)
	}

	go func() {
		defer w.Close()
		if done != nil {
			defer done()
		}

		// Debounce in this goroutine rather than time.AfterFunc. This serializes
		// reloads: an older, slow reload can never overwrite a newer one.
		var debounceTimer *time.Timer
		var debounceC <-chan time.Time
		const debounce = 500 * time.Millisecond

		for {
			select {
			case <-ctx.Done():
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				return

			case <-debounceC:
				debounceTimer = nil
				debounceC = nil
				if ctx.Err() != nil {
					return
				}
				newRT := runtimeFn()
				pp, err := reloadPipeline(newRT, configFn())
				if err != nil {
					log.Printf("[plugin] reload failed: %v", err)
					if errorFn != nil {
						errorFn(err)
					}
					newRT.Close()
					continue
				}
				if ctx.Err() != nil {
					newRT.Close()
					return
				}
				log.Printf("[plugin] hot-reload complete: %d plugins", len(pp.plugins))
				reloadFn(pp)

			case event, ok := <-w.Events:
				if !ok {
					return
				}
				// Handle newly created directories for recursive watching.
				if event.Op&fsnotify.Create == fsnotify.Create {
					if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
						if err := addRecursive(event.Name); err != nil {
							log.Printf("[plugin] watch new directory failed: %v", err)
							if errorFn != nil {
								errorFn(err)
							}
						}
						continue
					}
				}

				// Only reload on executable or consumed bundle metadata changes.
				name := filepath.Base(event.Name)
				if name != "plugin.wasm" && name != "plugin.json" &&
					name != "schema.json" && name != "agent.json" {
					continue
				}
				// Remove/Rename included: deleting a plugin must unload it.
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}

				if debounceTimer == nil {
					debounceTimer = time.NewTimer(debounce)
					debounceC = debounceTimer.C
				} else {
					if !debounceTimer.Stop() {
						select {
						case <-debounceTimer.C:
						default:
						}
					}
					debounceTimer.Reset(debounce)
				}

			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[plugin] fsnotify error: %v", err)
				if errorFn != nil {
					errorFn(err)
				}
			}
		}
	}()

	return nil
}

// blocked reports whether any plugin has refused this request.
//
// Consulted on every exit from a hook iteration — trap, invalid result and
// success alike. Checking only the success path let a plugin block, then trap,
// and still have every downstream plugin see the request.
func (pp *PluginPipeline) blocked(reqID uint64) bool {
	v := pp.runtime.VerdictsFor(reqID)
	return v != nil && v.Block() != nil
}

// discardTrapped applies trap semantics for a plugin whose call failed or whose
// result was refused.
//
// A block SURVIVES: a security verdict fails closed, and code that decided to
// refuse a request and then crashed still refused it. Respond, route and
// identity are DISCARDED — a half-built synthetic response, or a reroute chosen
// by code that crashed or returned garbage immediately afterwards, is not
// trustworthy enough to act on.
func (pp *PluginPipeline) discardTrapped(reqID uint64, plugin string) {
	pp.runtime.DiscardTrappedVerdicts(reqID, plugin)
}
