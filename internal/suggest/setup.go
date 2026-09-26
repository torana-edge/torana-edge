package suggest

import (
	"encoding/json"
	"fmt"
	"time"
)

// SetupHint is host-only and informational. Its durable cooldown is shared
// across conversations; dismissing it cannot trigger setup or a model change.
// A failed write may suppress a hint, but must never break model traffic.
func (s *Store) SetupHint(conversation, source string, turn uint64, now time.Time) (string, error) {
	var name string
	switch source {
	case "claude-code-session":
		name = "claude-code"
	case "codex-thread", "codex-session", "codex-client-thread":
		name = "codex"
	case "gemini-code-assist-session":
		name = "antigravity"
	default:
		return "", nil
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupCooldown == nil {
		s.setupCooldown = make(map[string]time.Time)
	}
	if s.setupSeen == nil {
		s.setupSeen = make(map[string]bool)
	}
	if s.setupSeen[conversation] || now.Before(s.setupCooldown[name]) {
		return "", nil
	}
	// Bound retries on unavailable storage as well as the normal cooldown.
	s.setupCooldown[name] = now.Add(time.Minute)
	current, _, _, err := s.read(conversation)
	if err != nil || current.SetupHintSeen {
		if err == nil {
			s.rememberSetupSeen(conversation)
		}
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	key := "setup-cooldown/" + name
	value, version, found, err := s.state.GetVersioned(stateNamespace, key)
	if err != nil {
		return "", err
	}
	var previous time.Time
	if found {
		if err := json.Unmarshal([]byte(value), &previous); err != nil {
			return "", fmt.Errorf("decode setup cooldown: %w", err)
		}
		if now.Before(previous.Add(7 * 24 * time.Hour)) {
			s.setupCooldown[name] = previous.Add(7 * 24 * time.Hour)
			return "", nil
		}
	}
	var expected *string
	if found {
		expected = &version
	}
	raw, err := json.Marshal(now.UTC())
	if err != nil {
		return "", err
	}
	claimed, _, err := s.state.CompareAndSet(stateNamespace, key, string(raw), expected)
	if err != nil || !claimed {
		return "", err
	}
	s.setupCooldown[name] = now.Add(7 * 24 * time.Hour)
	created := ""
	err = s.update(conversation, func(current *record) (bool, error) {
		if current.SetupHintSeen {
			return false, nil
		}
		current.SetupHintSeen = true
		if len(current.Suggestions) >= maxRecords {
			return true, nil
		}
		body := "Connect Torana's MCP server to inspect plugins and propose changes from your harness. See the Torana harness setup guide for generic stdio configuration."
		if name != "antigravity" {
			body = "Connect Torana's MCP server to inspect plugins and propose changes from your harness. Preview with torana harness setup " + name + " --dry-run, then run it without --dry-run to review and apply."
		}
		current.Suggestions = append(current.Suggestions, Suggestion{
			ID: id, Plugin: "torana", Conversation: conversation,
			Kind: "torana_setup", DedupeKey: "setup/" + name,
			Title: "Connect Torana to your harness", Body: body,
			Actions:   []Action{{ID: "dismiss", Label: "Dismiss"}},
			CreatedAt: now.UTC(), CreatedTurn: turn,
			ExpiresAfterUserTurns: defaultExpiry, Status: "pending",
		})
		created = id
		return true, nil
	})
	if err == nil {
		s.rememberSetupSeen(conversation)
	}
	return created, err
}

// Called under setupMu. Durable records remain authoritative after eviction.
func (s *Store) rememberSetupSeen(conversation string) {
	if len(s.setupSeen) >= 4096 {
		clear(s.setupSeen)
	}
	s.setupSeen[conversation] = true
}
