package controlcmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResultDecisionCannotUsePipedInputOrYes(t *testing.T) {
	ref := "tr_" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-interactive decision sent a write: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"reference":%q,"status":"pending"}`, ref)
	}))
	defer server.Close()
	for _, action := range []string{"approve", "decline", "revoke"} {
		_, _, err := invoke(server.URL, "aaaaaaaa\n", "approvals", action, ref)
		if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
			t.Fatalf("piped %s: %v", action, err)
		}
		_, _, err = invoke(server.URL, "aaaaaaaa\n", "approvals", action, ref, "--yes")
		if err == nil || !strings.Contains(err.Error(), "unknown option") {
			t.Fatalf("--yes %s: %v", action, err)
		}
	}
}
