// Package suggest stores conversation-scoped, host-owned plugin suggestions.
// Confirmation codes never appear in the plugin-facing result.
package suggest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

const (
	stateNamespace = "@torana/suggestions" // no valid plugin manifest can claim this name
	maxRecords     = 128
	defaultExpiry  = 3
	codeAlphabet   = "23456789abcdefghjkmnpqrstuvwxyz"
)

var (
	ErrNotFound = errors.New("suggestion not found")
	ErrConflict = errors.New("suggestion state changed concurrently")
	ErrLimit    = errors.New("too many live suggestions in this conversation")
)

type State interface {
	GetVersioned(plugin, key string) (value, version string, found bool, err error)
	CompareAndSet(plugin, key, value string, expected *string) (applied bool, version string, err error)
}

type Store struct{ state State }

func New(state State) *Store { return &Store{state: state} }

type Action struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Suggestion struct {
	ID                    string    `json:"id"`
	Code                  string    `json:"code"`
	Plugin                string    `json:"plugin"`
	Conversation          string    `json:"conversation"`
	Kind                  string    `json:"kind"`
	DedupeKey             string    `json:"dedupe_key"`
	Title                 string    `json:"title"`
	Body                  string    `json:"body"`
	Actions               []Action  `json:"actions,omitempty"`
	CostUSD               *float64  `json:"cost_usd,omitempty"`
	HarnessTargetModel    string    `json:"harness_target_model,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	CreatedTurn           uint64    `json:"created_turn"`
	ExpiresAfterUserTurns uint32    `json:"expires_after_user_turns"`
	Status                string    `json:"status"`
	Via                   string    `json:"via,omitempty"`
	Action                string    `json:"action,omitempty"`
}

type record struct {
	Conversation      string       `json:"conversation"`
	UserTurns         uint64       `json:"user_turns"`
	LastUserSignature string       `json:"last_user_signature,omitempty"`
	NoticeDisabled    bool         `json:"notice_disabled,omitempty"`
	Suggestions       []Suggestion `json:"suggestions"`
}

// DisableNotices is durable and one-way for a conversation. If signed-marker
// removal fails, future replies must not add more notices to that history.
func (s *Store) DisableNotices(conversation string) error {
	return s.update(conversation, func(current *record) (bool, error) {
		if current.NoticeDisabled {
			return false, nil
		}
		current.NoticeDisabled = true
		return true, nil
	})
}

func (s *Store) NoticesDisabled(conversation string) (bool, error) {
	current, _, _, err := s.read(conversation)
	return current.NoticeDisabled, err
}

// ObserveUserTurn advances only when the latest genuine user message changes.
// Continuation requests pass the same signature; an empty signature means no
// user message could be identified and leaves the counter unchanged.
func (s *Store) ObserveUserTurn(conversation, signature string) (uint64, error) {
	if signature == "" {
		current, _, _, err := s.read(conversation)
		return current.UserTurns, err
	}
	var turn uint64
	err := s.update(conversation, func(current *record) (bool, error) {
		if current.LastUserSignature == signature {
			turn = current.UserTurns
			return false, nil
		}
		current.UserTurns++
		current.LastUserSignature = signature
		turn = current.UserTurns
		return true, nil
	})
	return turn, err
}

func conversationKey(conversation string) string {
	sum := sha256.Sum256([]byte(conversation))
	return "conversation/" + hex.EncodeToString(sum[:])
}

func (s *Store) read(conversation string) (record, string, bool, error) {
	if s == nil || s.state == nil || conversation == "" {
		return record{}, "", false, errors.New("suggestion store and conversation are required")
	}
	value, version, found, err := s.state.GetVersioned(stateNamespace, conversationKey(conversation))
	if err != nil || !found {
		return record{Conversation: conversation}, version, found, err
	}
	var current record
	if err := json.Unmarshal([]byte(value), &current); err != nil {
		return record{}, "", false, fmt.Errorf("decode suggestion state: %w", err)
	}
	if current.Conversation != conversation {
		return record{}, "", false, errors.New("suggestion conversation key collision")
	}
	return current, version, true, nil
}

func (s *Store) update(conversation string, change func(*record) (bool, error)) error {
	for attempt := 0; attempt < 8; attempt++ {
		current, version, found, err := s.read(conversation)
		if err != nil {
			return err
		}
		changed, err := change(&current)
		if err != nil || !changed {
			return err
		}
		value, err := json.Marshal(current)
		if err != nil {
			return err
		}
		var expected *string
		if found {
			expected = &version
		}
		applied, _, err := s.state.CompareAndSet(stateNamespace, conversationKey(conversation), string(value), expected)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
	}
	return ErrConflict
}

func randomID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "sg_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

func randomCode() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	var code [4]byte
	for i, b := range raw {
		code[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(code[:]), nil
}

func expire(current *record, turn uint64) bool {
	changed := false
	for i := range current.Suggestions {
		item := &current.Suggestions[i]
		if item.Status == "pending" && turn >= item.CreatedTurn && turn-item.CreatedTurn >= uint64(item.ExpiresAfterUserTurns) {
			item.Status = "expired"
			changed = true
		}
	}
	return changed
}

// Create refreshes matching live intent without changing its ID/code. A new
// intent supersedes the plugin's previous live suggestion in this conversation.
// Only the ID returns to the plugin; the host retains the code.
func (s *Store) Create(conversation, plugin string, turn uint64, args *pb.SuggestArgs) (string, error) {
	if plugin == "" || args == nil {
		return "", errors.New("plugin and suggestion are required")
	}
	if err := args.Validate(); err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	var created string
	err = s.update(conversation, func(current *record) (bool, error) {
		expire(current, turn)
		refresh := -1
		for i := range current.Suggestions {
			item := &current.Suggestions[i]
			if item.Status == "pending" && item.Plugin == plugin && item.DedupeKey == args.DedupeKey && sameSuggestionIntent(*item, args) {
				refresh = i
				break
			}
		}
		if refresh >= 0 {
			for i := range current.Suggestions {
				if i != refresh && current.Suggestions[i].Plugin == plugin && current.Suggestions[i].Status == "pending" {
					current.Suggestions[i].Status = "superseded"
				}
			}
			item := &current.Suggestions[refresh]
			item.Title, item.Body, item.CostUSD = args.Title, args.Body, args.CostUsd
			item.CreatedTurn = turn
			item.ExpiresAfterUserTurns = args.ExpiresAfterUserTurns
			if item.ExpiresAfterUserTurns == 0 {
				item.ExpiresAfterUserTurns = defaultExpiry
			}
			created = item.ID
			return true, nil
		}
		used := make(map[string]bool, len(current.Suggestions))
		for i := range current.Suggestions {
			item := &current.Suggestions[i]
			used[item.Code] = true
			if item.Status == "pending" && item.Plugin == plugin {
				item.Status = "superseded"
			}
		}
		if len(current.Suggestions) >= maxRecords {
			kept := current.Suggestions[:0]
			for _, item := range current.Suggestions {
				if item.Status == "pending" {
					kept = append(kept, item)
				}
			}
			current.Suggestions = kept
			if len(kept) >= maxRecords {
				return false, ErrLimit
			}
		}
		var code string
		for attempt := 0; attempt < 16; attempt++ {
			candidate, err := randomCode()
			if err != nil {
				return false, err
			}
			if !used[candidate] {
				code = candidate
				break
			}
		}
		if code == "" {
			return false, ErrConflict
		}
		expiry := args.ExpiresAfterUserTurns
		if expiry == 0 {
			expiry = defaultExpiry
		}
		item := Suggestion{
			ID: id, Code: code, Plugin: plugin, Conversation: conversation,
			Kind: args.Kind, DedupeKey: args.DedupeKey, Title: args.Title, Body: args.Body,
			CostUSD: args.CostUsd, CreatedAt: time.Now().UTC(), CreatedTurn: turn,
			ExpiresAfterUserTurns: expiry, Status: "pending",
		}
		if args.HarnessTargetModel != nil {
			item.HarnessTargetModel = *args.HarnessTargetModel
		}
		for _, action := range args.Actions {
			item.Actions = append(item.Actions, Action{ID: action.Id, Label: action.Label})
		}
		current.Suggestions = append(current.Suggestions, item)
		created = id
		return true, nil
	})
	return created, err
}

func sameSuggestionIntent(item Suggestion, args *pb.SuggestArgs) bool {
	if item.Kind != args.Kind || item.HarnessTargetModel != args.GetHarnessTargetModel() || len(item.Actions) != len(args.Actions) {
		return false
	}
	for i, action := range args.Actions {
		if item.Actions[i].ID != action.Id || item.Actions[i].Label != action.Label {
			return false
		}
	}
	return true
}

// List advances expiry and returns only the requested conversation. A plugin
// name filters outcomes; an empty plugin lets the host UI list all entries.
func (s *Store) List(conversation, plugin string, turn uint64) ([]Suggestion, error) {
	if err := s.update(conversation, func(current *record) (bool, error) { return expire(current, turn), nil }); err != nil {
		return nil, err
	}
	current, _, _, err := s.read(conversation)
	if err != nil {
		return nil, err
	}
	var out []Suggestion
	for _, item := range current.Suggestions {
		if plugin == "" || item.Plugin == plugin {
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Store) ResolveCode(conversation, code, action, via string, turn uint64) (Suggestion, error) {
	if action != "accepted" && action != "dismissed" {
		return Suggestion{}, errors.New("invalid suggestion action")
	}
	var resolved Suggestion
	err := s.update(conversation, func(current *record) (bool, error) {
		expired := expire(current, turn)
		for i := range current.Suggestions {
			item := &current.Suggestions[i]
			if item.Code == code && item.Status == "pending" {
				item.Status, item.Action, item.Via = action, action, via
				resolved = *item
				return true, nil
			}
		}
		return expired, nil
	})
	if err == nil && resolved.ID == "" {
		return Suggestion{}, ErrNotFound
	}
	return resolved, err
}

// ResolveID is for authenticated, conversation-bound control-plane actions.
// Unlike ResolveCode it does not accept a short confirmation code, so callers
// must provide both the conversation scope and the full suggestion ID.
func (s *Store) ResolveID(conversation, id, action, via string, turn uint64) (Suggestion, error) {
	if action != "accepted" && action != "dismissed" {
		return Suggestion{}, errors.New("invalid suggestion action")
	}
	if id == "" {
		return Suggestion{}, ErrNotFound
	}
	var resolved Suggestion
	err := s.update(conversation, func(current *record) (bool, error) {
		expired := expire(current, turn)
		for i := range current.Suggestions {
			item := &current.Suggestions[i]
			if item.ID == id && item.Status == "pending" {
				item.Status, item.Action, item.Via = action, action, via
				resolved = *item
				return true, nil
			}
		}
		return expired, nil
	})
	if err == nil && resolved.ID == "" {
		return Suggestion{}, ErrNotFound
	}
	return resolved, err
}

func (s *Store) AcceptHarnessSwitch(conversation, model string, turn uint64) ([]Suggestion, error) {
	var accepted []Suggestion
	err := s.update(conversation, func(current *record) (bool, error) {
		changed := expire(current, turn)
		accepted = nil
		for i := range current.Suggestions {
			item := &current.Suggestions[i]
			if item.Status == "pending" && item.HarnessTargetModel == model && model != "" {
				item.Status, item.Action, item.Via = "accepted", "accepted", "harness_switch"
				accepted = append(accepted, *item)
				changed = true
			}
		}
		return changed, nil
	})
	return accepted, err
}
