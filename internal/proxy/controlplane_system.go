package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/torana-edge/torana-edge/internal/instance"
)

// StopRequested lets the binary's owner run its normal bounded shutdown path.
// The HTTP handler does not exit the process or kill a PID.
func (s *Server) StopRequested() <-chan struct{} { return s.stopRequested }

func (s *Server) systemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "system status requires GET")
		return
	}
	cfg := s.GetConfig()
	configPath, err := filepath.Abs(s.configPath)
	if err != nil {
		writeAgentError(w, http.StatusInternalServerError, "status_unavailable", "cannot resolve configuration path")
		return
	}
	state := "running"
	if s.pluginReloadDegraded.Load() || s.pluginState != nil && s.pluginState.ReadOnly() {
		state = "degraded"
	}
	select {
	case <-s.stopRequested:
		state = "stopping"
	default:
	}
	writeAgentJSON(w, http.StatusOK, map[string]any{
		"service": "torana-edge", "instance_id": s.instanceID, "pid": os.Getpid(),
		"version": cfg.HostVersion, "status": state, "started_at": s.startedAt,
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"config_path":    configPath, "port": cfg.Providers.Port, "plugin_directory": cfg.Providers.Plugins.Dir,
	})
}

func (s *Server) requestStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "shutdown requires POST")
		return
	}
	body, err := readControlPlaneBody(r.Body)
	if errors.Is(err, errControlPlaneBodyTooLarge) {
		writeAgentError(w, http.StatusRequestEntityTooLarge, "invalid_request", "shutdown request is too large")
		return
	}
	if err != nil {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "cannot read shutdown request")
		return
	}
	var request struct {
		InstanceID string `json:"instance_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&request); err != nil || !json.Valid(body) || request.InstanceID == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "instance_id from system status is required (no other fields)")
		return
	}
	if request.InstanceID != s.instanceID {
		writeAgentError(w, http.StatusPreconditionFailed, "stale_instance", "Torana restarted; inspect status before stopping this instance")
		return
	}
	writeAgentJSON(w, http.StatusAccepted, map[string]any{"status": "stopping", "instance_id": s.instanceID})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.stopOnce.Do(func() { close(s.stopRequested) })
}

// Only the standalone binary enables this record, while holding instance.lock.
// Publishing failure prevents a listener change; the CLI must not be left
// pointing at a port that the running instance no longer owns.
func (s *Server) recordListener(address net.Addr) error {
	path := s.config.InstanceRecordPath
	if path == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return instance.WriteRecord(path, instance.Record{Address: net.JoinHostPort(host, port), InstanceID: s.instanceID})
}
