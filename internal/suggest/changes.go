package suggest

import (
	"errors"
	"sort"
	"strings"
	"time"
)

const maxChanges = 16

// Change is safe for a model-visible history listing. Undo codes and the
// encrypted prior configuration remain exclusively in the host's record.
type Change struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type changeRecord struct {
	Change
	Code       string `json:"code"`
	SealedUndo string `json:"sealed_undo"`
	Revision   string `json:"revision"`
}

// PrepareOperationChange reserves durable undo history and claims the accepted
// operation atomically, BEFORE any mutation. Storage limits therefore reject
// the operation before external effects, not after losing its undo snapshot.
// postRevision is the revision of the fully validated candidate configuration.
func (s *Store) PrepareOperationChange(conversation, id, sealedUndo, postRevision string) (string, error) {
	if !strings.HasPrefix(sealedUndo, "enc:") || len(sealedUndo) > 128<<10 || postRevision == "" {
		return "", errors.New("encrypted undo snapshot and candidate revision are required")
	}
	var intent string
	err := s.update(conversation, func(current *record) (bool, error) {
		operation, exists := current.Operations[id]
		if !exists || operation.Execution != "" || !time.Now().Before(operation.ExpiresAt) {
			return false, ErrNotFound
		}
		var item *Suggestion
		for i := range current.Suggestions {
			if current.Suggestions[i].ID == id && current.Suggestions[i].Status == "accepted" {
				item = &current.Suggestions[i]
				break
			}
		}
		if item == nil {
			return false, ErrNotFound
		}
		if current.Changes == nil {
			current.Changes = map[string]changeRecord{}
		}
		if len(current.Changes) >= maxChanges {
			oldest := ""
			for key, change := range current.Changes {
				if change.Status == "prepared" || change.Status == "undoing" {
					continue
				}
				if oldest == "" || change.CreatedAt.Before(current.Changes[oldest].CreatedAt) {
					oldest = key
				}
			}
			if oldest == "" {
				return false, ErrLimit
			}
			delete(current.Changes, oldest)
		}
		current.Changes[id] = changeRecord{Change: Change{ID: id, Status: "prepared", CreatedAt: time.Now().UTC()}, Code: item.Code, SealedUndo: sealedUndo, Revision: postRevision}
		operation.Execution = "applying"
		current.Operations[id] = operation
		intent = operation.SealedIntent
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return intent, nil
}

func (s *Store) ListChanges(conversation string) ([]Change, error) {
	current, _, _, err := s.read(conversation)
	if err != nil {
		return nil, err
	}
	changes := make([]Change, 0, len(current.Changes))
	for _, change := range current.Changes {
		changes = append(changes, change.Change)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].CreatedAt.Equal(changes[j].CreatedAt) {
			return changes[i].ID < changes[j].ID
		}
		return changes[i].CreatedAt.Before(changes[j].CreatedAt)
	})
	return changes, nil
}

// ClaimUndo must run while the caller holds the configuration mutation lock.
// The supplied revision is host-derived, never accepted from model input.
func (s *Store) ClaimUndo(conversation, changeID, actualRevision string) (id, sealedUndo string, err error) {
	err = s.update(conversation, func(current *record) (bool, error) {
		change, exists := current.Changes[changeID]
		if !exists || change.Status != "applied" {
			return false, ErrNotFound
		}
		if actualRevision != change.Revision {
			return false, ErrConflict
		}
		change.Status = "undoing"
		current.Changes[changeID] = change
		id, sealedUndo = changeID, change.SealedUndo
		return true, nil
	})
	if err != nil {
		return "", "", err
	}
	return id, sealedUndo, nil
}

func (s *Store) FinishUndo(conversation, id, outcome string) error {
	if outcome != "undone" && outcome != "failed" && outcome != "conflict" {
		return errors.New("invalid undo outcome")
	}
	return s.update(conversation, func(current *record) (bool, error) {
		change, exists := current.Changes[id]
		if !exists || change.Status != "undoing" {
			return false, ErrNotFound
		}
		change.Status, change.SealedUndo = outcome, ""
		current.Changes[id] = change
		return true, nil
	})
}
