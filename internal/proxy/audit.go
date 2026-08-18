package proxy

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/torana-edge/torana-edge/internal/auditlog"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
)

const auditSchemaVersion = 1

func openAuditWriter(config *auditlog.Config) (*auditlog.Writer, error) {
	if config == nil || !config.Enabled {
		return nil, nil
	}
	return auditlog.Open(*config)
}

// preserveAuditConfigIfOmitted gives the settings API PATCH-like semantics for
// the audit surface the embedded form does not render. Explicit null remains a
// deliberate disable; omission must never silently turn auditing off.
func preserveAuditConfigIfOmitted(body []byte, current, incoming *provider.Config) {
	var topLevel map[string]json.RawMessage
	if json.Unmarshal(body, &topLevel) != nil {
		return
	}
	if _, sent := topLevel["audit"]; !sent {
		incoming.Audit = current.Audit
	}
}

func (s *Server) swapAuditWriter(next *auditlog.Writer) {
	s.auditMu.Lock()
	old := s.auditWriter
	s.auditWriter = next
	s.auditMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// captureAuditRequest snapshots only the sensitive fields the versioned audit
// contract names. It runs on the already-validated canonical request, in wire
// order, and never retains arbitrary tool arguments.
func (rs *reqState) captureAuditRequest(chat *engine.ChatRequest) {
	if rs == nil || !rs.Intercepted || chat == nil {
		return
	}
	rs.Model = chat.Model
	if rs.InitialModel == "" {
		rs.InitialModel = chat.Model
	}
	var calls []auditlog.ToolCall
	for _, message := range chat.Messages {
		for _, block := range message.Blocks {
			if block.ToolUse == nil {
				continue
			}
			call := auditlog.ToolCall{ID: block.ToolUse.ID, Name: block.ToolUse.Name}
			members, _, err := block.ToolUse.Arguments.DecodeObject()
			if err == nil {
				// "i" is the official intent plugin's request-side convention.
				// Only a JSON string is captured; other values remain ordinary
				// opaque tool arguments and are never copied into the audit.
				_ = json.Unmarshal(members["i"], &call.Intent)
			}
			calls = append(calls, call)
		}
	}
	rs.AuditToolCalls = calls
}

func (rs *reqState) auditRecord(status int, plugins []string) auditlog.Record {
	if status == 0 {
		status = http.StatusOK
	}
	errorCode := rs.AuditErrorCode
	if errorCode == "" && status >= http.StatusBadRequest {
		errorCode = "upstream_error"
	} else if errorCode == "" && rs.PluginFailure {
		// Streaming headers may already carry 200 when a filtering plugin
		// refuses a later event and the host terminates the body. Preserve the
		// failure as an explicit fact rather than inferring success from status.
		errorCode = "plugin_failure"
	}
	return auditlog.Record{
		SchemaVersion:        auditSchemaVersion,
		Timestamp:            rs.Start.UTC().Format(time.RFC3339Nano),
		RequestID:            rs.ID,
		InitialProvider:      rs.InitialProvider,
		Provider:             rs.Provider,
		Format:               rs.InitialFormat,
		Path:                 rs.Path,
		InitialModel:         rs.InitialModel,
		Model:                rs.Model,
		Status:               status,
		IngressBytes:         int64(len(rs.inboundBody)),
		UpstreamRequestBytes: rs.AuditUpstreamRequestBytes,
		Plugins:              append([]string(nil), plugins...),
		Verdict:              rs.Verdict,
		VerdictPlugin:        rs.VerdictPlugin,
		PluginFailure:        rs.PluginFailure,
		ErrorCode:            errorCode,
		ToolCalls:            append([]auditlog.ToolCall(nil), rs.AuditToolCalls...),
	}
}

func (s *Server) appendAudit(rs *reqState, status int, plugins []string) {
	if rs == nil || !rs.Intercepted {
		return
	}
	s.auditMu.RLock()
	w := s.auditWriter
	if w == nil {
		s.auditMu.RUnlock()
		return
	}
	err := w.Append(rs.auditRecord(status, plugins))
	s.auditMu.RUnlock()
	if err == nil {
		return
	}
	s.stats.RecordAuditWriteFailure()
	// A broken disk must not create one log line per request. The diagnostic is
	// value-free and rate-limited; the durable counter remains exact.
	now := time.Now().Unix()
	for {
		next := s.auditNextErrorLog.Load()
		if now < next {
			return
		}
		if s.auditNextErrorLog.CompareAndSwap(next, now+60) {
			log.Printf("audit: append failed for configured path")
			return
		}
	}
}
