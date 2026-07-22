package proxy

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestApplyMITMBadConfigKeepsRunningIngress verifies that a MITM
// reconfiguration whose new config fails validation does NOT tear down the
// currently-running ingress. A rejected settings PUT must leave the operator
// with the ingress they already had, not with no ingress at all.
func TestApplyMITMBadConfigKeepsRunningIngress(t *testing.T) {
	srv, err := New(Config{Port: "0", Providers: testProviderConfig("http://127.0.0.1:1", "test", "openai")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.applyMITM(provider.MITMConfig{Enabled: false}) })

	// Bring up a valid ingress (bind to an ephemeral port; CA materializes in a temp dir).
	good := provider.MITMConfig{Enabled: true, Listen: "127.0.0.1:0", CADir: t.TempDir()}
	if err := srv.applyMITM(good); err != nil {
		t.Fatalf("applyMITM(good): %v", err)
	}
	srv.mitmMu.Lock()
	running := srv.mitmSrv
	srv.mitmMu.Unlock()
	if running == nil {
		t.Fatal("expected a running MITM ingress after applyMITM(good)")
	}

	// A subsequent invalid config (missing ca_dir) must be rejected without
	// disturbing the running ingress.
	bad := provider.MITMConfig{Enabled: true, Listen: "127.0.0.1:0", CADir: ""}
	if err := srv.applyMITM(bad); err == nil {
		t.Fatal("applyMITM(bad): expected an error, got nil")
	}
	srv.mitmMu.Lock()
	after := srv.mitmSrv
	srv.mitmMu.Unlock()
	if after == nil {
		t.Fatal("running ingress was torn down by a failed reconfiguration")
	}
	if after != running {
		t.Fatal("running ingress was replaced by a failed reconfiguration")
	}
}
