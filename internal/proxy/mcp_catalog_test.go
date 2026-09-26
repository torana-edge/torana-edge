package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

func catalogTestPolicy(t *testing.T, count int) *namespaceAccessPolicy {
	t.Helper()
	d := &plugin.AgentDescriptor{SchemaVersion: 2, Namespace: &plugin.AgentNamespace{Title: "Logger", Summary: "Inspect usage", Alias: "logs"}}
	for i := 0; i < count; i++ {
		d.Operations = append(d.Operations, plugin.AgentOperation{ID: fmt.Sprintf("read.%03d", i), Risk: "read", Description: "Read usage", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"string"}`), Examples: []string{"read usage"}})
	}
	d.Operations = append(d.Operations, plugin.AgentOperation{ID: "hidden", Risk: "destructive", Path: "/private-secret-route", Description: "hidden-only"}, plugin.AgentOperation{ID: "old", Risk: "read", Description: "old usage", Deprecated: true})
	b := plugin.PluginBundle{Manifest: plugin.PluginManifest{Name: "logger"}, Digest: "approved", Agent: d}
	r, err := buildNamespaceRegistry([]plugin.PluginBundle{b}, []plugin.LoadedPluginStatus{{Name: "logger", Digest: "approved", Agent: d}}, []string{"logger"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newNamespaceAccessPolicy(r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMCPCatalogUsesExecutionPolicy(t *testing.T) {
	p := catalogTestPolicy(t, 2)
	for _, raw := range []string{`{"namespace":"logger"}`, `{"namespace":"logger","operation":"hidden"}`} {
		result := p.catalogDispatch("torana_describe", json.RawMessage(raw))
		encoded, _ := json.Marshal(result)
		for _, secret := range []string{"hidden-only", "private-secret-route", "_config.get", "confirmation_code"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("discovery leaked %s: %s", secret, encoded)
			}
		}
	}
	p.overrides["logger.read.000"] = "never"
	for _, item := range p.catalog("logger", true) {
		if item.ID == "read.000" {
			t.Fatal("operator restriction ignored")
		}
	}
	result := p.catalogDispatch("torana_describe", json.RawMessage(`{"namespace":"logs"}`))
	if result.Error == nil || result.Error.Code != "unknown_namespace" {
		t.Fatalf("alias accepted: %+v", result)
	}
	result = p.catalogDispatch("torana_describe", json.RawMessage(`{"namespace":"logger","operation":"read.001"}`))
	item, ok := result.Result.(catalogOperation)
	if !result.OK || !ok || len(item.InputSchema) == 0 || len(item.OutputSchema) == 0 || len(item.Examples) != 1 {
		t.Fatalf("missing operation details: %+v", result)
	}
}

func TestMCPCatalogPaginationAndStaleCursor(t *testing.T) {
	p := catalogTestPolicy(t, 103)
	var all []catalogOperation
	cursor := ""
	firstCursor := ""
	for {
		raw, _ := json.Marshal(map[string]string{"namespace": "logger", "cursor": cursor})
		result := p.catalogDispatch("torana_describe", raw)
		if !result.OK {
			t.Fatalf("page error: %+v", result)
		}
		encoded, _ := json.Marshal(result.Result)
		var page struct {
			Operations []catalogOperation `json:"operations"`
			NextCursor string             `json:"next_cursor"`
		}
		if err := json.Unmarshal(encoded, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Operations) > 50 {
			t.Fatal("oversized page")
		}
		all = append(all, page.Operations...)
		if firstCursor == "" {
			firstCursor = page.NextCursor
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(all) != len(p.catalog("logger", false)) {
		t.Fatal("pagination lost operations")
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID >= all[i].ID {
			t.Fatal("unstable/duplicate page entries")
		}
	}
	p.overrides["logger.read.000"] = "never"
	raw, _ := json.Marshal(map[string]string{"namespace": "logger", "cursor": firstCursor})
	if result := p.catalogDispatch("torana_describe", raw); result.OK {
		t.Fatal("stale cursor accepted")
	}
}

func TestMCPCatalogSearchAndInputErrors(t *testing.T) {
	p := catalogTestPolicy(t, 30)
	result := p.catalogDispatch("torana_search", json.RawMessage(`{"query":"read usage","namespace":"logger"}`))
	items, ok := result.Result.([]catalogOperation)
	if !result.OK || !ok || len(items) != 10 {
		t.Fatalf("search limit: %+v", result)
	}
	for _, item := range items {
		if item.Deprecated || item.OutputSchema != nil {
			t.Fatal("search included deprecated/detailed result")
		}
	}
	for _, raw := range []string{`{}`, `{"query":" "}`, `{"query":"usage","namespace":"missing"}`, `[]`, `{`} {
		if p.catalogDispatch("torana_search", json.RawMessage(raw)).OK {
			t.Fatalf("invalid input accepted: %s", raw)
		}
	}
	if p.catalogDispatch("torana_describe", json.RawMessage(`{"namespace":"logger","cursor":"bad"}`)).OK {
		t.Fatal("bad cursor accepted")
	}
}
