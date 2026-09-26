package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
)

// PlanClaudeHooks edits only explicitly owned HTTP hook groups in a settings
// file, never Claude's live .claude.json state or its permission settings.
func PlanClaudeHooks(path, origin string, pre, teardown bool) (*FilePlan, error) {
	client, err := controlclient.New(origin, time.Second)
	if err != nil {
		return nil, err
	}
	origin = client.Address()
	client.Close()
	plan, err := planSnapshot(path)
	if err != nil {
		return nil, err
	}
	plan.Teardown = teardown
	plan.edit, err = editClaudeHooks(plan.before, plan.ownership, origin, pre, teardown)
	if err != nil {
		return nil, err
	}
	plan.Changed = plan.edit.Changed
	return plan, nil
}

func editClaudeHooks(current, ownership []byte, origin string, pre, teardown bool) (Edit, error) {
	root := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(current)) != 0 && (!uniqueJSONKeys(current) || json.Unmarshal(current, &root) != nil || root == nil) {
		return Edit{}, fmt.Errorf("Claude settings must be an object without duplicate keys")
	}
	groups := map[string][]json.RawMessage{}
	if raw, ok := root["hooks"]; ok && (json.Unmarshal(raw, &groups) != nil || groups == nil) {
		return Edit{}, fmt.Errorf("Claude hooks must be an object of hook groups")
	}
	owned := map[string]string{}
	if len(ownership) != 0 && (json.Unmarshal(ownership, &owned) != nil || owned == nil) {
		return Edit{}, fmt.Errorf("Torana hook ownership record is invalid")
	}
	for event, digest := range owned {
		if event != "Stop" && event != "PostModelSwitch" && event != "PreModelSwitch" || digest == "" {
			return Edit{}, fmt.Errorf("Torana hook ownership record is invalid")
		}
		found := -1
		for i, group := range groups[event] {
			if entryDigest(group) == digest {
				if found >= 0 {
					return Edit{}, fmt.Errorf("owned Torana hook is duplicated; review %s manually", event)
				}
				found = i
			}
		}
		if found < 0 {
			return Edit{}, fmt.Errorf("owned Torana hook is missing or edited; review %s manually", event)
		}
		groups[event] = append(groups[event][:found], groups[event][found+1:]...)
		if len(groups[event]) == 0 {
			delete(groups, event)
		}
	}
	newOwned := map[string]string{}
	if !teardown {
		events := map[string]string{"Stop": "stop", "PostModelSwitch": "post-model-switch"}
		if pre {
			events["PreModelSwitch"] = "pre-model-switch"
		}
		for event, path := range events {
			timeout := 3
			if event == "PreModelSwitch" {
				timeout = 1
			}
			group, _ := json.Marshal(map[string]any{"hooks": []any{map[string]any{
				"type": "http", "url": strings.TrimRight(origin, "/") + "/_torana/hooks/claude-code/" + path,
				"timeout":        timeout,
				"headers":        map[string]string{"Authorization": "Bearer $TORANA_MCP_TOKEN", "X-Torana-Local-Request": "1"},
				"allowedEnvVars": []string{"TORANA_MCP_TOKEN"},
			}}})
			digest := entryDigest(group)
			identical := false
			for _, existing := range groups[event] {
				if entryDigest(existing) == digest {
					identical = true
				}
			}
			// Do not claim an identical group the user installed themselves.
			if !identical {
				groups[event] = append(groups[event], group)
				newOwned[event] = digest
			}
		}
	}
	if len(groups) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"], _ = json.Marshal(groups)
	}
	after, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return Edit{}, err
	}
	newOwnership, _ := json.Marshal(newOwned)
	changed := entryDigest(after) != entryDigest(current) || len(owned) != len(newOwned)
	if !changed {
		return Edit{Content: current, Ownership: ownership}, nil
	}
	return Edit{Content: append(after, '\n'), Ownership: newOwnership, Changed: true}, nil
}
