package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostedFontsDoNotRelaxPluginPolicy(t *testing.T) {
	for _, sandbox := range []bool{false, true} {
		rec := httptest.NewRecorder()
		setControlPlaneSecurityHeaders(rec, sandbox, sandbox)
		csp := rec.Header().Get("Content-Security-Policy")
		if strings.Contains(csp, "font-src 'self' https://fonts.gstatic.com") == sandbox {
			t.Fatalf("wrong font policy for sandbox=%v: %s", sandbox, csp)
		}
		if !strings.Contains(csp, "connect-src 'self';") || !strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
			t.Fatal("unrelated policy relaxed")
		}
	}
}
