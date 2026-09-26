package proxy

import (
	"log"

	"github.com/torana-edge/torana-edge/internal/annotate"
)

// appendPendingNotice uses only suggestions from this user turn, avoiding a
// repeated notice on every later assistant reply. UI and CLI retain the full
// durable list, including suggestions a harness cannot safely display.
func (s *Server) appendPendingNotice(body []byte, rs *reqState, shape string) []byte {
	if rs == nil || !rs.NoticeEnabled || rs.ConversationID == "" || rs.UserTurn == 0 || s.suggestions == nil {
		return body
	}
	items, err := s.suggestions.List(rs.ConversationID, "", rs.UserTurn)
	if err != nil {
		log.Printf("[suggest] could not list notice candidates: %v", err)
		return body
	}
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Status != "pending" || item.CreatedTurn != rs.UserTurn {
			continue
		}
		notice, err := annotate.Render(s.secrets, rs.ConversationID, item)
		if err != nil {
			log.Printf("[suggest] could not sign notice: %v", err)
			return body
		}
		updated, changed, err := appendNoticeJSON(body, shape, notice, item.ID)
		if err != nil {
			log.Printf("[suggest] could not place notice: %v", err)
			return body
		}
		if changed {
			return updated
		}
		return body // tool-calling or incomplete turn: never attach a notice
	}
	return body
}
