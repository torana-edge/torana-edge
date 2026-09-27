package suggest

import (
	"encoding/json"
	"errors"
	"time"
)

const transcriptTicketKey = "transcript-tickets/v1"
const maxTranscriptTickets = 512

type transcriptTickets map[string]int64

// ClaimTranscriptTicket durably consumes one host-issued ticket. A ticket is
// global to this Torana instance, so replaying it in another conversation also
// fails. Expired entries are pruned on each claim.
func (s *Store) ClaimTranscriptTicket(nonce string, expires time.Time) error {
	if s == nil || s.state == nil || nonce == "" || !time.Now().Before(expires) {
		return errors.New("valid transcript ticket is required")
	}
	for attempt := 0; attempt < 8; attempt++ {
		value, version, found, err := s.state.GetVersioned(stateNamespace, transcriptTicketKey)
		if err != nil {
			return err
		}
		current := transcriptTickets{}
		if found && json.Unmarshal([]byte(value), &current) != nil {
			return errors.New("invalid transcript ticket state")
		}
		now := time.Now().Unix()
		for key, expiry := range current {
			if expiry <= now {
				delete(current, key)
			}
		}
		if _, used := current[nonce]; used {
			return errors.New("transcript ticket was already used")
		}
		if len(current) >= maxTranscriptTickets {
			return ErrLimit
		}
		current[nonce] = expires.Unix()
		encoded, err := json.Marshal(current)
		if err != nil {
			return err
		}
		var expected *string
		if found {
			expected = &version
		}
		applied, _, err := s.state.CompareAndSet(stateNamespace, transcriptTicketKey, string(encoded), expected)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
	}
	return ErrConflict
}
