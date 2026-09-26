// Package effort writes portable, operator-declared reasoning effort in the
// native upstream request dialect. It never guesses a model's supported levels.
package effort

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/provider"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

var names = map[pb.Effort]string{
	pb.Effort_EFFORT_MINIMAL: "minimal", pb.Effort_EFFORT_LOW: "low",
	pb.Effort_EFFORT_MEDIUM: "medium", pb.Effort_EFFORT_HIGH: "high",
	pb.Effort_EFFORT_XHIGH: "xhigh", pb.Effort_EFFORT_MAX: "max",
}

var ranks = map[string]int{
	"minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// Apply returns the resulting body, selected portable level, and outcome.
// Omitted means the declaration offers no safe native setting; unchanged
// means no effort was requested. Clamping chooses the nearer supported level,
// preferring the lower one on a tie.
func Apply(body []byte, shape string, requested pb.Effort, model provider.ModelCapabilitiesConfig) ([]byte, string, string, error) {
	if requested == pb.Effort_EFFORT_UNSPECIFIED {
		return body, "", "unchanged", nil
	}
	want, valid := names[requested]
	if !valid {
		return nil, "", "", fmt.Errorf("invalid portable effort")
	}
	if model.Effort == nil || len(model.Effort.Levels) == 0 {
		return body, "", "omitted", nil
	}
	selected := ""
	bestDistance := 100
	for _, level := range model.Effort.Levels {
		distance := ranks[level] - ranks[want]
		if distance < 0 {
			distance = -distance
		}
		if selected == "" || distance < bestDistance || distance == bestDistance && ranks[level] < ranks[selected] {
			selected, bestDistance = level, distance
		}
	}
	if selected == "" {
		return body, "", "omitted", nil
	}
	var path []string
	var native any = selected
	switch shape {
	case "anthropic":
		path = []string{"output_config", "effort"}
	case "openai-chat":
		path = []string{"reasoning_effort"}
	case "openai-responses":
		path = []string{"reasoning", "effort"}
	case "gemini", "gemini-codeassist":
		mapping, found := model.Effort.Gemini[selected]
		if !found {
			return body, "", "omitted", nil
		}
		path = []string{"generationConfig", "thinkingConfig"}
		if shape == "gemini-codeassist" {
			path = append([]string{"request"}, path...)
		}
		if mapping.ThinkingLevel != "" {
			path = append(path, "thinkingLevel")
			native = mapping.ThinkingLevel
		} else if mapping.ThinkingBudget != nil {
			path = append(path, "thinkingBudget")
			native = *mapping.ThinkingBudget
		} else {
			return body, "", "omitted", nil
		}
	default:
		return body, "", "omitted", nil
	}
	value, err := json.Marshal(native)
	if err != nil {
		return nil, "", "", err
	}
	updated, err := setNested(body, path, value)
	if err != nil {
		return nil, "", "", err
	}
	if shape == "gemini" || shape == "gemini-codeassist" {
		other := "thinkingBudget"
		if path[len(path)-1] == "thinkingBudget" {
			other = "thinkingLevel"
		}
		updated, err = deleteNested(updated, append(append([]string(nil), path[:len(path)-1]...), other))
		if err != nil {
			return nil, "", "", err
		}
	}
	status := "applied"
	if selected != want {
		status = "clamped"
	}
	return updated, selected, status, nil
}

func setNested(body []byte, path []string, value json.RawMessage) ([]byte, error) {
	if len(path) == 0 {
		return value, nil
	}
	var object map[string]json.RawMessage
	if len(body) == 0 {
		object = map[string]json.RawMessage{}
	} else if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, fmt.Errorf("effort container %q is not an object", path[0])
	}
	child, err := setNested(object[path[0]], path[1:], value)
	if err != nil {
		return nil, err
	}
	object[path[0]] = child
	return json.Marshal(object)
}

func deleteNested(body []byte, path []string) ([]byte, error) {
	if len(path) == 0 {
		return body, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, fmt.Errorf("effort container %q is not an object", path[0])
	}
	if len(path) == 1 {
		delete(object, path[0])
	} else if child, exists := object[path[0]]; exists {
		updated, err := deleteNested(child, path[1:])
		if err != nil {
			return nil, err
		}
		object[path[0]] = updated
	}
	return json.Marshal(object)
}
