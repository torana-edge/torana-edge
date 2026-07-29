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
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	if manifest.ABIVersion != "v1" {
		return fmt.Errorf("unsupported abi_version %q", manifest.ABIVersion)
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
// the host was built with a semantic version. Development builds identify
// themselves with "dev" or a commit SHA, so compatibility remains governed by
// abi_version, hooks, and permissions until Edge has releases.
func validateHostCompatibility(manifest PluginManifest, hostVersion string) error {
	rawHostVersion := hostVersion
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
	if len(agentBytes) > 0 {
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

	mu        sync.Mutex
	active    int
	draining  bool
	drained   chan struct{}
	closed    chan struct{}
	drainOnce sync.Once
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
		if err := pl.ValidateHooks(context.Background(), hookNames(bundle.Manifest.Hooks)); err != nil {
			if config.Strict {
				return nil, fmt.Errorf("enabled plugin %q failed hook validation: %w", name, err)
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
		plugins: loaded,
		runtime: runtime,
		skipped: skipped,
		drained: make(chan struct{}),
		closed:  make(chan struct{}),
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
func (pp *PluginPipeline) EndRequest(reqID uint64) { pp.runtime.EndRequest(reqID) }

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

// RunOnChatRequest calls every plugin that implements run_before_request.
func (pp *PluginPipeline) RunBeforeRequest(ctx context.Context, reqID uint64, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	pp.Acquire()
	defer pp.Release()

	pbReq := pbconv.ToPBChatRequest(chat)
	reqBytes, err := proto.Marshal(pbReq)
	if err != nil {
		return chat, err
	}

	resultBytes := reqBytes
	modified := false
	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_before_request") {
			continue
		}
		var outBytes []byte
		if err := lp.plugin.CallRequest(ctx, "run_before_request", reqID, resultBytes, &outBytes); err != nil {
			log.Printf("[plugin] %s run_before_request: %v", lp.manifest.Name, err)
			if lp.failureMode == "block" {
				return chat, fmt.Errorf("plugin %s blocked request after failure: %w", lp.manifest.Name, err)
			}
			continue
		}
		if len(outBytes) > 0 {
			resultBytes = outBytes
			modified = true
			pp.runtime.ObserveRequestMutation(ctx, outBytes)
		}
	}

	if !modified {
		// No plugin produced output — skip the pb round-trip entirely.
		return chat, nil
	}
	var resReq pb.ChatRequest
	if err := proto.Unmarshal(resultBytes, &resReq); err != nil {
		return chat, err
	}
	return pbconv.FromPBChatRequest(&resReq), nil
}

// RunAfterResponse calls every plugin that implements run_after_response.
func (pp *PluginPipeline) RunAfterResponse(ctx context.Context, reqID uint64, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	pp.Acquire()
	defer pp.Release()

	pbReq := pbconv.ToPBChatRequest(chat)
	reqBytes, err := proto.Marshal(pbReq)
	if err != nil {
		return chat, err
	}

	resultBytes := reqBytes
	modified := false
	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_after_response") {
			continue
		}
		var outBytes []byte
		if err := lp.plugin.CallRequest(ctx, "run_after_response", reqID, resultBytes, &outBytes); err != nil {
			log.Printf("[plugin] %s run_after_response: %v", lp.manifest.Name, err)
			if lp.failureMode == "block" {
				return chat, fmt.Errorf("plugin %s blocked response after failure: %w", lp.manifest.Name, err)
			}
			continue
		}
		if len(outBytes) > 0 {
			resultBytes = outBytes
			modified = true
		}
	}

	if !modified {
		// No plugin produced output — skip the pb round-trip entirely.
		return chat, nil
	}
	var resReq pb.ChatRequest
	if err := proto.Unmarshal(resultBytes, &resReq); err != nil {
		return chat, err
	}
	return pbconv.FromPBChatRequest(&resReq), nil
}

// RunOnStreamChunk calls every plugin that implements run_on_stream_chunk.
//
// Each plugin sees every event produced by the previous plugin in the chain
// and returns a StreamEventResult per event: a zero-length return passes the
// event through unchanged; handled=true splices in its events (empty =
// suppress, one = replace, many = fan-out). The final event set replaces the
// input chunk in the stream — possibly empty.
func (pp *PluginPipeline) RunOnStreamChunk(ctx context.Context, reqID uint64, chunk *engine.StreamEvent) ([]engine.StreamEvent, error) {
	pp.Acquire()
	defer pp.Release()

	current := []*pb.StreamEvent{pbconv.ToPBStreamEvent(chunk)}

	for _, lp := range pp.plugins {
		if !hasHook(lp.manifest, "run_on_stream_chunk") {
			continue
		}
		next := make([]*pb.StreamEvent, 0, len(current))
		for _, ev := range current {
			evBytes, err := proto.Marshal(ev)
			if err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk marshal: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after marshal failure: %w", lp.manifest.Name, err)
				}
				next = append(next, ev)
				continue
			}
			var outBytes []byte
			if err := lp.plugin.CallRequest(ctx, "run_on_stream_chunk", reqID, evBytes, &outBytes); err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after failure: %w", lp.manifest.Name, err)
				}
				next = append(next, ev)
				continue
			}
			if len(outBytes) == 0 {
				// Passthrough: plugin did not handle this event.
				next = append(next, ev)
				continue
			}
			var res pb.StreamEventResult
			if err := proto.Unmarshal(outBytes, &res); err != nil {
				log.Printf("[plugin] %s run_on_stream_chunk unmarshal: %v", lp.manifest.Name, err)
				if lp.failureMode == "block" {
					return nil, fmt.Errorf("plugin %s blocked stream after invalid output: %w", lp.manifest.Name, err)
				}
				next = append(next, ev)
				continue
			}
			if !res.Handled {
				next = append(next, ev)
				continue
			}
			next = append(next, res.Events...)
		}
		current = next
	}

	out := make([]engine.StreamEvent, 0, len(current))
	for _, ev := range current {
		out = append(out, *pbconv.FromPBStreamEvent(ev))
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
func (pp *PluginPipeline) RunOnHTTPRequest(ctx context.Context, reqID uint64, pluginName string, httpReq *pb.HttpRequest) (*pb.HttpResponse, error) {
	pp.Acquire()
	defer pp.Release()

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

	inBytes, err := proto.Marshal(httpReq)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: marshal http request: %w", pluginName, err)
	}

	var outBytes []byte
	if err := target.plugin.CallRequest(ctx, "run_on_http_request", reqID, inBytes, &outBytes); err != nil {
		return nil, fmt.Errorf("plugin %s: run_on_http_request: %w", pluginName, err)
	}

	// Zero-length return → plugin did not handle the request.
	if len(outBytes) == 0 {
		return nil, nil
	}

	var resp pb.HttpResponse
	if err := proto.Unmarshal(outBytes, &resp); err != nil {
		return nil, fmt.Errorf("plugin %s: unmarshal http response: %w", pluginName, err)
	}

	// Explicit handled flag required — see proto comment.
	if !resp.Handled {
		return nil, nil
	}

	return &resp, nil
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
func (pp *PluginPipeline) RunOnTick(ctx context.Context, reqID uint64, tick *pb.TickRequest) []TickOutcome {
	pp.Acquire()
	defer pp.Release()

	inBytes, err := proto.Marshal(tick)
	if err != nil {
		log.Printf("[plugin] tick: marshal: %v", err)
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
		if err := lp.plugin.CallRequest(ctx, "run_on_tick", reqID, inBytes, &outBytes); err != nil {
			log.Printf("[plugin] %s run_on_tick: %v", lp.manifest.Name, err)
			continue
		}
		if len(outBytes) == 0 {
			continue // nothing to do this tick
		}
		var res pb.TickResult
		if err := proto.Unmarshal(outBytes, &res); err != nil {
			log.Printf("[plugin] %s run_on_tick: unmarshal: %v", lp.manifest.Name, err)
			continue
		}
		// Explicit handled flag required — see proto comment.
		if !res.Handled {
			continue
		}
		outcomes = append(outcomes, TickOutcome{
			Plugin:  lp.manifest.Name,
			Actions: int(res.Actions),
			Note:    res.Note,
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
