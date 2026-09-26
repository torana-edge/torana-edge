package suggest

import (
	"errors"
	"strings"
	"time"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// OperationProposal is host-owned. The intent is encrypted with the instance
// secret store before storage; human-facing suggestion fields never contain it.
type OperationProposal struct {
	IntentKey    string
	IntentDigest string
	Title        string
	Body         string
	SealedIntent string
	ExpiresAt    time.Time
}

type operationRecord struct {
	SealedIntent string    `json:"sealed_intent"`
	ExpiresAt    time.Time `json:"expires_at"`
	Execution    string    `json:"execution,omitempty"`
	IntentDigest string    `json:"intent_digest"`
}

func (s *Store) CreateOperation(conversation string, turn uint64, proposal OperationProposal) (string, error) {
	if proposal.IntentDigest == "" || len(proposal.IntentDigest) > 128 || !strings.HasPrefix(proposal.SealedIntent, "enc:") || len(proposal.SealedIntent) > 128<<10 || !time.Now().Before(proposal.ExpiresAt) {
		return "", errors.New("sealed operation intent and a future expiry are required")
	}
	return s.create(conversation, "torana", turn, &pb.SuggestArgs{Kind: "torana_operation", DedupeKey: proposal.IntentKey, Title: proposal.Title, Body: proposal.Body, ExpiresAfterUserTurns: 100}, &operationRecord{SealedIntent: proposal.SealedIntent, ExpiresAt: proposal.ExpiresAt, IntentDigest: proposal.IntentDigest})
}

func pruneOperationPayloads(current *record) {
	kept := map[string]bool{}
	for _, item := range current.Suggestions {
		if item.Status == "pending" || item.Status == "accepted" {
			kept[item.ID] = true
		}
	}
	for id := range current.Operations {
		if !kept[id] {
			delete(current.Operations, id)
		}
	}
}

// ClaimOperation is called only after explicit user acceptance. Its CAS-backed
// claim precedes execution, preventing concurrent/replayed confirmation from
// running a guest mutation twice. An interrupted claim is not silently retried:
// external effects cannot be made exactly-once by replaying an arbitrary plugin.
func (s *Store) ClaimOperation(conversation, id string) (string, error) {
	var intent string
	err := s.update(conversation, func(current *record) (bool, error) {
		for _, item := range current.Suggestions {
			if item.ID != id || item.Status != "accepted" {
				continue
			}
			operation, exists := current.Operations[id]
			if !exists || operation.Execution != "" || !time.Now().Before(operation.ExpiresAt) {
				return false, ErrNotFound
			}
			operation.Execution = "applying"
			current.Operations[id] = operation
			intent = operation.SealedIntent
			return true, nil
		}
		return false, ErrNotFound
	})
	if err != nil {
		return "", err
	}
	return intent, nil
}

// FinishOperation releases the encrypted pending intent after execution. The
// caller must first persist any undo history in the host's change ledger.
func (s *Store) FinishOperation(conversation, id, outcome string) error {
	if outcome != "applied" && outcome != "failed" && outcome != "conflict" {
		return errors.New("invalid operation outcome")
	}
	return s.update(conversation, func(current *record) (bool, error) {
		operation, exists := current.Operations[id]
		if !exists || operation.Execution != "applying" {
			return false, ErrNotFound
		}
		for i := range current.Suggestions {
			if current.Suggestions[i].ID == id && current.Suggestions[i].Status == "accepted" {
				current.Suggestions[i].Outcome = outcome
				delete(current.Operations, id)
				return true, nil
			}
		}
		return false, ErrNotFound
	})
}
