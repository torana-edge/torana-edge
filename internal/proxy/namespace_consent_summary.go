package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var consentIdentifier = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,32}$`)
var consentAlphanumericRun = regexp.MustCompile(`[A-Za-z0-9]{12,}`)

func consentCredentialPrefix(text string) bool {
	for _, prefix := range []string{"sk-", "ghp_", "github_pat_", "AKIA", "AIza", "xox", "sk_live_", "rk_live_", "glpat-", "hf_", "eyJ"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func consentHumanIdentifier(text string) bool {
	if !consentIdentifier.MatchString(text) {
		return false
	}
	digits, hexCharacters := 0, 0
	for _, ch := range text {
		if ch >= '0' && ch <= '9' {
			digits++
		}
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F' {
			hexCharacters++
		}
	}
	// Vendor model identifiers often end with a release date. Exempt only a
	// valid calendar date from the digit budget, not arbitrary numeric tails.
	if len(text) >= 10 && text[len(text)-9] == '-' {
		date := text[len(text)-8:]
		if strings.HasPrefix(date, "20") {
			if _, err := time.Parse("20060102", date); err == nil {
				digits -= 8
			}
		}
	}
	if digits > 8 || len(text) >= 16 && hexCharacters*4 >= len(text)*3 {
		return false
	}
	for _, run := range consentAlphanumericRun.FindAllString(text, -1) {
		upper, lower := false, false
		runDigits, seenDigit, letterAfterDigit := 0, false, false
		for _, ch := range run {
			upper = upper || ch >= 'A' && ch <= 'Z'
			lower = lower || ch >= 'a' && ch <= 'z'
			if ch >= '0' && ch <= '9' {
				runDigits++
				seenDigit = true
			} else if seenDigit {
				letterAfterDigit = true
			}
		}
		if len(run) >= 16 && upper && lower || runDigits >= 3 && letterAfterDigit {
			return false
		}
	}
	return true
}

func consentSensitive(schema map[string]any, key string) bool {
	lower := strings.ToLower(key)
	return schema["writeOnly"] == true || schema["sensitive"] == true || schema["format"] == "password" || schema["$ref"] != nil || schema["allOf"] != nil || schema["anyOf"] != nil || schema["oneOf"] != nil || strings.Contains(lower, "key") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password")
}

// Only the operator sees this summary. Print short identifiers, not arbitrary
// free text. Schema/key sensitivity takes precedence even for scalar values.
func consentScalar(value any, schema map[string]any, key string) string {
	if consentSensitive(schema, key) {
		return "[redacted]"
	}
	switch value.(type) {
	case nil, bool, float64:
		raw, _ := json.Marshal(value)
		return string(raw)
	case string:
		text := value.(string)
		if consentCredentialPrefix(text) {
			return "[redacted]"
		}
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
		if consentHumanIdentifier(text) {
			raw, _ := json.Marshal(text)
			return string(raw)
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
	parts := []string{}
	if err := appendConsentLeaves(&parts, "", "", before, operation.Source == "standard", after, true, schema, operation.Source == "standard", 0); err != nil {
		return "", err
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

func appendConsentLeaves(parts *[]string, path, key string, old any, was bool, value any, present bool, schema map[string]any, diff bool, depth int) error {
	if depth > 16 || len(*parts) > 64 {
		return errors.New("operation needs a full operator review")
	}
	if diff && was == present && reflect.DeepEqual(old, value) {
		return nil
	}
	oldObject, oldIsObject := old.(map[string]any)
	newObject, newIsObject := value.(map[string]any)
	if !consentSensitive(schema, key) && (oldIsObject || newIsObject) && (!was || oldIsObject) && (!present || newIsObject) {
		keys := map[string]bool{}
		for name := range oldObject {
			keys[name] = true
		}
		for name := range newObject {
			keys[name] = true
		}
		ordered := []string{}
		for name := range keys {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		properties, _ := schema["properties"].(map[string]any)
		for _, name := range ordered {
			before, existsBefore := oldObject[name]
			after, existsAfter := newObject[name]
			field, ok := properties[name].(map[string]any)
			if !ok {
				field, _ = schema["additionalProperties"].(map[string]any)
			}
			pointer := path + "/" + strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
			if err := appendConsentLeaves(parts, pointer, name, before, existsBefore, after, existsAfter, field, diff, depth+1); err != nil {
				return err
			}
		}
		if len(ordered) > 0 {
			return nil
		}
	}
	oldArray, oldIsArray := old.([]any)
	newArray, newIsArray := value.([]any)
	if !consentSensitive(schema, key) && (oldIsArray || newIsArray) && (!was || oldIsArray) && (!present || newIsArray) {
		count := max(len(oldArray), len(newArray))
		for i := 0; i < count; i++ {
			var before, after any
			if i < len(oldArray) {
				before = oldArray[i]
			}
			if i < len(newArray) {
				after = newArray[i]
			}
			field, _ := schema["items"].(map[string]any)
			if prefix, ok := schema["prefixItems"].([]any); ok && i < len(prefix) {
				field, _ = prefix[i].(map[string]any)
			}
			if err := appendConsentLeaves(parts, path+"/"+strconv.Itoa(i), key, before, i < len(oldArray), after, i < len(newArray), field, diff, depth+1); err != nil {
				return err
			}
		}
		if count > 0 {
			return nil
		}
	}
	label, _ := json.Marshal(path)
	to := "[removed]"
	if present {
		to = consentScalar(value, schema, key)
	}
	if diff {
		from := "[absent]"
		if was {
			from = consentScalar(old, schema, key)
		}
		*parts = append(*parts, string(label)+": "+from+" -> "+to)
	} else {
		*parts = append(*parts, string(label)+"="+to)
	}
	return nil
}
