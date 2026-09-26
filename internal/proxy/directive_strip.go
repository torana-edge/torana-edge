package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/torana-edge/torana-edge/internal/directive"
)

type directiveTextSlot struct {
	path []any
	user int
}

type directiveSplice struct {
	start, end int
	value      []byte
}

// stripDirectiveText changes only JSON string values that the request adapter
// interprets as user-authored text. Every untouched byte remains verbatim,
// including provider extensions, number lexemes, field order, and escapes.
// The returned commands come only from the latest user message, not history.
func stripDirectiveText(body []byte, shape string, knownNamespace func(string) bool) ([]byte, []directive.Command, bool, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, false, err
	}
	slots := directiveTextSlots(document, shape)
	var splices []directiveSplice
	latestUser := latestDirectiveUserIndex(document, shape)
	var latest []directive.Command
	for _, slot := range slots {
		start, end, ok := rawJSONSpanAt(body, slot.path...)
		if !ok {
			return nil, nil, false, fmt.Errorf("directive text span is missing")
		}
		var original string
		if err := json.Unmarshal(body[start:end], &original); err != nil {
			return nil, nil, false, err
		}
		parsed := directive.ParseText(original, knownNamespace)
		if slot.user >= 0 && slot.user == latestUser {
			latest = append(latest, parsed.Commands...)
		}
		if !parsed.Changed {
			continue
		}
		replacement, err := json.Marshal(parsed.Text)
		if err != nil {
			return nil, nil, false, err
		}
		splices = append(splices, directiveSplice{start, end, replacement})
	}
	for i := 4; i < len(latest); i++ {
		latest[i].Known, latest[i].Reason = false, "too_many_directives"
	}
	if len(splices) == 0 {
		return body, latest, false, nil
	}
	sort.Slice(splices, func(i, j int) bool { return splices[i].start > splices[j].start })
	updated := body
	for _, splice := range splices {
		updated = spliceBytes(updated, splice.start, splice.end, splice.value)
	}
	return updated, latest, true, nil
}

// A command is new only when the final input item is a genuine human turn.
// A tool-result continuation can replay old user messages verbatim, but must
// not execute their directives again.
func latestDirectiveUserIndex(document any, shape string) int {
	root := object(document)
	if shape == "gemini-codeassist" {
		root = object(root["request"])
	}
	field := "messages"
	if shape == "openai-responses" {
		field = "input"
	} else if strings.HasPrefix(shape, "gemini") {
		field = "contents"
	}
	if shape == "openai-responses" {
		if _, ok := root[field].(string); ok {
			return 0
		}
	}
	items := array(root[field])
	if len(items) == 0 {
		return -1
	}
	last := object(items[len(items)-1])
	if last["role"] != "user" {
		return -1
	}
	parts := array(last["content"])
	if field == "contents" {
		parts = array(last["parts"])
	}
	for _, raw := range parts {
		part := object(raw)
		if part["type"] == "tool_result" || part["functionResponse"] != nil {
			return -1
		}
	}
	return len(items) - 1
}

func directiveTextSlots(document any, shape string) []directiveTextSlot {
	root, ok := document.(map[string]any)
	if !ok {
		return nil
	}
	prefix := []any{}
	if shape == "gemini-codeassist" {
		root, ok = root["request"].(map[string]any)
		if !ok {
			return nil
		}
		prefix = []any{"request"}
	}
	field := "messages"
	if strings.HasPrefix(shape, "gemini") {
		field = "contents"
	} else if shape == "openai-responses" {
		field = "input"
	}
	if shape == "openai-responses" {
		if _, isString := root[field].(string); isString {
			return []directiveTextSlot{{path: []any{"input"}, user: 0}}
		}
	}
	items, ok := root[field].([]any)
	if !ok {
		return nil
	}
	var slots []directiveTextSlot
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["role"] != "user" {
			continue
		}
		base := append(append([]any(nil), prefix...), field, index)
		contentField := "content"
		if strings.HasPrefix(shape, "gemini") {
			contentField = "parts"
		}
		if _, isString := item[contentField].(string); isString {
			slots = append(slots, directiveTextSlot{path: append(base, contentField), user: index})
			continue
		}
		parts, ok := item[contentField].([]any)
		if !ok {
			continue
		}
		userIndex := index
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if part["type"] == "tool_result" || part["functionResponse"] != nil {
				// A user-role carrier with a tool result is a continuation,
				// not a new instruction authored by the human.
				userIndex = -1
				break
			}
		}
		for partIndex, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if !strings.HasPrefix(shape, "gemini") {
				kind, _ := part["type"].(string)
				if kind != "text" && kind != "input_text" && kind != "output_text" {
					continue
				}
			}
			if _, isString := part["text"].(string); !isString {
				continue
			}
			path := append(append([]any(nil), base...), contentField, partIndex, "text")
			slots = append(slots, directiveTextSlot{path: path, user: userIndex})
		}
	}
	return slots
}
