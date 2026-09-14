package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/instance"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestSystemCannotReportReadyBeforeStartupCompletes(t *testing.T) {
	cfg := provider.DefaultConfig()
	cfg.Port = 0
	cfg.Plugins.Dir = t.TempDir()
	s, err := New(Config{Port: "0", Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatal(err)
	}
	// Hold the startup seam: a bound HTTP listener must not serve a ready
	// status while MITM configuration can still fail.
	s.mitmMu.Lock()
	var unlock sync.Once
	defer func() { unlock.Do(s.mitmMu.Unlock); _ = s.Shutdown(context.Background()) }()
	started := make(chan error, 1)
	go func() { started <- s.Start("127.0.0.1") }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var address string
	for address == "" {
		s.listenerMu.Lock()
		if s.listener != nil {
			address = s.listener.Addr().String()
		}
		s.listenerMu.Unlock()
		if address == "" {
			select {
			case <-ctx.Done():
				t.Fatal("listener was never bound")
			case <-time.After(time.Millisecond):
			}
		}
	}
	client := &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	if resp, err := client.Get("http://" + address + "/_torana/api/v1/system"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("control API served before startup completed")
	}
	unlock.Do(s.mitmMu.Unlock)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://" + address + "/_torana/api/v1/system")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status after startup: %d", resp.StatusCode)
	}
}

func TestSystemStopIdentityAndStrictInput(t *testing.T) {
	cfg := provider.DefaultConfig()
	cfg.Plugins.Dir = t.TempDir()
	s, err := New(Config{Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{}`, 400}, {`{"instance_id":"stale"}`, 412},
		{`{"instance_id":"` + s.instanceID + `","extra":true}`, 400},
		{`{"instance_id":"` + s.instanceID + `"} {}`, 400},
	} {
		r := authorizedControlPlaneMutation(http.MethodPost, "/_torana/api/v1/system/stop", strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
		select {
		case <-s.StopRequested():
			t.Fatal("invalid request stopped instance")
		default:
		}
	}
	for i := 0; i < 2; i++ {
		r := authorizedControlPlaneMutation(http.MethodPost, "/_torana/api/v1/system/stop", strings.NewReader(`{"instance_id":"`+s.instanceID+`"}`))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 202 {
			t.Fatalf("stop: %d %s", w.Code, w.Body.String())
		}
	}
	select {
	case <-s.StopRequested():
	default:
		t.Fatal("stop not delivered")
	}
}

func TestInstanceRecordTracksPortAndRejectsStaleClient(t *testing.T) {
	dir := t.TempDir()
	cfg := provider.DefaultConfig()
	cfg.Port = 0
	cfg.Plugins.Dir = t.TempDir()
	path := filepath.Join(dir, "instance.json")
	s, err := New(Config{Port: "0", Providers: cfg, ConfigPath: filepath.Join(dir, "config.json"), InstanceRecordPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	if err := s.Start("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	first, err := instance.ReadRecord(path)
	if err != nil || first.InstanceID != s.instanceID {
		t.Fatalf("record: %v %v", first, err)
	}
	if err := s.SetPort(0); err != nil {
		t.Fatal(err)
	}
	second, err := instance.ReadRecord(path)
	if err != nil || second.Address == first.Address {
		t.Fatalf("port record unchanged: %v %v", second, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/_torana/api/v1/system", nil)
	r.RemoteAddr, r.Host = "127.0.0.1:9999", "localhost"
	r.Header.Set("X-Torana-Instance-ID", "stale")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 412 {
		t.Fatalf("stale identity accepted: %d", w.Code)
	}
	r.Header.Set("X-Torana-Instance-ID", s.instanceID)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var status map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	if w.Code != 200 || status["instance_id"] != s.instanceID {
		t.Fatalf("status %d %v", w.Code, status)
	}
}
