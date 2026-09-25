package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/torana-edge/torana-edge/internal/annotate"
)

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
	for _, path := range assistantTextPaths(document, shape) {
		start, end, ok := rawJSONSpanAt(body, path...)
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

func assistantTextPaths(document any, shape string) [][]any {
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
	var paths [][]any
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
			paths = append(paths, append(base, contentField))
			continue
		}
		for partIndex, rawPart := range array(item[contentField]) {
			part := object(rawPart)
			if _, ok := part["text"].(string); !ok {
				continue
			}
			paths = append(paths, append(append([]any{}, base...), contentField, partIndex, "text"))
		}
	}
	return paths
}
