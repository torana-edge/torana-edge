package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/suggest"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestNamespacePolicyExhaustiveStandardProtectionAndOverrides(t *testing.T) {
	for _, name := range []string{"pii", "pii_guard", "auth", "logger"} {
		bundle := plugin.PluginBundle{Manifest: plugin.PluginManifest{Name: name}, Digest: "digest"}
		r, err := buildNamespaceRegistry([]plugin.PluginBundle{bundle}, []plugin.LoadedPluginStatus{{Name: name, Digest: "digest"}}, []string{name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		entry, _ := r.resolve(name, false)
		for _, override := range []string{"read", "confirm", "never"} {
			p, err := newNamespaceAccessPolicy(r, nil, map[string]string{name: override})
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range entry.Operations {
				got := p.ModelReachable(name, op.ID)
				floor := op.ID == "_config.get" || (name != "logger" && op.Risk != "read")
				want := op.ModelAccess
				if accessRank(override) > accessRank(want) {
					want = override
				}
				if floor {
					want = "never"
				}
				if got != want {
					t.Fatalf("%s.%s override=%s access=%s want=%s", name, op.ID, override, got, want)
				}
			}
		}
	}
}

func TestNamespacePolicyCoreFloorCannotBeReached(t *testing.T) {
	r, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, map[string]string{"torana": "read"})
	if err != nil {
		t.Fatal(err)
	}
	for _, builtIn := range builtInAgentOperations() {
		allowed := false
		switch builtIn.ID {
		case "torana.system.status", "torana.plugins.list", "torana.stats.get", "torana.feed.list", "torana.suggestions.list":
			allowed = true
		}
		if allowed {
			continue
		}
		operation := builtIn.ID[len("torana."):]
		if p.ModelReachable("torana", operation) != "never" {
			t.Fatalf("operator-only core operation reachable: %s", builtIn.ID)
		}
	}
}

func TestNamespacePolicyUnscopedCoreHandlersStayUnavailable(t *testing.T) {
	r, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, map[string]string{"torana": "read"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"feed.recent", "suggestions.list", "session.usage", "changes.list", "changes.undo"} {
		if p.ModelReachable("torana", id) != "never" {
			t.Fatalf("unscoped operation reachable: %s", id)
		}
	}
}

// Keep this guard when scoped core dispatch replaces the operator mappings:
// every callable core mapping must have neither caller-chosen conversation
// input nor confirmation-code output. New scoped operations need their own
// dispatcher regression here before becoming reachable.
func TestModelReachableCoreContractsDoNotExposeCodesOrConversationSelection(t *testing.T) {
	r, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := provider.DefaultConfig()
	config.Suggestions.Enabled = true
	server, err := New(Config{Port: "8080", Providers: config})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	server.suggestions = suggest.New(state)
	if _, err := server.suggestions.Create("bound", "router", 1, &pb.SuggestArgs{Kind: "model_switch", DedupeKey: "guard", Title: "Try another model", Body: "Guard fixture"}); err != nil {
		t.Fatal(err)
	}
	items, err := server.suggestions.List("bound", "", 1)
	if err != nil || len(items) != 1 || items[0].Code == "" {
		t.Fatalf("seed=%+v err=%v", items, err)
	}
	code := items[0].Code
	contracts := map[string]agentAPIOperation{}
	for _, op := range builtInAgentOperations() {
		contracts[op.ID] = op
	}
	for _, entry := range r.list() {
		if entry.Name != "torana" {
			continue
		}
		for _, op := range entry.Operations {
			if p.ModelReachable(entry.Name, op.ID) == "never" {
				continue
			}
			contract, exists := contracts[op.CoreID]
			if !exists {
				t.Fatalf("add scoped dispatch guard before exposing %s", op.ID)
			}
			// Current reachable core reads take no caller input. Future scoped
			// handlers must use a host binding, not any model-supplied selector.
			if strings.ContainsAny(contract.Path, "?{}") || len(contract.InputSchema) != 0 {
				t.Fatalf("core input needs scoped dispatcher validation: %s", op.ID)
			}
			request := localControlPlaneRequest(http.MethodGet, contract.Path, nil)
			request.RemoteAddr = "127.0.0.1:12345"
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s: status=%d", op.ID, recorder.Code)
			}
			if strings.Contains(recorder.Body.String(), code) {
				t.Fatalf("confirmation code value leaked by %s", op.ID)
			}
			var value any
			if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			assertNoConfirmationCode(t, op.ID, value)
		}
	}
}

func assertNoConfirmationCode(t *testing.T, operation string, value any) {
	t.Helper()
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if key == "code" || key == "confirmation_code" {
				t.Fatalf("confirmation code field in %s", operation)
			}
			assertNoConfirmationCode(t, operation, child)
		}
	case []any:
		for _, child := range item {
			assertNoConfirmationCode(t, operation, child)
		}
	}
}
