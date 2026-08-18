package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	"google.golang.org/protobuf/proto"
)

func governorObject(t *testing.T, raw string) engine.RequiredJSONObject {
	t.Helper()
	value, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func governorMarker(t *testing.T, raw string) engine.OptionalJSONObject {
	t.Helper()
	value, err := engine.ParseOptionalJSONObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func governorRequest(t *testing.T) *engine.ChatRequest {
	t.Helper()
	return &engine.ChatRequest{
		Model: "model",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{
			Text: &engine.TextBlock{Text: "help"},
		}}}},
		Tools: []engine.ToolDef{
			{Name: "read", Description: "old", Parameters: governorObject(t, `{"old":1}`), Strict: true,
				CacheControl: governorMarker(t, `{ "type":"ephemeral" }`)},
			{Name: "search", Description: "keep", Parameters: governorObject(t, `{"q":1.0}`),
				CacheControl: governorMarker(t, `{"ttl":"1h"}`)},
			{Name: "shell", Description: "remove", Parameters: governorObject(t, `{}`)},
		},
	}
}

func TestToolGovernorRealBundleAppliesExactPolicy(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "tool_governor")
	config := map[string]json.RawMessage{
		"tool_governor": json.RawMessage(`{
			"allow":["read","search"],
			"replace":{"read":{"description":"approved","parameters":{"a":1.0},"strict":false}}
		}`),
	}
	pp := newTestPipelineWith(t, bundles, []string{"tool_governor"}, cache.NewLocalCache(0), config)
	input := governorRequest(t)
	before, err := pbconv.ToPBChatRequestChecked(input)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	afterInput, err := pbconv.ToPBChatRequestChecked(input)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(before, afterInput) {
		t.Fatal("real guest mutated the accepted engine request in place")
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools = %+v, want read and search", out.Tools)
	}
	if out.Tools[0].Name != "read" || out.Tools[0].Description != "approved" || out.Tools[0].Strict ||
		string(out.Tools[0].Parameters.Bytes()) != `{"a":1.0}` ||
		string(out.Tools[0].CacheControl.Bytes()) != `{ "type":"ephemeral" }` {
		t.Fatalf("governed read definition = %+v", out.Tools[0])
	}
	if out.Tools[1].Name != "search" || out.Tools[1].Description != "keep" || out.Tools[1].Strict ||
		string(out.Tools[1].Parameters.Bytes()) != `{"q":1.0}` ||
		string(out.Tools[1].CacheControl.Bytes()) != `{"ttl":"1h"}` {
		t.Fatalf("unrelated definition changed = %+v", out.Tools[1])
	}
}

func TestToolGovernorRealBundleNoopPreservesWholeRequest(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "tool_governor")
	pp := newTestPipelineWith(t, bundles, []string{"tool_governor"}, cache.NewLocalCache(0), map[string]json.RawMessage{
		"tool_governor": json.RawMessage(`{"replace":{"absent":{"strict":true}}}`),
	})
	input := governorRequest(t)
	before, err := pbconv.ToPBChatRequestChecked(input)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pp.RunBeforeRequest(context.Background(), 2, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := pbconv.ToPBChatRequestChecked(out)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(before, after) {
		t.Fatal("no-op policy changed the request")
	}
}

func TestToolGovernorRealBundleRequiresExactWriteGrantUnion(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "tool_governor")
	for _, tc := range []struct {
		name      string
		remove    string
		config    string
		request   func(*testing.T) *engine.ChatRequest
		wantGrant string
	}{
		{
			name: "definition replacement needs tools grant", remove: "ir.tools.write",
			config:  `{"replace":{"read":{"description":"approved"}}}`,
			request: governorRequest, wantGrant: "ir.tools.write",
		},
		{
			name: "marker position shift needs cache grant", remove: "ir.cache_control.write",
			config:  `{"allow":["search","shell"]}`,
			request: governorRequest, wantGrant: "ir.cache_control.write",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staged := stageGovernorBundleWithout(t, bundles, tc.remove)
			pp := newTestPipelineWith(t, staged, []string{"tool_governor"}, cache.NewLocalCache(0), map[string]json.RawMessage{
				"tool_governor": json.RawMessage(tc.config),
			})
			input := tc.request(t)
			before, err := pbconv.ToPBChatRequestChecked(input)
			if err != nil {
				t.Fatal(err)
			}
			out, err := pp.RunBeforeRequest(context.Background(), 3, input, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantGrant) || !strings.Contains(err.Error(), "tool_governor") {
				t.Fatalf("error = %v, want plugin and %s", err, tc.wantGrant)
			}
			if out != input {
				t.Fatal("block-mode refusal did not return the accepted request")
			}
			after, convErr := pbconv.ToPBChatRequestChecked(input)
			if convErr != nil || !proto.Equal(before, after) {
				t.Fatal("refused replacement mutated the accepted request")
			}
		})
	}
}

func TestToolGovernorRealBundleFinalWireAcrossFormats(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "tool_governor")
	pp := newTestPipelineWith(t, bundles, []string{"tool_governor"}, cache.NewLocalCache(0), map[string]json.RawMessage{
		"tool_governor": json.RawMessage(`{
			"allow":["read"],
			"replace":{"read":{"description":"approved","parameters":{"approved":1.0},"strict":false}}
		}`),
	})
	rows := []struct {
		name   string
		format string
		body   string
		cache  bool
	}{
		{
			name: "openai chat", format: "openai",
			body: `{"model":"m","messages":[{"role":"user","content":"help"}],"tools":[` +
				`{"type":"function","function":{"name":"read","description":"old","parameters":{"old":1},"strict":true}},` +
				`{"type":"function","function":{"name":"shell","description":"remove","parameters":{}}}]}`,
		},
		{
			name: "anthropic", format: "anthropic", cache: true,
			body: `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"help"}],"tools":[` +
				`{"name":"read","description":"old","input_schema":{"old":1},"cache_control":{"type":"ephemeral"}},` +
				`{"name":"shell","description":"remove","input_schema":{}}]}`,
		},
		{
			name: "bedrock", format: "bedrock", cache: true,
			body: `{"modelId":"m","messages":[{"role":"user","content":[{"text":"help"}]}],"toolConfig":{"tools":[` +
				`{"toolSpec":{"name":"read","description":"old","inputSchema":{"json":{"old":1}}}},` +
				`{"cachePoint":{"type":"default"}},` +
				`{"toolSpec":{"name":"shell","description":"remove","inputSchema":{"json":{}}}}]}}`,
		},
		{
			name: "gemini", format: "gemini",
			body: `{"contents":[{"role":"user","parts":[{"text":"help"}]}],"tools":[{"functionDeclarations":[` +
				`{"name":"read","description":"old","parameters":{"old":1}},` +
				`{"name":"shell","description":"remove","parameters":{}}]}]}`,
		},
		{
			name: "code assist", format: "gemini-codeassist",
			body: `{"model":"m","request":{"contents":[{"role":"user","parts":[{"text":"help"}]}],"tools":[{"functionDeclarations":[` +
				`{"name":"read","description":"old","parameters":{"old":1}},` +
				`{"name":"shell","description":"remove","parameters":{}}]}]}}`,
		},
	}
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			adapter := format.Lookup(row.format)
			if adapter == nil {
				t.Fatalf("format %q not registered", row.format)
			}
			chat, err := adapter.Request.Unmarshal([]byte(row.body))
			if err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			out, err := pp.RunBeforeRequest(context.Background(), uint64(100+i), chat, nil)
			if err != nil {
				t.Fatalf("real guest: %v", err)
			}
			if len(out.Tools) != 1 || out.Tools[0].Name != "read" || out.Tools[0].Description != "approved" ||
				out.Tools[0].Strict || string(out.Tools[0].Parameters.Bytes()) != `{"approved":1.0}` {
				t.Fatalf("governed IR tools = %+v", out.Tools)
			}
			if row.cache != !out.Tools[0].CacheControl.IsAbsent() {
				t.Fatalf("cache marker presence = %v, want %v", !out.Tools[0].CacheControl.IsAbsent(), row.cache)
			}
			wire, err := adapter.Request.Marshal(out)
			if err != nil {
				t.Fatalf("marshal governed request: %v", err)
			}
			assertGovernorWire(t, row.format, wire, row.cache)
		})
	}
}

func assertGovernorWire(t *testing.T, formatName string, wire []byte, wantCache bool) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(wire, &root); err != nil {
		t.Fatalf("final wire JSON: %v", err)
	}
	if formatName == "gemini-codeassist" {
		root, _ = root["request"].(map[string]any)
		if root == nil {
			t.Fatalf("Code Assist request envelope missing: %s", wire)
		}
	}
	var definitions []map[string]any
	switch formatName {
	case "openai":
		for _, raw := range asJSONArray(t, root["tools"], "tools") {
			outer := asJSONObject(t, raw, "tool")
			definitions = append(definitions, asJSONObject(t, outer["function"], "function"))
		}
	case "anthropic":
		for _, raw := range asJSONArray(t, root["tools"], "tools") {
			definitions = append(definitions, asJSONObject(t, raw, "tool"))
		}
	case "bedrock":
		config := asJSONObject(t, root["toolConfig"], "toolConfig")
		cachePoints := 0
		for _, raw := range asJSONArray(t, config["tools"], "toolConfig.tools") {
			entry := asJSONObject(t, raw, "tool entry")
			if spec, ok := entry["toolSpec"]; ok {
				definitions = append(definitions, asJSONObject(t, spec, "toolSpec"))
			}
			if _, ok := entry["cachePoint"]; ok {
				cachePoints++
			}
		}
		if (cachePoints == 1) != wantCache {
			t.Fatalf("Bedrock cachePoint count = %d, want presence %v", cachePoints, wantCache)
		}
	case "gemini", "gemini-codeassist":
		groups := asJSONArray(t, root["tools"], "tools")
		if len(groups) != 1 {
			t.Fatalf("tool groups = %d, want 1", len(groups))
		}
		group := asJSONObject(t, groups[0], "tool group")
		for _, raw := range asJSONArray(t, group["functionDeclarations"], "functionDeclarations") {
			definitions = append(definitions, asJSONObject(t, raw, "function declaration"))
		}
	default:
		t.Fatalf("unhandled format %q", formatName)
	}
	if len(definitions) != 1 {
		t.Fatalf("final definitions = %+v, want exactly read", definitions)
	}
	definition := definitions[0]
	if definition["name"] != "read" || definition["description"] != "approved" {
		t.Fatalf("final definition identity = %+v", definition)
	}
	var parameters any
	switch formatName {
	case "anthropic":
		parameters = definition["input_schema"]
	case "bedrock":
		parameters = asJSONObject(t, definition["inputSchema"], "inputSchema")["json"]
	default:
		parameters = definition["parameters"]
	}
	object := asJSONObject(t, parameters, "parameters")
	if len(object) != 1 || object["approved"] != float64(1) {
		t.Fatalf("final parameters = %+v", object)
	}
	if formatName == "anthropic" {
		_, present := definition["cache_control"]
		if present != wantCache {
			t.Fatalf("Anthropic cache_control presence = %v, want %v", present, wantCache)
		}
	}
}

func asJSONObject(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want object", name, value)
	}
	return object
}

func asJSONArray(t *testing.T, value any, name string) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("%s = %T, want array", name, value)
	}
	return array
}

func stageGovernorBundleWithout(t *testing.T, bundles, remove string) string {
	t.Helper()
	root := t.TempDir()
	dst := filepath.Join(root, "tool_governor")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(bundles, "tool_governor")
	for _, name := range []string{"plugin.wasm", "schema.json"} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(src, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion        int    `json:"schema_version"`
		ID                   string `json:"id"`
		Name                 string `json:"name"`
		Version              string `json:"version"`
		ABIVersion           string `json:"abi_version"`
		MinimumToranaVersion string `json:"minimum_torana_version"`
		FailureMode          string `json:"failure_mode"`
		Repository           string `json:"repository"`
		Description          string `json:"description"`
		Hooks                []struct {
			Name string `json:"name"`
		} `json:"hooks"`
		Permissions []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	filtered := manifest.Permissions[:0]
	for _, permission := range manifest.Permissions {
		if permission.Name != remove {
			filtered = append(filtered, permission)
		}
	}
	if len(filtered) != len(manifest.Permissions)-1 {
		t.Fatalf("permission %q not found exactly once", remove)
	}
	manifest.Permissions = filtered
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(dst, "plugin.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}
