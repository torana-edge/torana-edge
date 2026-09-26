package proxy

import (
	"log"

	"github.com/torana-edge/torana-edge/internal/annotate"
)

// A generic thread/session header or content-derived root identifies a
// conversation, not a verified harness. Never use it to enable a display
// channel even if an operator accidentally puts it in the allowlist.
func knownNoticeHarnessSource(source string) bool {
	switch source {
	case "claude-code-session", "codex-thread", "codex-session", "codex-client-thread", "gemini-code-assist-session":
		return true
	default:
		return false
	}
}

// appendPendingNotice uses only suggestions from this user turn, avoiding a
// repeated notice on every later assistant reply. UI and CLI retain the full
// durable list, including suggestions a harness cannot safely display.
func (s *Server) appendPendingNotice(body []byte, rs *reqState, shape string) []byte {
	notice, id := s.pendingNotice(rs)
	if notice == "" {
		return body
	}
	updated, changed, err := appendNoticeJSON(body, shape, notice, id)
	if err != nil {
		log.Printf("[suggest] could not place notice: %v", err)
		return body
	}
	if changed {
		return updated
	}
	return body // tool-calling or incomplete turn: never attach a notice
}

func (s *Server) pendingNotice(rs *reqState) (string, string) {
	if rs == nil || !rs.NoticeEnabled || rs.ConversationID == "" || rs.UserTurn == 0 || s.suggestions == nil {
		return "", ""
	}
	if rs.NoticeProbe {
		probe, err := annotate.RenderProbe(s.secrets, rs.ConversationID)
		if err != nil {
			log.Printf("[suggest] could not sign notice probe: %v", err)
			return "", ""
		}
		return probe, "probe_v1"
	}
	items, err := s.suggestions.List(rs.ConversationID, "", rs.UserTurn)
	if err != nil {
		log.Printf("[suggest] could not list notice candidates: %v", err)
		return "", ""
	}
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Status != "pending" || item.CreatedTurn != rs.UserTurn {
			continue
		}
		notice, err := annotate.Render(s.secrets, rs.ConversationID, item)
		if err != nil {
			log.Printf("[suggest] could not sign notice: %v", err)
			return "", ""
		}
		return notice, item.ID
	}
	return "", ""
}
