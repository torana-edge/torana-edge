package format

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// UnsupportedOutputFormatError means a valid portable constraint cannot be
// represented by the selected provider without weakening its guarantees.
// The proxy maps it to a client 400 rather than treating it as a host fault.
type UnsupportedOutputFormatError struct{ Reason string }

func (e *UnsupportedOutputFormatError) Error() string { return "output format: " + e.Reason }

func unsupportedOutputFormat(reason string) error {
	return &UnsupportedOutputFormatError{Reason: reason}
}

// Provider mappings follow the respective structured-output APIs:
// https://platform.openai.com/docs/guides/structured-outputs
// https://platform.claude.com/docs/en/build-with-claude/structured-outputs
// https://ai.google.dev/gemini-api/docs/structured-output
func outputPath(chat *engine.ChatRequest, provider string) []string {
	switch provider {
	case "openai":
		if chat.OpenAIVariant == engine.OpenAIResponses {
			return []string{"text", "format"}
		}
		return []string{"response_format"}
	case "anthropic":
		return []string{"output_config", "format"}
	case "gemini":
		if chat.CodeAssist {
			return []string{"request", "generationConfig"}
		}
		return []string{"generationConfig"}
	}
	return nil
}

func rawAt(obj engine.OptionalJSONObject, keys []string) (json.RawMessage, error) {
	if obj.IsAbsent() {
		return nil, nil
	}
	if len(keys) == 0 {
		return obj.Bytes(), nil
	}
	members, _, err := obj.DecodeObject()
	if err != nil {
		return nil, err
	}
	raw, ok := members[keys[0]]
	if !ok {
		return nil, nil
	}
	if len(keys) == 1 {
		return raw, nil
	}
	child, err := engine.ParseOptionalJSONObject(raw)
	if err != nil {
		return nil, err
	}
	return rawAt(child, keys[1:])
}
func setRawAt(obj engine.OptionalJSONObject, keys []string, raw []byte) (engine.OptionalJSONObject, error) {
	if len(keys) == 1 {
		if raw == nil {
			return obj.DeleteMember(keys[0])
		}
		return obj.SetMember(keys[0], raw)
	}
	childRaw, err := rawAt(obj, keys[:1])
	if err != nil {
		return obj, err
	}
	if childRaw == nil && raw == nil {
		return obj, nil
	}
	child, err := engine.ParseOptionalJSONObject(childRaw)
	if err != nil {
		return obj, err
	}
	child, err = setRawAt(child, keys[1:], raw)
	if err != nil {
		return obj, err
	}
	// Preserve explicit parent objects: siblings are opaque provider data and
	// their lexical representation must survive removal of the canonical leaf.
	return obj.SetMember(keys[0], child.Bytes())
}

// ExtractOutputFormat removes recognized constraints from provider extras so
// typed output_format is their sole authority. Unknown provider modes remain
// opaque; setting a typed constraint over one is refused at marshal time.
func ExtractOutputFormat(chat *engine.ChatRequest, provider string) error {
	keys := outputPath(chat, provider)
	raw, err := rawAt(chat.ProviderExtensions, keys)
	if err != nil {
		return fmt.Errorf("output format: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("output format must be an object")
	}
	f := &pb.OutputFormat{}
	remaining := json.RawMessage(nil)
	if provider == "gemini" {
		var mime string
		if r, ok := fields["responseMimeType"]; ok {
			if err = json.Unmarshal(r, &mime); err != nil {
				return fmt.Errorf("response MIME type must be a string")
			}
		}
		if _, legacy := fields["responseSchema"]; legacy {
			return nil
		} // Different schema dialect remains opaque.
		if mime != "application/json" {
			return nil
		}
		f.Mode = pb.OutputFormat_MODE_JSON_OBJECT
		if schema, ok := fields["responseJsonSchema"]; ok {
			f.Mode = pb.OutputFormat_MODE_JSON_SCHEMA
			f.Name = "response"
			f.SchemaJson = append([]byte(nil), schema...)
		}
		obj, err := engine.ParseOptionalJSONObject(raw)
		if err != nil {
			return err
		}
		obj, err = obj.WithoutMembers("responseMimeType", "responseJsonSchema")
		if err != nil {
			return err
		}
		remaining = obj.Bytes()
	} else {
		typeRaw, ok := fields["type"]
		if !ok {
			return nil // An unrecognized provider object remains byte-for-byte opaque.
		}
		var kind string
		if err = json.Unmarshal(typeRaw, &kind); err != nil {
			return fmt.Errorf("output format type must be a string")
		}
		// Canonicalization must not erase provider options absent from the IR.
		// Keep the whole constraint opaque if any unmodeled member is present.
		allowed := []string{"type"}
		if kind == "json_schema" {
			if provider == "openai" && chat.OpenAIVariant != engine.OpenAIResponses {
				allowed = append(allowed, "json_schema")
			} else {
				allowed = append(allowed, "name", "schema", "strict")
			}
		}
		if hasUnmodeledOutputFields(fields, allowed...) {
			return nil
		}
		switch kind {
		case "text":
			f.Mode = pb.OutputFormat_MODE_TEXT
		case "json_object":
			f.Mode = pb.OutputFormat_MODE_JSON_OBJECT
		case "json_schema":
			f.Mode = pb.OutputFormat_MODE_JSON_SCHEMA
			schemaFields := fields
			if provider == "openai" && chat.OpenAIVariant != engine.OpenAIResponses {
				schemaFields = nil
				if err = json.Unmarshal(fields["json_schema"], &schemaFields); err != nil || schemaFields == nil {
					return fmt.Errorf("json_schema must be an object")
				}
			}
			if hasUnmodeledOutputFields(schemaFields, "type", "name", "schema", "strict") {
				return nil
			}
			f.Name = "response"
			if name, ok := schemaFields["name"]; ok {
				if err = json.Unmarshal(name, &f.Name); err != nil {
					return fmt.Errorf("schema name must be a string")
				}
			}
			f.SchemaJson = append([]byte(nil), schemaFields["schema"]...)
			if strict, ok := schemaFields["strict"]; ok {
				var v bool
				if string(strict) == "null" || json.Unmarshal(strict, &v) != nil {
					return fmt.Errorf("schema strict must be boolean")
				}
				f.Strict = &v
			}
		default:
			return nil
		}
	}
	if err = f.Validate(); err != nil {
		return err
	}
	schema, err := engine.ParseOptionalJSONObject(f.SchemaJson)
	if err != nil {
		return err
	}
	chat.OutputFormat = &engine.OutputFormat{Mode: int32(f.Mode), Name: f.Name, Schema: schema, Strict: f.Strict}
	chat.ProviderExtensions, err = setRawAt(chat.ProviderExtensions, keys, remaining)
	return err
}

func ValidateOutputFormatExtras(chat *engine.ChatRequest, provider string) error {
	if chat == nil {
		return fmt.Errorf("request is nil")
	}
	if chat.OutputFormat == nil {
		return nil
	}
	raw, err := rawAt(chat.ProviderExtensions, outputPath(chat, provider))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	if provider != "gemini" {
		return fmt.Errorf("output format duplicates a provider extension")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"responseMimeType", "responseJsonSchema", "responseSchema"} {
		if _, ok := fields[key]; ok {
			return fmt.Errorf("output format duplicates generationConfig.%s", key)
		}
	}
	return nil
}

func ApplyOutputFormat(raw []byte, chat *engine.ChatRequest, provider string) ([]byte, error) {
	if chat.OutputFormat == nil {
		return raw, nil
	}
	f := chat.OutputFormat
	wire := &pb.OutputFormat{Mode: pb.OutputFormat_Mode(f.Mode), Name: f.Name, SchemaJson: f.Schema.Bytes(), Strict: f.Strict}
	if err := wire.Validate(); err != nil {
		return nil, err
	}
	obj, err := engine.ParseOptionalJSONObject(raw)
	if err != nil {
		return nil, err
	}
	keys := outputPath(chat, provider)
	if len(keys) == 0 {
		return nil, unsupportedOutputFormat("provider does not support portable output constraints")
	}
	var encoded []byte
	switch provider {
	case "gemini":
		if f.Strict != nil {
			return nil, unsupportedOutputFormat("Gemini does not support the explicit strict option")
		}
		existing, err := rawAt(obj, keys)
		if err != nil {
			return nil, err
		}
		gc, err := engine.ParseOptionalJSONObject(existing)
		if err != nil {
			return nil, err
		}
		if f.Mode != int32(pb.OutputFormat_MODE_TEXT) {
			gc, err = gc.SetMember("responseMimeType", []byte(`"application/json"`))
			if err != nil {
				return nil, err
			}
		}
		if f.Mode == int32(pb.OutputFormat_MODE_JSON_SCHEMA) {
			gc, err = gc.SetMember("responseJsonSchema", f.Schema.Bytes())
			if err != nil {
				return nil, err
			}
		}
		if gc.IsAbsent() {
			return raw, nil
		}
		encoded = gc.Bytes()
	case "anthropic":
		if f.Mode == int32(pb.OutputFormat_MODE_TEXT) {
			return raw, nil
		}
		if f.Mode != int32(pb.OutputFormat_MODE_JSON_SCHEMA) || f.Strict != nil {
			return nil, unsupportedOutputFormat("Anthropic supports schema output without an explicit strict option")
		}
		encoded, err = json.Marshal(map[string]any{"type": "json_schema", "schema": json.RawMessage(f.Schema.Bytes())})
	case "openai":
		mode := []string{"text", "json_object", "json_schema"}[f.Mode]
		fields := map[string]any{"type": mode}
		if f.Mode == int32(pb.OutputFormat_MODE_JSON_SCHEMA) {
			schema := map[string]any{"name": f.Name, "schema": json.RawMessage(f.Schema.Bytes())}
			if f.Strict != nil {
				schema["strict"] = *f.Strict
			}
			if chat.OpenAIVariant == engine.OpenAIResponses {
				for k, v := range schema {
					fields[k] = v
				}
			} else {
				fields["json_schema"] = schema
			}
		}
		encoded, err = json.Marshal(fields)
	}
	if err != nil {
		return nil, err
	}
	obj, err = setRawAt(obj, keys, encoded)
	if err != nil {
		return nil, err
	}
	return obj.Bytes(), nil
}

func hasUnmodeledOutputFields(fields map[string]json.RawMessage, allowed ...string) bool {
	for name := range fields {
		known := false
		for _, key := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			return true
		}
	}
	return false
}
