package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Only the operator sees this summary. Free-form strings and containers are
// never printed: they can contain credentials even without a schema annotation.
func consentScalar(value any, schema map[string]any, key string) string {
	lower := strings.ToLower(key)
	if schema["writeOnly"] == true || schema["sensitive"] == true || schema["format"] == "password" || strings.Contains(lower, "key") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") {
		return "[redacted]"
	}
	switch value.(type) {
	case nil, bool, float64:
		raw, _ := json.Marshal(value)
		return string(raw)
	case string:
		if choices, ok := schema["enum"].([]any); ok {
			for _, choice := range choices {
				if reflect.DeepEqual(choice, value) {
					raw, _ := json.Marshal(value)
					if len(raw) <= 64 {
						return string(raw)
					}
				}
			}
		}
	}
	return "[redacted]"
}

func operationConsentSummary(entry namespaceEntry, operation namespaceOperation, intent sealedOperationIntent) (string, error) {
	if operation.Source == "standard" {
		switch operation.ID {
		case "_enable":
			return fmt.Sprintf("Enable plugin %s for all conversations.", entry.Name), nil
		case "_disable":
			return fmt.Sprintf("Disable plugin %s for all conversations.", entry.Name), nil
		}
	}
	var before, after, schema map[string]any
	if operation.Source == "standard" {
		_ = json.Unmarshal(intent.Before, &before)
		_ = json.Unmarshal(intent.After, &after)
		_ = json.Unmarshal(entry.ConfigSchema, &schema)
	} else {
		_ = json.Unmarshal(intent.Input, &after)
		if operation.Guest != nil {
			_ = json.Unmarshal(operation.Guest.InputSchema, &schema)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	keys := map[string]bool{}
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	ordered := []string{}
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	parts := []string{}
	for _, key := range ordered {
		old, was := before[key]
		value, present := after[key]
		if operation.Source == "standard" && was == present && reflect.DeepEqual(old, value) {
			continue
		}
		field, _ := properties[key].(map[string]any)
		label, _ := json.Marshal("/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"))
		if operation.Source != "standard" {
			parts = append(parts, string(label)+"="+consentScalar(value, field, key))
			continue
		}
		from, to := "[absent]", "[removed]"
		if was {
			from = consentScalar(old, field, key)
		}
		if present {
			to = consentScalar(value, field, key)
		}
		parts = append(parts, string(label)+": "+from+" -> "+to)
	}
	prefix := "Run " + entry.Name + "." + operation.ID
	if operation.Source == "standard" {
		prefix = "Change " + entry.Name + " settings for all conversations"
	}
	if len(parts) == 0 {
		return prefix + " (no input changes).", nil
	}
	summary := prefix + ": " + strings.Join(parts, "; ") + "."
	if len(summary) > 580 {
		return "", errors.New("operation needs a full operator review")
	}
	return summary, nil
}
