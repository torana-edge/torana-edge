package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/torana-edge/torana-edge/internal/annotate"
)

type noticeTextSlot struct {
	path       []any
	removePath []any // a notice-only appended block/part/item
}

// stripSignedNoticesJSON removes only notices signed for this conversation
// from assistant history. All other wire bytes remain unchanged, preserving
// provider extensions and prompt-cache prefixes outside the notice strings.
func stripSignedNoticesJSON(body []byte, shape string, signer annotate.Signer, conversation string) ([]byte, bool, error) {
	if conversation == "" {
		return body, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, false, err
	}
	var splices []directiveSplice
	for _, slot := range assistantTextPaths(document, shape) {
		start, end, ok := rawJSONSpanAt(body, slot.path...)
		if !ok {
			return nil, false, fmt.Errorf("assistant notice text span is missing")
		}
		var original string
		if err := json.Unmarshal(body[start:end], &original); err != nil {
			return nil, false, err
		}
		stripped, changed, err := annotate.Strip(signer, conversation, original)
		if err != nil {
			return nil, false, err
		}
		if changed {
			if stripped == "" && len(slot.removePath) > 0 {
				deleteStart, deleteEnd, derr := jsonArrayElementDeletion(body, slot.removePath)
				if derr != nil {
					return nil, false, derr
				}
				splices = append(splices, directiveSplice{deleteStart, deleteEnd, nil})
				continue
			}
			replacement, _ := json.Marshal(stripped)
			splices = append(splices, directiveSplice{start, end, replacement})
		}
	}
	if len(splices) == 0 {
		return body, false, nil
	}
	sort.Slice(splices, func(i, j int) bool { return splices[i].start > splices[j].start })
	for _, splice := range splices {
		body = spliceBytes(body, splice.start, splice.end, splice.value)
	}
	return body, true, nil
}

func assistantTextPaths(document any, shape string) []noticeTextSlot {
	root := object(document)
	prefix := []any{}
	if shape == "gemini-codeassist" {
		root = object(root["request"])
		prefix = []any{"request"}
	}
	field := "messages"
	role := "assistant"
	if shape == "openai-responses" {
		field = "input"
	}
	if shape == "gemini" || shape == "gemini-codeassist" {
		field, role = "contents", "model"
	}
	var paths []noticeTextSlot
	for index, raw := range array(root[field]) {
		item := object(raw)
		if item["role"] != role {
			continue
		}
		base := append(append([]any{}, prefix...), field, index)
		contentField := "content"
		if field == "contents" {
			contentField = "parts"
		}
		if _, ok := item[contentField].(string); ok {
			paths = append(paths, noticeTextSlot{path: append(base, contentField)})
			continue
		}
		for partIndex, rawPart := range array(item[contentField]) {
			part := object(rawPart)
			if _, ok := part["text"].(string); !ok {
				continue
			}
			path := append(append([]any{}, base...), contentField, partIndex, "text")
			remove := append([]any{}, path[:len(path)-1]...)
			if shape == "openai-responses" && len(array(item[contentField])) == 1 {
				remove = base // notices are appended as separate response output items
			}
			paths = append(paths, noticeTextSlot{path: path, removePath: remove})
		}
	}
	return paths
}

func jsonArrayElementDeletion(body []byte, elementPath []any) (int, int, error) {
	if len(elementPath) < 1 {
		return 0, 0, fmt.Errorf("notice element path is empty")
	}
	index, ok := elementPath[len(elementPath)-1].(int)
	if !ok {
		return 0, 0, fmt.Errorf("notice element path has no array index")
	}
	parent := elementPath[:len(elementPath)-1]
	start, end, ok := rawJSONSpanAt(body, elementPath...)
	if !ok {
		return 0, 0, fmt.Errorf("notice element span is missing")
	}
	next := append(append([]any{}, parent...), index+1)
	if nextStart, _, exists := rawJSONSpanAt(body, next...); exists {
		return start, nextStart, nil
	}
	if index > 0 {
		prev := append(append([]any{}, parent...), index-1)
		_, prevEnd, exists := rawJSONSpanAt(body, prev...)
		if !exists {
			return 0, 0, fmt.Errorf("previous notice element span is missing")
		}
		return prevEnd, end, nil
	}
	return start, end, nil
}
