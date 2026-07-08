package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/engine"
)

// testCache is a shared cache for tests.
func testCache() cache.IntentCache {
	return cache.NewLocalCache(5 * time.Minute)
}

// ---------------------------------------------------------------------------
// Schema mutation tests
// ---------------------------------------------------------------------------

func TestSchemaTranslator_MutatesAdditionalPropertiesToString(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
						"env": map[string]any{
							"type":                 "object",
							"additionalProperties": map[string]any{"type": "string"},
						},
					},
					"required": []any{"command"},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	result, err := st.BeforeRequest(context.Background(), req, chat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tool := result.Tools[0]
	props := tool.Parameters["properties"].(map[string]any)

	env, ok := props["env"].(map[string]any)
	if !ok {
		t.Fatal("env is not a map")
	}
	if env["type"] != "array" {
		t.Errorf("env type = %v, want array", env["type"])
	}

	items, ok := env["items"].(map[string]any)
	if !ok {
		t.Fatal("env.items is not a map")
	}
	if items["type"] != "object" {
		t.Errorf("env.items.type = %v, want object", items["type"])
	}

	itemProps := items["properties"].(map[string]any)
	if _, ok := itemProps["key"]; !ok {
		t.Error("env.items missing 'key' property")
	}
	if _, ok := itemProps["value"]; !ok {
		t.Error("env.items missing 'value' property")
	}

	// items object should have additionalProperties: false.
	if ap, ok := items["additionalProperties"]; !ok || ap != false {
		t.Errorf("env.items.additionalProperties should be false, got %v", ap)
	}

	// env is now an array — additionalProperties is on items, not the array itself.
	if ap, ok := env["additionalProperties"]; ok {
		t.Errorf("env is an array, should not have additionalProperties — got %v", ap)
	}

	registry, ok := result.ToranaMeta[metaKeyMutations].(map[string][]string)
	if !ok {
		t.Fatal("no mutation registry in ToranaMeta")
	}
	if len(registry["bash"]) != 1 || registry["bash"][0] != "env" {
		t.Errorf("registry[bash] = %v, want [env]", registry["bash"])
	}

	if tool.Strict {
		t.Error("tool.Strict should be false — we no longer enforce strict mode")
	}
}

func TestSchemaTranslator_AdditionalPropertiesTrue(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"headers": map[string]any{
							"type":                 "object",
							"additionalProperties": true,
						},
					},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, chat)

	tool := result.Tools[0]
	props := tool.Parameters["properties"].(map[string]any)
	headers := props["headers"].(map[string]any)

	items := headers["items"].(map[string]any)
	valueProp := items["properties"].(map[string]any)["value"].(map[string]any)
	if valueProp["type"] != "string" {
		t.Errorf("value type = %v, want string", valueProp["type"])
	}
}

func TestSchemaTranslator_InjectsIntentAtRoot(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []any{"command"},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, chat)

	tool := result.Tools[0]
	props := tool.Parameters["properties"].(map[string]any)

	intent, ok := props[ToranaIntentField].(map[string]any)
	if !ok {
		t.Fatalf("%s not injected", ToranaIntentField)
	}
	if intent["type"] != "string" {
		t.Errorf("intent type = %v, want string", intent["type"])
	}

	required := tool.Parameters["required"].([]any)
	found := false
	for _, r := range required {
		if r == ToranaIntentField {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("%s not in required array", ToranaIntentField)
	}

	ap := tool.Parameters["additionalProperties"]
	if ap != false {
		t.Errorf("root additionalProperties = %v, want false", ap)
	}
}

func TestSchemaTranslator_Idempotent(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)

	r1, _ := st.BeforeRequest(context.Background(), req, chat)
	r2, _ := st.BeforeRequest(context.Background(), req, r1)

	props := r2.Tools[0].Parameters["properties"].(map[string]any)
	if _, ok := props[ToranaIntentField]; !ok {
		t.Errorf("%s missing after second pass", ToranaIntentField)
	}

	required := r2.Tools[0].Parameters["required"].([]any)
	intentCount := 0
	for _, r := range required {
		if r == ToranaIntentField {
			intentCount++
		}
	}
	if intentCount != 1 {
		t.Errorf("intent appears %d times in required, want 1", intentCount)
	}

	registry, _ := r2.ToranaMeta[metaKeyMutations].(map[string][]string)
	muts := registry["bash"]
	if len(muts) > 0 {
		t.Errorf("expected no mutations for simple schema, got %v", muts)
	}
}

func TestSchemaTranslator_NoToolsPassthrough(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{}
	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, chat)
	if result.ToranaMeta != nil {
		t.Error("ToranaMeta should be nil when no tools present")
	}
}

func TestSchemaTranslator_NilChatPassthrough(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, nil)
	if result != nil {
		t.Error("should return nil for nil chat")
	}
}

func TestSchemaTranslator_NestedObjects(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "deploy",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"config": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"env_vars": map[string]any{
									"type":                 "object",
									"additionalProperties": map[string]any{"type": "string"},
								},
							},
						},
					},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, chat)

	registry, _ := result.ToranaMeta[metaKeyMutations].(map[string][]string)
	muts := registry["deploy"]
	if len(muts) != 1 || muts[0] != "config.env_vars" {
		t.Errorf("mutations = %v, want [config.env_vars]", muts)
	}

	props := result.Tools[0].Parameters["properties"].(map[string]any)
	config := props["config"].(map[string]any)
	configProps := config["properties"].(map[string]any)
	envVars := configProps["env_vars"].(map[string]any)
	if envVars["type"] != "array" {
		t.Error("nested env_vars not converted to array")
	}
}

func TestSchemaTranslator_ArrayOfObjects(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "batch",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"steps": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"name": map[string]any{"type": "string"},
									"env": map[string]any{
										"type":                 "object",
										"additionalProperties": map[string]any{"type": "string"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, chat)

	registry, _ := result.ToranaMeta[metaKeyMutations].(map[string][]string)
	muts := registry["batch"]
	if len(muts) != 1 || muts[0] != "steps[].env" {
		t.Errorf("mutations = %v, want [steps[].env]", muts)
	}

	props := result.Tools[0].Parameters["properties"].(map[string]any)
	steps := props["steps"].(map[string]any)
	items := steps["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)
	env := itemProps["env"].(map[string]any)
	if env["type"] != "array" {
		t.Error("array item env not converted to KV array")
	}
}

func TestSchemaTranslator_NoAdditionalPropertiesPassthrough(t *testing.T) {
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "read_file",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, chat)

	registry, _ := result.ToranaMeta[metaKeyMutations].(map[string][]string)
	if len(registry) != 0 {
		t.Errorf("unexpected mutations: %v", registry)
	}

	props := result.Tools[0].Parameters["properties"].(map[string]any)
	if _, ok := props[ToranaIntentField]; !ok {
		t.Errorf("%s not injected even without additionalProperties", ToranaIntentField)
	}
}

// ---------------------------------------------------------------------------
// Reverse translation tests
// ---------------------------------------------------------------------------

func TestReverseTranslate_ExtractsIntent(t *testing.T) {
	argsJSON := `{"command": "ls", "i": "find the error log"}`
	registry := map[string][]string{}
	result, intent := ReverseTranslate("bash", argsJSON, registry)

	if intent != "find the error log" {
		t.Errorf("intent = %q, want %q", intent, "find the error log")
	}

	var parsed map[string]any
	json.Unmarshal([]byte(result), &parsed)
	if parsed["command"] != "ls" {
		t.Errorf("command = %v, want ls", parsed["command"])
	}
}

func TestReverseTranslate_KVArrayToObject(t *testing.T) {
	argsJSON := `{
		"command": "./build.sh",
		"i": "run build",
		"env": [
			{"key": "NODE_ENV", "value": "staging"},
			{"key": "DEBUG", "value": "true"}
		]
	}`

	registry := map[string][]string{"bash": {"env"}}
	result, intent := ReverseTranslate("bash", argsJSON, registry)

	if intent != "run build" {
		t.Errorf("intent = %q, want 'run build'", intent)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	env, ok := parsed["env"].(map[string]any)
	if !ok {
		t.Fatalf("env = %T, want map[string]any", parsed["env"])
	}
	if env["NODE_ENV"] != "staging" {
		t.Errorf("env.NODE_ENV = %v, want staging", env["NODE_ENV"])
	}
	if env["DEBUG"] != "true" {
		t.Errorf("env.DEBUG = %v, want true", env["DEBUG"])
	}
}

func TestReverseTranslate_NestedKVArray(t *testing.T) {
	argsJSON := `{
		"config": {
			"env_vars": [
				{"key": "PORT", "value": "3000"},
				{"key": "HOST", "value": "localhost"}
			]
		},
		"i": "deploy config"
	}`

	registry := map[string][]string{"deploy": {"config.env_vars"}}
	result, intent := ReverseTranslate("deploy", argsJSON, registry)

	if intent != "deploy config" {
		t.Errorf("intent = %q", intent)
	}

	var parsed map[string]any
	json.Unmarshal([]byte(result), &parsed)

	config, _ := parsed["config"].(map[string]any)
	envVars, _ := config["env_vars"].(map[string]any)
	if envVars["PORT"] != "3000" {
		t.Errorf("PORT = %v", envVars["PORT"])
	}
	if envVars["HOST"] != "localhost" {
		t.Errorf("HOST = %v", envVars["HOST"])
	}
}

func TestReverseTranslate_NoMutationsPassthrough(t *testing.T) {
	argsJSON := `{"command": "ls", "i": "list files"}`
	registry := map[string][]string{}
	result, intent := ReverseTranslate("bash", argsJSON, registry)

	if intent != "list files" {
		t.Errorf("intent = %q", intent)
	}

	var parsed map[string]any
	json.Unmarshal([]byte(result), &parsed)
	if parsed["command"] != "ls" {
		t.Errorf("command = %v, want ls", parsed["command"])
	}
}

func TestReverseTranslate_UnknownToolPassthrough(t *testing.T) {
	argsJSON := `{
		"i": "test",
		"env": [{"key": "X", "value": "Y"}]
	}`
	registry := map[string][]string{"other_tool": {"env"}}
	result, intent := ReverseTranslate("bash", argsJSON, registry)

	if intent != "test" {
		t.Errorf("intent = %q", intent)
	}

	var parsed map[string]any
	json.Unmarshal([]byte(result), &parsed)
	// Heuristic reversal should convert KV arrays even for unknown tools.
	if _, ok := parsed["env"].(map[string]any); !ok {
		t.Error("heuristic reversal should convert KV array to object for unknown tool")
	}
}

func TestReverseTranslate_EmptyArgs(t *testing.T) {
	result, intent := ReverseTranslate("bash", "", nil)
	if result != "" {
		t.Errorf("empty args should return empty, got %q", result)
	}
	if intent != "" {
		t.Errorf("intent should be empty for empty args")
	}
}

func TestReverseTranslate_InvalidJSON(t *testing.T) {
	result, intent := ReverseTranslate("bash", "not json", nil)
	if result != "not json" {
		t.Errorf("invalid JSON should passthrough unchanged, got %q", result)
	}
	if intent != "" {
		t.Errorf("intent should be empty for invalid JSON")
	}
}

// ---------------------------------------------------------------------------
// Round-trip test
// ---------------------------------------------------------------------------

func TestRoundTrip_MutateThenReverse(t *testing.T) {
	st := NewSchemaTranslator(testCache())

	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
						"env": map[string]any{
							"type":                 "object",
							"additionalProperties": map[string]any{"type": "string"},
						},
					},
					"required": []any{"command"},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	mutated, _ := st.BeforeRequest(context.Background(), req, chat)

	llmResponse := `{
		"command": "npm test",
		"i": "find the failing test",
		"env": [
			{"key": "CI", "value": "true"},
			{"key": "NODE_ENV", "value": "test"}
		]
	}`

	registry, _ := mutated.ToranaMeta[metaKeyMutations].(map[string][]string)
	result, intent := ReverseTranslate("bash", llmResponse, registry)

	if intent != "find the failing test" {
		t.Errorf("intent = %q", intent)
	}

	var parsed map[string]any
	json.Unmarshal([]byte(result), &parsed)

	if parsed["command"] != "npm test" {
		t.Errorf("command = %v", parsed["command"])
	}

	env := parsed["env"].(map[string]any)
	if env["CI"] != "true" {
		t.Errorf("env.CI = %v", env["CI"])
	}
	if env["NODE_ENV"] != "test" {
		t.Errorf("env.NODE_ENV = %v", env["NODE_ENV"])
	}
}

func TestSchemaTranslator_ImplicitOpenMap(t *testing.T) {
	// An object with no properties and no additionalProperties declaration
	// should be treated as an implicit open map and converted to KV array.
	st := NewSchemaTranslator(testCache())
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
						"headers": map[string]any{
							"type": "object",
							// No properties, no additionalProperties — implicit open map.
						},
					},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	result, _ := st.BeforeRequest(context.Background(), req, chat)

	props := result.Tools[0].Parameters["properties"].(map[string]any)
	headers := props["headers"].(map[string]any)

	// Should be converted to a KV array, not left as a locked object.
	if headers["type"] != "array" {
		t.Fatalf("implicit open map should be converted to array, got type=%v", headers["type"])
	}

	items := headers["items"].(map[string]any)
	if items["type"] != "object" {
		t.Error("items should be object")
	}

	itemProps := items["properties"].(map[string]any)
	if _, ok := itemProps["key"]; !ok {
		t.Error("items missing 'key'")
	}
	if _, ok := itemProps["value"]; !ok {
		t.Error("items missing 'value'")
	}

	// Verify it's in the mutation registry.
	registry, _ := result.ToranaMeta[metaKeyMutations].(map[string][]string)
	if paths, ok := registry["bash"]; !ok || len(paths) == 0 {
		t.Error("implicit open map should be in mutation registry")
	}
}
