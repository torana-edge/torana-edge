package controlcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func invoke(addr, input string, args ...string) (string, string, error) {
	var out, diag bytes.Buffer
	args = append(args, "--addr", addr)
	err := Run(context.Background(), args, strings.NewReader(input), &out, &diag)
	return out.String(), diag.String(), err
}

func TestUsageAndInvalidInputDoNotContactServer(t *testing.T) {
	for _, args := range [][]string{
		{"plugin", "enable", "foo"}, {"plugin", "approve", "foo"},
		{"config", "apply"}, {"pipeline", "order", "--yes"},
		{"pipeline", "order", "foo", "--empty", "--yes"},
		{"config", "get", "typo"}, {"plugin", "inspect", "../foo"},
		{"stats", "--file", "foo"}, {"feed", "--follow", "--follow"},
		{"stats", "--typo"},
		{"suggestions", "list"}, {"suggestions", "accept", "sg_1", "--conversation", "c"},
	} {
		_, _, err := invoke("127.0.0.1:1", "", args...)
		if err == nil || strings.Contains(err.Error(), "could not reach") {
			t.Errorf("%v: %v", args, err)
		}
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"plugin", "inspect", "--help"}, strings.NewReader(""), &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "approve") {
		t.Fatal("missing help")
	}
}

func TestSuggestionsCLIOptions(t *testing.T) {
	if !Handles([]string{"suggestions", "list"}) || !Handles([]string{"conversations"}) {
		t.Fatal("suggestions or conversations not routed to live CLI")
	}
	var diag bytes.Buffer
	got, err := parseOptions([]string{"sg_1", "--conversation", "c", "--yes"}, "conversation yes", &diag)
	if err != nil || got.conversation != "c" || !got.yes || len(got.args) != 1 || got.args[0] != "sg_1" {
		t.Fatalf("parsed suggestion action: %+v, %v", got, err)
	}
}

func TestPluginCommandsPreserveUnrelatedConfigurationAndApprovals(t *testing.T) {
	const digest = "sha256:reviewed"
	const revision = `"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`
	cfg := provider.DefaultConfig()
	cfg.Plugins.Order = []string{"other"}
	cfg.Plugins.Approvals = map[string]provider.PluginApproval{"absent-but-retained": {Digest: "sha256:other"}}
	var patches []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/config"):
			w.Header().Set("ETag", revision)
			json.NewEncoder(w).Encode(cfg)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/plugins"):
			fmt.Fprintf(w, `{"plugins":[{"id":"org/foo","name":"foo","digest":%q,"permissions":["env.log"]}]}`, digest)
		case r.Method == "PUT":
			if r.Header.Get("If-Match") != revision {
				t.Error("missing revision")
			}
			var patch map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
			}
			patches = append(patches, patch)
			if p := patch["approvals"]; p != nil {
				cfg.Plugins.Approvals = nil
				json.Unmarshal(p, &cfg.Plugins.Approvals)
			}
			if p := patch["order"]; p != nil {
				json.Unmarshal(p, &cfg.Plugins.Order)
			}
			json.NewEncoder(w).Encode(cfg.Plugins)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if _, _, err := invoke(srv.URL, "", "plugin", "enable", "foo", "--yes"); err == nil {
		t.Fatal("enabled without approval")
	}
	approval := `{"digest":"sha256:reviewed","permissions":["env.log"],"failure_mode":"block"}`
	for _, input := range []string{strings.Replace(approval, "reviewed", "old", 1), strings.Replace(approval, "env.log", "env.http_call", 1), strings.Replace(approval, "block", "", 1)} {
		if _, _, err := invoke(srv.URL, input, "plugin", "approve", "foo", "--file", "-", "--yes"); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	if len(patches) != 0 {
		t.Fatal("invalid approval mutated server")
	}
	if _, _, err := invoke(srv.URL, approval, "plugin", "approve", "foo", "--file", "-", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(patches[0]) != 1 || patches[0]["approvals"] == nil {
		t.Fatal("approval changed unrelated settings")
	}
	if len(cfg.Plugins.Order) != 1 {
		t.Fatal("approval auto-enabled plugin")
	}
	if _, ok := cfg.Plugins.Approvals["absent-but-retained"]; !ok {
		t.Fatal("lost unrelated approval")
	}
	for _, action := range []string{"enable", "disable", "enable", "revoke"} {
		if _, _, err := invoke(srv.URL, "", "plugin", action, "foo", "--yes"); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if len(cfg.Plugins.Order) != 1 || cfg.Plugins.Order[0] != "other" || len(cfg.Plugins.Approvals) != 1 {
		t.Fatalf("revoke lost unrelated data: %+v", cfg.Plugins)
	}
}

func TestSavedButUnloadedIsNotSilentSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"warnings":[{"name":"foo","reason":"digest changed","remedy":"Review approval"}]}`)
	}))
	defer srv.Close()
	s := snapshot{Revision: `"` + strings.Repeat("a", 64) + `"`, Pipeline: json.RawMessage(`{"order":[],"hook_order":{},"config":{},"approvals":{}}`)}
	b, _ := json.Marshal(s)
	out, diag, err := invoke(srv.URL, string(b), "pipeline", "apply", "--file", "-", "--yes")
	if err == nil || !strings.Contains(err.Error(), "configuration saved") || !strings.Contains(diag, "foo is not running") || !json.Valid([]byte(out)) {
		t.Fatalf("out=%s diag=%s err=%v", out, diag, err)
	}
}

func TestAgentCallRequiresDiscoveryAndWriteConsent(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == controlclient.BasePath+"/" {
			io.WriteString(w, `{"operations":[{"id":"plugin:foo:clear","plugin":"foo","plugin_digest":"sha256:reviewed","method":"POST","path":"/_torana/api/v1/agent/plugins/foo/clear","risk":"destructive","input_schema":{"type":"object"}},{"id":"torana.config.update","method":"PUT","path":"/_torana/api/v1/config","risk":"write"}]}`)
			return
		}
		calls++
		if r.Header.Get("X-Torana-Plugin-Digest") != "sha256:reviewed" {
			t.Error("operation was not bound to discovered digest")
		}
		io.WriteString(w, `{"cleared":true}`)
	}))
	defer srv.Close()
	for _, args := range [][]string{{"agent", "call", "plugin:foo:clear", "--file", "-"}, {"agent", "call", "torana.config.update", "--yes"}, {"agent", "call", "missing", "--yes"}} {
		if _, _, err := invoke(srv.URL, `{}`, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if calls != 0 {
		t.Fatal("unsafe call dispatched")
	}
	if _, _, err := invoke(srv.URL, `{}`, "agent", "call", "plugin:foo:clear", "--file", "-", "--yes"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestFeedFollowsAsNDJSONAndReportsDisconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": comment\n\ndata: {\"event\":1}\n\ndata: {\n"+"data: \"event\":2}\n\n")
	}))
	defer srv.Close()
	out, _, err := invoke(srv.URL, "", "feed", "--follow")
	if err == nil || !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("disconnect error=%v", err)
	}
	if out != "{\"event\":1}\n{\"event\":2}\n" {
		t.Fatalf("output=%q", out)
	}
}
