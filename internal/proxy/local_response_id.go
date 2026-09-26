package proxy

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/annotate"
)

// rewriteLocalPreviousResponseID recovers the last provider-owned Responses
// ID before forwarding a request that follows a Torana-local reply. It edits
// only the ID value (or deletes that member when there was no provider reply).
func rewriteLocalPreviousResponseID(body []byte, signer annotate.Signer) ([]byte, bool, error) {
	start, end, ok := rawJSONSpanAt(body, "previous_response_id")
	if !ok {
		return body, false, nil
	}
	var id string
	if err := json.Unmarshal(body[start:end], &id); err != nil {
		return body, false, nil // null and other invalid forms belong to the adapter
	}
	providerID, recognized, err := annotate.DecodeLocalResponseID(signer, id)
	if err != nil || !recognized {
		return body, recognized, err
	}
	if providerID == "" {
		memberStart, memberEnd, err := topLevelMemberDeletion(body, "previous_response_id")
		if err != nil {
			return nil, false, err
		}
		return spliceBytes(body, memberStart, memberEnd, nil), true, nil
	}
	replacement, _ := json.Marshal(providerID)
	return spliceBytes(body, start, end, replacement), true, nil
}

func topLevelMemberDeletion(body []byte, name string) (int, int, error) {
	i := 0
	skipWS(body, &i)
	if i >= len(body) || body[i] != '{' {
		return 0, 0, fmt.Errorf("request is not a JSON object")
	}
	i++
	previousComma := -1
	for i < len(body) {
		skipWS(body, &i)
		if i < len(body) && body[i] == '}' {
			break
		}
		keyStart := i
		keyEnd := scanStringEnd(body, i)
		if keyEnd < 0 {
			return 0, 0, fmt.Errorf("invalid JSON object key")
		}
		key, err := unescapeJSONString(body[keyStart:keyEnd])
		if err != nil {
			return 0, 0, err
		}
		i = keyEnd
		skipWS(body, &i)
		if i >= len(body) || body[i] != ':' {
			return 0, 0, fmt.Errorf("invalid JSON object member")
		}
		i++
		skipWS(body, &i)
		valueEnd := skipValue(body, i)
		i = valueEnd
		skipWS(body, &i)
		comma := -1
		if i < len(body) && body[i] == ',' {
			comma = i
		}
		if key == name {
			if comma >= 0 {
				return keyStart, comma + 1, nil
			}
			if previousComma >= 0 {
				return previousComma, valueEnd, nil
			}
			return keyStart, valueEnd, nil
		}
		if comma < 0 {
			break
		}
		previousComma = comma
		i = comma + 1
	}
	return 0, 0, fmt.Errorf("JSON object member is missing")
}
