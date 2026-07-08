// Package middleware provides hooks for the Torana proxy pipeline.
package middleware

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/engine"
)

// SchemaTranslator implements bidirectional schema translation.
// It converts open-ended maps (additionalProperties) into arrays of
// {key, value} pairs on the way to the LLM, and reverses the
// transformation on the way back to the harness.
//
// It implements both RequestHook and ResponseHook.
type SchemaTranslator struct {
	IntentCache cache.IntentCache
}

// NewSchemaTranslator creates a SchemaTranslator with the given intent cache.
func NewSchemaTranslator(ic cache.IntentCache) *SchemaTranslator {
	return &SchemaTranslator{IntentCache: ic}
}

func (st *SchemaTranslator) Name() string { return "schema-translator" }

// ---------------------------------------------------------------------------
// RequestHook — schema mutation (harness → LLM)
// ---------------------------------------------------------------------------

// metaKey is the ToranaMeta key for the per-request mutation registry.
const metaKeyMutations = "_torana_mutations"

// metaKeyIntentCache is the ToranaMeta key for the intent cache reference.
const metaKeyIntentCache = "_torana_intent_cache"

// BeforeRequest mutates tool schemas: converts additionalProperties maps
// to KV arrays, injects the "i" intent field, and records all mutations
// in the request's ToranaMeta for later reversal.
func (st *SchemaTranslator) BeforeRequest(ctx context.Context, req *http.Request, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	if chat == nil || len(chat.Tools) == 0 {
		return chat, nil
	}

	if chat.ToranaMeta == nil {
		chat.ToranaMeta = make(map[string]any)
	}

	// Inject a targeted system-prompt addendum that tells the model
	// what we expect in the "i" field on tool calls.
	injectSystemPrompt(chat)

	// toolName → list of dotted paths that were converted to KV arrays
	registry := make(map[string][]string)

	for i := range chat.Tools {
		tool := &chat.Tools[i]

		// Ensure parameters exist.
		if tool.Parameters == nil {
			tool.Parameters = make(map[string]any)
		}
		if tool.Parameters["type"] == nil {
			tool.Parameters["type"] = "object"
		}

		// Recursively translate the schema.
		mutations := translateSchema(tool.Name, tool.Parameters, "")
		if len(mutations) > 0 {
			registry[tool.Name] = mutations
		}

		// Inject the "i" intent field.
		injectIntentParam(tool)
	}

	chat.ToranaMeta[metaKeyMutations] = registry
	log.Printf("[schema-translator] mutated %d tools, %d with KV conversions",
		len(chat.Tools), len(registry))

	return chat, nil
}

// ---------------------------------------------------------------------------
// ResponseHook — reverse translation (LLM → harness)
// ---------------------------------------------------------------------------

// AfterResponse buffers tool-call argument deltas, then on ToolCallEnd
// assembles the full arguments JSON, extracts intent, reverses KV-array
// mutations, and emits sanitized deltas downstream.
func (st *SchemaTranslator) AfterResponse(ctx context.Context, resp *http.Response, events <-chan engine.StreamEvent,
	req *http.Request, chat *engine.ChatRequest) (<-chan engine.StreamEvent, error) {

	// Extract the mutation registry from chat metadata.
	registry, _ := chat.ToranaMeta[metaKeyMutations].(map[string][]string)

	out := make(chan engine.StreamEvent, 32)
	go func() {
		defer close(out)

		type toolBuf struct {
			name      string
			id        string
			index     int
			fragments []string
		}
		var current *toolBuf

		for ev := range events {
			switch {
			case ev.ToolCallStart != nil:
				// Flush any previous incomplete tool (shouldn't happen, but safe).
				current = &toolBuf{
					name:  ev.ToolCallStart.Name,
					id:    ev.ToolCallStart.ID,
					index: ev.ToolCallStart.Index,
				}
				// Suppress — we'll emit after processing.

			case ev.ToolCallDelta != nil && current != nil:
				current.fragments = append(current.fragments, ev.ToolCallDelta.ArgumentsDelta)
				// Suppress — buffering for assembly.

			case ev.ToolCallEnd != nil && current != nil:
				assembled := strings.Join(current.fragments, "")


				// Extract intent + reverse mutations.
				processed, intent := ReverseTranslate(current.name, assembled, registry)
				if intent != "" {
					log.Printf("[schema-translator] extracted intent for %s/%s: %q",
						current.name, current.id, intent)
					st.IntentCache.Store(current.id, intent)
				}

				// Emit ToolCallStart.
				out <- engine.StreamEvent{
					ToolCallStart: &engine.ToolCallStart{
						Index: current.index,
						ID:    current.id,
						Name:  current.name,
					},
				}
				// Emit processed arguments as a single delta.
				out <- engine.StreamEvent{
					ToolCallDelta: &engine.ToolCallDelta{
						Index:          current.index,
						ArgumentsDelta: processed,
					},
				}
				// Emit ToolCallEnd.
				out <- ev
				current = nil

			default:
				// Pass through text, thinking, finish, errors unchanged.
				out <- ev
			}
		}
	}()
	return out, nil
}

// ==========================================================================
// Schema translation (outbound: harness → LLM)
// ==========================================================================

// translateSchema recursively walks a JSON Schema object, converting any
// open-ended maps (additionalProperties) into arrays of {key, value} pairs.
// Returns the dotted paths of all mutated fields (e.g. "env", "headers.auth").
func translateSchema(toolName string, schema map[string]any, path string) []string {
	var mutations []string

	// Detect implicit open maps BEFORE we lock them.
	props, hasProps := schema["properties"].(map[string]any)
	_, hasExplicitAP := schema["additionalProperties"]
	schemaType, _ := schema["type"].(string)

	if schemaType == "object" && !hasProps && !hasExplicitAP {
		// No properties + no explicit additionalProperties → implicit open map.
		// Convert to KV array instead of locking it with zero allowed keys.
		convertToKVArray(schema, "string")
		mutations = append(mutations, path)
		log.Printf("[schema-translator] %s: converted implicit open map at %s to KV array",
			toolName, path)
		return mutations
	}

	// Enforce additionalProperties: false for objects that have explicit properties.
	schema["additionalProperties"] = false

	if !hasProps {
		return mutations
	}

	for propName, propVal := range props {
		propSchema, ok := propVal.(map[string]any)
		if !ok {
			continue
		}

		currentPath := joinPath(path, propName)

		// Detect implicit open maps BEFORE touching additionalProperties.
		propType, _ := propSchema["type"].(string)
		_, propHasProps := propSchema["properties"].(map[string]any)
		_, propHasAP := propSchema["additionalProperties"]

		// Case 1: Implicit open map — object type, no properties, no additionalProperties.
		if propType == "object" && !propHasProps && !propHasAP {
			convertToKVArray(propSchema, "string")
			mutations = append(mutations, currentPath)
			log.Printf("[schema-translator] %s: converted implicit open map at %s to KV array",
				toolName, currentPath)
			continue
		}

		// Case 2: Explicit open map — has additionalProperties=true or {type:...}.
		if hasAdditionalProperties(propSchema) {
			valueType := extractAdditionalPropertiesType(propSchema)
			convertToKVArray(propSchema, valueType)
			mutations = append(mutations, currentPath)
			log.Printf("[schema-translator] %s: converted %s from map to KV array (value type: %s)",
				toolName, currentPath, valueType)
			continue
		}

		// Case 3: Normal object with properties — enforce additionalProperties: false.
		propSchema["additionalProperties"] = false

		// Recurse into nested objects.
		typeName, _ := propSchema["type"].(string)
		switch typeName {
		case "object":
			mutations = append(mutations, translateSchema(toolName, propSchema, currentPath)...)
		case "array":
			if items, ok := propSchema["items"].(map[string]any); ok {
				if itemType, _ := items["type"].(string); itemType == "object" {
					mutations = append(mutations, translateSchema(toolName, items, currentPath+"[]")...)
				}
			}
		}
	}

	return mutations
}

// hasAdditionalProperties returns true if the schema has additionalProperties
// set to a type constraint or true.
func hasAdditionalProperties(schema map[string]any) bool {
	ap, exists := schema["additionalProperties"]
	if !exists {
		return false
	}
	switch v := ap.(type) {
	case bool:
		return v // true = allow anything
	case map[string]any:
		return true // {type: "string"} etc.
	}
	return false
}

// extractAdditionalPropertiesType determines the value type for the KV array.
// additionalProperties: true → "string" (safe default)
// additionalProperties: {type: "string"} → "string"
// additionalProperties: {type: "number"} → "number"
// etc.
func extractAdditionalPropertiesType(schema map[string]any) string {
	ap := schema["additionalProperties"]
	switch v := ap.(type) {
	case map[string]any:
		if t, ok := v["type"].(string); ok {
			return t
		}
	}
	return "string" // default for untyped maps
}

// convertToKVArray mutates the schema in place from an open-ended map to
// a Strict-compliant array of {key, value} objects.
func convertToKVArray(schema map[string]any, valueType string) {
	// Preserve description for the converted field.
	desc := ""
	if d, ok := schema["description"].(string); ok {
		desc = d
	}

	// Wipe the schema and rebuild as array.
	for k := range schema {
		delete(schema, k)
	}

	schema["type"] = "array"
	schema["description"] = desc + " (as key-value pairs: [{\"key\": \"...\", \"value\": \"...\"}])"
	schema["items"] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "describe your goal for this tool call",
			},
			"value": map[string]any{
				"type":        valueType,
				"description": "describe your goal for this tool call",
			},
		},
		"additionalProperties": false,
		"required": []any{"key", "value"},
	}
}

// ToranaIntentField is injected into every tool schema. Named "i" to match
// the field that omp (oh-my-pi) uses, because models trained on omp interactions
// already know to populate "i" on every tool call. We update the description
// to guide the model toward specific, actionable intents.
const ToranaIntentField = "i"

// injectIntentParam ensures every tool has an "i" (concise intent) field
// with a description that guides the model toward specific, actionable intents.
// If the harness already has "i", we update its description. If not, we inject it.
func injectIntentParam(tool *engine.ToolDef) {
	if tool.Parameters == nil {
		tool.Parameters = make(map[string]any)
	}
	if tool.Parameters["type"] == nil {
		tool.Parameters["type"] = "object"
	}

	props, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		props = make(map[string]any)
		tool.Parameters["properties"] = props
	}

	// Inject or update the "i" field with our enhanced description.
	props[ToranaIntentField] = map[string]any{
		"type":        "string",
		"description": "what you intend to accomplish: the question you are answering or the information you need",
	}

	// Add to required array.
	requiredRaw := tool.Parameters["required"]
	var required []any
	switch v := requiredRaw.(type) {
	case []any:
		required = v
	case []string:
		for _, s := range v {
			required = append(required, s)
		}
	default:
		required = make([]any, 0)
	}

	// Check if already present.
	for _, r := range required {
		if s, ok := r.(string); ok && s == ToranaIntentField {
			return
		}
	}

	required = append(required, ToranaIntentField)
	tool.Parameters["required"] = required
	tool.Parameters["additionalProperties"] = false
}

// ==========================================================================
// System prompt injection
// ==========================================================================

// injectSystemPrompt appends a targeted addendum to the system message that
// tells the model what we expect in the "i" field on tool calls. We append
// rather than replace to avoid disrupting the harness's own prompt.
func injectSystemPrompt(chat *engine.ChatRequest) {
	const addendum = "\n\nWhen populating the \"i\" field on any tool call, do not describe the " +
		"action you are taking (e.g. \"Read go.mod\" or \"List files\"). " +
		"Instead state the underlying question you are trying to answer or " +
		"decision you are trying to make (e.g. \"Determine minimum required " +
		"Go version for this module\" or \"Find which middleware handles " +
		"authentication\"). Action descriptions in \"i\" will be discarded " +
		"and are not useful."

	// Find existing system message and append.
	for i := range chat.Messages {
		if chat.Messages[i].Role == engine.RoleSystem {
			chat.Messages[i].Content += addendum
			return
		}
	}

	// No system message exists — inject as first message.
	chat.Messages = append([]engine.Message{{
		Role:    engine.RoleSystem,
		Content: "[SYSTEM]" + addendum,
	}}, chat.Messages...)
}

// ==========================================================================
// Reverse translation (inbound: LLM → harness)
// ==========================================================================

// ReverseTranslate processes assembled tool call arguments JSON:
// 1. Extracts intent from the "i" (concise intent) field
// 2. Reverses KV-array mutations back to objects
// Returns (sanitizedJSON, intentString).
func ReverseTranslate(toolName string, argsJSON string, registry map[string][]string) (string, string) {
	if argsJSON == "" || argsJSON == "{}" {
		return argsJSON, ""
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		log.Printf("[schema-translator] %s: failed to parse arguments: %v", toolName, err)
		return argsJSON, ""
	}

	// Extract intent from the "i" field.
	intent := ""
	if v, ok := args[ToranaIntentField].(string); ok && v != "" {
		intent = v
	}

	// 2. Reverse KV-array mutations.
	paths, ok := registry[toolName]
	if !ok {
		// Tool not in mutation registry — try heuristic KV reversal
		// for any property that looks like a [{key,value}] array.
		args = heuristicKVReversal(args)
		b, _ := json.Marshal(args)
		return string(b), intent
	}

	for _, path := range paths {
		reverseKVArrayAtPath(args, path)
	}

	b, err := json.Marshal(args)
	if err != nil {
		log.Printf("[schema-translator] %s: failed to marshal reversed args: %v", toolName, err)
		return argsJSON, intent
	}
	return string(b), intent
}

// reverseKVArrayAtPath traverses the dotted path into args and converts
// a [{key, value}] array back into a {key: value} object at that location.
func reverseKVArrayAtPath(args map[string]any, path string) {
	parts := strings.Split(path, ".")
	reverseAtPath(args, parts)
}

// reverseAtPath traverses parts into obj and performs the KV reversal
// at the terminal field. Handles array indices ([] suffix).
func reverseAtPath(obj map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}

	current := parts[0]
	rest := parts[1:]

	// Handle array-of-objects path (e.g. "items[]").
	if strings.HasSuffix(current, "[]") {
		fieldName := strings.TrimSuffix(current, "[]")
		arr, ok := obj[fieldName].([]any)
		if !ok {
			return
		}
		if len(rest) == 0 {
			// Terminal: reverse each item in the array.
			for i, item := range arr {
				if itemMap, ok := item.(map[string]any); ok {
					arr[i] = reverseKVObject(itemMap)
				}
			}
		} else {
			// Recurse into each array element.
			for _, item := range arr {
				if itemMap, ok := item.(map[string]any); ok {
					reverseAtPath(itemMap, rest)
				}
			}
		}
		return
	}

	// Scalar field.
	if len(rest) == 0 {
		// Terminal: reverse this field.
		if val, ok := obj[current]; ok {
			if arr, ok := val.([]any); ok {
				obj[current] = reverseKVArray(arr)
			}
		}
		return
	}

	// Intermediate: recurse into nested object.
	if nested, ok := obj[current].(map[string]any); ok {
		reverseAtPath(nested, rest)
	}
}

// reverseKVArray converts a [{key, value}] array into a {key: value} map.
func reverseKVArray(arr []any) map[string]any {
	result := make(map[string]any, len(arr))
	for _, item := range arr {
		kv, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := kv["key"].(string)
		if key == "" {
			continue
		}
		if val, exists := kv["value"]; exists {
			result[key] = val
		}
	}
	return result
}

// reverseKVObject reverses all KV-array fields in a map (used for array items).
func reverseKVObject(obj map[string]any) map[string]any {
	for k, v := range obj {
		if arr, ok := v.([]any); ok {
			// Check if this looks like a KV array.
			if isKVArray(arr) {
				obj[k] = reverseKVArray(arr)
			}
		}
	}
	return obj
}

// heuristicKVReversal scans all top-level properties and reverses
// any that look like KV arrays. Used as a fallback when a tool
// is not in the mutation registry.
func heuristicKVReversal(args map[string]any) map[string]any {
	for k, v := range args {
		switch val := v.(type) {
		case map[string]any:
			args[k] = heuristicKVReversal(val)
		case []any:
			if isKVArray(val) {
				args[k] = reverseKVArray(val)
			} else {
				for i, item := range val {
					if m, ok := item.(map[string]any); ok {
						val[i] = heuristicKVReversal(m)
					}
				}
			}
		}
	}
	return args
}

// isKVArray heuristically checks if an array looks like a KV-pair array.
func isKVArray(arr []any) bool {
	if len(arr) == 0 {
		return false
	}
	if m, ok := arr[0].(map[string]any); ok {
		_, hasKey := m["key"]
		_, hasValue := m["value"]
		return hasKey && hasValue
	}
	return false
}

// ==========================================================================
// Helpers
// ==========================================================================

func joinPath(base, segment string) string {
	if base == "" {
		return segment
	}
	return base + "." + segment
}

// Compile-time guard.
var _ engine.RequestHook = (*SchemaTranslator)(nil)
var _ engine.ResponseHook = (*SchemaTranslator)(nil)
