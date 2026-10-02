// Package resultrelease records human-controlled exceptions without storing
// tool output. Its namespace cannot be claimed by a plugin manifest.
package resultrelease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
)

const Namespace = "@torana/result-release"

var ErrNotFound = errors.New("withheld result not found")
var ErrConflict = errors.New("approval changed concurrently")
var ErrRateLimited = errors.New("review request limit reached")

// A conversation may create at most five new review requests per hour.
// Replaying an existing request does not consume the allowance.
const RequestsPerHour = 5

type AuditEvent struct {
	Action string    `json:"action"`
	At     time.Time `json:"at"`
}

type Scope struct {
	Conversation string `json:"conversation"`
	Plugin       string `json:"plugin"`
	Digest       string `json:"digest"`
	CallID       string `json:"call_id"`
	ContentHash  string `json:"content_hash"`
}

// Record contains metadata only; no tool text, scanner prose or credentials.
type Record struct {
	Scope
	Reference string       `json:"reference"`
	Status    string       `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	Audit     []AuditEvent `json:"audit,omitempty"`
}

type Store struct {
	State     *pluginstate.Store
	MAC       func(string, string) ([]byte, error)
	requestMu sync.Mutex
}

func ValidReference(ref string) bool {
	if len(ref) != 67 || ref[:3] != "tr_" {
		return false
	}
	_, err := hex.DecodeString(ref[3:])
	return err == nil
}

func (s *Store) Observe(scope Scope, register bool) (Record, bool, error) {
	if s == nil || s.State == nil || s.MAC == nil {
		return Record{}, false, errors.New("result approvals are unavailable")
	}
	if scope.Conversation == "" || scope.Plugin == "" || scope.Digest == "" || scope.CallID == "" || len(scope.ContentHash) != 64 {
		return Record{}, false, errors.New("incomplete result scope")
	}
	raw, _ := json.Marshal(scope)
	mac, err := s.MAC("tool-result-release-v1", string(raw))
	if err != nil {
		return Record{}, false, err
	}
	ref := "tr_" + hex.EncodeToString(mac)
	if !ValidReference(ref) {
		return Record{}, false, errors.New("invalid result MAC")
	}
	item, err := s.Get(ref)
	if err == nil {
		if item.Scope != scope {
			return Record{}, false, errors.New("result scope mismatch")
		}
		return item, true, nil
	}
	if !errors.Is(err, ErrNotFound) || !register {
		return Record{}, false, errUnlessMissing(err)
	}
	item = Record{Scope: scope, Reference: ref, Status: "withheld", CreatedAt: time.Now().UTC()}
	text, _ := json.Marshal(item)
	applied, _, err := s.State.CompareAndSet(Namespace, "result/"+ref, string(text), nil)
	if err != nil {
		return Record{}, false, err
	}
	if !applied {
		item, err = s.Get(ref)
	}
	return item, err == nil, err
}

func errUnlessMissing(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (s *Store) Get(ref string) (Record, error) {
	if !ValidReference(ref) {
		return Record{}, ErrNotFound
	}
	if s == nil || s.State == nil {
		return Record{}, errors.New("result approvals are unavailable")
	}
	raw, found, err := s.State.Get(Namespace, "result/"+ref)
	if err != nil {
		return Record{}, err
	}
	if !found {
		return Record{}, ErrNotFound
	}
	var item Record
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return Record{}, err
	}
	if item.Reference != ref {
		return Record{}, errors.New("invalid result approval record")
	}
	return item, nil
}

// Request cannot approve. Repeated model requests cannot reopen a declined
// exception or overwrite an operator's decision.
func (s *Store) Request(ref, conversation string) (Record, error) {
	if s == nil || s.State == nil {
		return Record{}, errors.New("result approvals are unavailable")
	}
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	current, err := s.Get(ref)
	if err != nil || current.Conversation != conversation {
		if err == nil {
			err = ErrNotFound
		}
		return Record{}, err
	}
	if current.Status != "withheld" {
		return current, nil
	}
	// Persist a bounded counter, not every attempted model request. Limits
	// survive restart and do not contain tool content or scanner text.
	hash := sha256.Sum256([]byte(conversation))
	key := "budget/" + hex.EncodeToString(hash[:])
	var budget struct {
		Window time.Time `json:"window"`
		Count  int       `json:"count"`
	}
	raw, found, err := s.State.Get(Namespace, key)
	if err != nil {
		return Record{}, err
	}
	if found && json.Unmarshal([]byte(raw), &budget) != nil {
		return Record{}, errors.New("invalid review request budget")
	}
	now := time.Now().UTC()
	if budget.Window.IsZero() || !now.Before(budget.Window.Add(time.Hour)) {
		budget.Window, budget.Count = now, 0
	}
	if budget.Count >= RequestsPerHour {
		return Record{}, ErrRateLimited
	}
	budget.Count++
	encoded, _ := json.Marshal(budget)
	if err := s.State.Set(Namespace, key, string(encoded)); err != nil {
		return Record{}, err
	}
	return s.change(ref, func(item *Record) error {
		if item.Conversation != conversation {
			return ErrNotFound
		}
		if item.Status == "withheld" {
			item.Status = "pending"
			item.addAudit("requested")
		}
		return nil
	})
}

// Decide is used only by operator decision routes, never MCP or guests.
// Those routes enforce interaction/CSRF safeguards; see #467 for authentication.
func (s *Store) Decide(ref, expectedStatus, decision string) (Record, error) {
	if decision != "approved" && decision != "declined" && decision != "revoked" {
		return Record{}, errors.New("invalid approval decision")
	}
	return s.change(ref, func(item *Record) error {
		if item.Status == decision {
			return nil
		}
		if item.Status != expectedStatus {
			return ErrConflict
		}
		if decision == "revoked" && item.Status != "approved" || decision != "revoked" && item.Status != "pending" {
			return ErrConflict
		}
		item.Status = decision
		item.addAudit(decision)
		return nil
	})
}

func (item *Record) addAudit(action string) {
	item.Audit = append(item.Audit, AuditEvent{Action: action, At: time.Now().UTC()})
	if len(item.Audit) > 8 {
		item.Audit = item.Audit[len(item.Audit)-8:]
	}
}

func (s *Store) change(ref string, change func(*Record) error) (Record, error) {
	if !ValidReference(ref) {
		return Record{}, ErrNotFound
	}
	if s == nil || s.State == nil {
		return Record{}, errors.New("result approvals are unavailable")
	}
	for i := 0; i < 8; i++ {
		raw, version, found, err := s.State.GetVersioned(Namespace, "result/"+ref)
		if err != nil {
			return Record{}, err
		}
		if !found {
			return Record{}, ErrNotFound
		}
		var item Record
		if json.Unmarshal([]byte(raw), &item) != nil || item.Reference != ref {
			return Record{}, errors.New("invalid result approval record")
		}
		before, _ := json.Marshal(item)
		if err := change(&item); err != nil {
			return Record{}, err
		}
		text, _ := json.Marshal(item)
		if bytes.Equal(before, text) {
			return item, nil
		}
		applied, _, err := s.State.CompareAndSet(Namespace, "result/"+ref, string(text), &version)
		if err != nil {
			return Record{}, err
		}
		if applied {
			return item, nil
		}
	}
	return Record{}, ErrConflict
}

func (s *Store) List(cursor string) ([]Record, string, error) {
	if s == nil || s.State == nil {
		return nil, "", errors.New("result approvals are unavailable")
	}
	entries, next, err := s.State.Scan(Namespace, "result/", cursor, 100, 128<<10)
	if err != nil {
		return nil, "", err
	}
	items := make([]Record, 0, len(entries))
	for _, entry := range entries {
		var item Record
		if json.Unmarshal([]byte(entry.Value), &item) != nil {
			return nil, "", errors.New("invalid result approval record")
		}
		items = append(items, item)
	}
	return items, next, nil
}
