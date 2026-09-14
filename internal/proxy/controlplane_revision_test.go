package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestEveryConfigMutationRequiresAnExactRevision(t *testing.T) {
	for _, prefix := range []string{"/_torana/api", "/_torana/api/v1"} {
		for _, route := range []struct{ method, path string }{
			{http.MethodPut, "/config"},
			{http.MethodPost, "/config"},
			{http.MethodPut, "/plugins"},
			{http.MethodPost, "/plugins"},
			{http.MethodPost, "/plugins/settings/config"},
		} {
			t.Run(prefix+route.path+"/"+route.method, func(t *testing.T) {
				e := newPluginConfigEnv(t)
				if status, body := e.put(t, `{"config":{"settings":{"enabled":false}}}`); status != http.StatusOK {
					t.Fatalf("seed configuration: %d %s", status, body)
				}
				revision := readControlPlaneRevision(t, e.srv)
				before, err := os.ReadFile(e.configPath)
				if err != nil {
					t.Fatal(err)
				}
				pipeline := e.srv.pluginPipeline.Load()
				for _, input := range []struct {
					name   string
					values []string
					status int
					code   string
				}{
					{"missing", nil, 428, "revision_required"},
					{"empty", []string{""}, 428, "revision_required"},
					{"stale", []string{`"outdated"`}, 412, "stale_revision"},
					{"wildcard", []string{"*"}, 412, "stale_revision"},
					{"weak", []string{"W/" + revision}, 412, "stale_revision"},
					{"list", []string{revision + `, "other"`}, 412, "stale_revision"},
					{"multiple", []string{revision, revision}, 412, "stale_revision"},
				} {
					t.Run(input.name, func(t *testing.T) {
						// A failed precondition must reject before even consuming the
						// candidate, not just reject after constructing runtime state.
						started := make(chan struct{})
						body := &observedMutationBody{reader: strings.NewReader(`{}`), started: started}
						req := authorizedControlPlaneMutation(route.method, prefix+route.path, body)
						for _, value := range input.values {
							req.Header.Add("If-Match", value)
						}
						rec := httptest.NewRecorder()
						e.srv.Handler().ServeHTTP(rec, req)
						if rec.Code != input.status {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
						}
						if prefix == "/_torana/api/v1" {
							var envelope agentAPIErrorEnvelope
							if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != input.code {
								t.Fatalf("expected %s JSON error: %s (%v)", input.code, rec.Body, err)
							}
						}
						if rec.Header().Get("Cache-Control") != "no-store" {
							t.Fatal("precondition error can be cached")
						}
						select {
						case <-started:
							t.Fatal("rejected request consumed candidate body")
						default:
						}
						after, err := os.ReadFile(e.configPath)
						if err != nil || !bytes.Equal(before, after) || readControlPlaneRevision(t, e.srv) != revision || e.srv.pluginPipeline.Load() != pipeline {
							t.Fatalf("rejected request changed persisted configuration or runtime: %v", err)
						}
					})
				}
				body := `{"config":{"settings":{"enabled":true}}}`
				if route.path == "/config" {
					cfg := e.srv.GetConfig().Providers
					cfg.Limits.RPM = 19
					raw, _ := json.Marshal(cfg)
					body = string(raw)
				} else if route.path == "/plugins/settings/config" {
					body = `{"enabled":true}`
				}
				req := authorizedControlPlaneMutation(route.method, prefix+route.path, strings.NewReader(body))
				req.Header.Set("If-Match", revision)
				rec := httptest.NewRecorder()
				e.srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || readControlPlaneRevision(t, e.srv) == revision {
					t.Fatalf("fresh edit did not apply: status=%d body=%s", rec.Code, rec.Body)
				}
			})
		}
	}
}

func TestRevisionPreconditionsAreDiscoverable(t *testing.T) {
	want := map[string]string{
		"torana.config.update":        "/_torana/api/v1/config",
		"torana.plugins.update":       "/_torana/api/v1/plugins",
		"torana.plugin.config.update": "/_torana/api/v1/plugins",
	}
	for _, op := range builtInAgentOperations() {
		path, required := want[op.ID]
		if !required {
			if op.RevisionPrecondition != nil {
				t.Fatalf("%s incorrectly requires a configuration revision", op.ID)
			}
			continue
		}
		if op.RevisionPrecondition == nil || op.RevisionPrecondition.Header != "If-Match" || op.RevisionPrecondition.ReadPath != path {
			t.Fatalf("%s missing revision instructions: %+v", op.ID, op.RevisionPrecondition)
		}
		delete(want, op.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing configuration operations: %v", want)
	}
}
