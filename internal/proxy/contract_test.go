package proxy

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// Contracts that span a boundary the compiler does not check: Go struct to
// dashboard form, host to guest, server to SPA. Each has already been broken
// once or is one careless commit from it, and each breaks silently — which is
// why they are pinned here rather than left to review.

// formRenderedProviderFields are the provider fields the settings form renders
// and submits. Kept explicit rather than inferred: the point is that adding a
// field to provider.Provider must force a decision, and a heuristic that
// guesses would quietly answer it for you.
var formRenderedProviderFields = []string{
	"url",
	"format",
	"fallback",
	"api_key_env",
	"api_key_enc",
}

// TestEveryProviderFieldIsRenderedOrPreserved is the highest-value test here.
//
// The dashboard rebuilds each provider object from its form and PUTs it. Any
// field the form does not render is absent from that object, so unless it is
// carried forward by preserveUnmanagedProviderFields it is DELETED — silently,
// on the next save, from a form the operator may not even have touched.
//
// Pricing, cache configuration and Responses compaction settings all live in
// fields like that. Losing pricing means cost accounting silently reports zero;
// losing cache config means the warmer and tier selector stop having anything
// to reason about.
//
// Adding a field to provider.Provider without doing either is a one-line change
// with no visible symptom until a user's settings vanish. This makes it fail.
func TestEveryProviderFieldIsRenderedOrPreserved(t *testing.T) {
	accounted := make(map[string]string) // json name -> how it is handled
	for _, f := range formRenderedProviderFields {
		accounted[f] = "rendered by the settings form"
	}
	for _, f := range unmanagedProviderFields {
		if how, dup := accounted[f]; dup {
			t.Errorf("field %q is both %s and listed in unmanagedProviderFields; "+
				"preservation would overwrite what the form submitted", f, how)
		}
		accounted[f] = "preserved by preserveUnmanagedProviderFields"
	}

	typ := reflect.TypeOf(provider.Provider{})
	for i := 0; i < typ.NumField(); i++ {
		name := jsonFieldName(typ.Field(i))
		if name == "" || name == "-" {
			continue
		}
		if _, ok := accounted[name]; !ok {
			t.Errorf("provider.Provider field %q (%s) is neither rendered by the settings form "+
				"nor listed in unmanagedProviderFields.\n"+
				"As it stands, saving provider settings from the dashboard will silently DELETE it.\n"+
				"Either render it in internal/controlplane/dist/index.html and add it to "+
				"formRenderedProviderFields here, or add it to unmanagedProviderFields in server.go.",
				name, typ.Field(i).Name)
		}
	}
}

// TestUnmanagedProviderFieldsAreAllHandled: the list drives a switch, and a
// name with no matching case is a no-op — the field is listed, looks handled,
// and is dropped anyway.
func TestUnmanagedProviderFieldsAreAllHandled(t *testing.T) {
	stored := map[string]provider.Provider{"p": {
		URL:                 "https://stored.example",
		Pricing:             map[string]economics.ModelPricing{"gpt-4": {}},
		ResponsesCompaction: &provider.ResponsesCompactionConfig{},
		Cache:               &provider.CacheConfig{},
	}}
	incoming := map[string]provider.Provider{"p": {URL: "https://incoming.example"}}

	preserveUnmanagedProviderFields(stored, incoming, map[string]map[string]struct{}{})

	got := incoming["p"]
	if got.ResponsesCompaction == nil {
		t.Error(`"responses_compaction" is listed as unmanaged but was not carried forward`)
	}
	if got.Cache == nil {
		t.Error(`"cache" is listed as unmanaged but was not carried forward`)
	}
	if got.Pricing == nil {
		t.Error(`"pricing" is listed as unmanaged but was not carried forward`)
	}
}

// TestFormRenderedFieldsExistInTheDashboard keeps the list above honest. It
// asserts each claimed field really appears in the SPA, so the list cannot
// silently become fiction — which would make the test above pass while the
// field is dropped in practice.
func TestFormRenderedFieldsExistInTheDashboard(t *testing.T) {
	spa := readDashboard(t)
	for _, field := range formRenderedProviderFields {
		if !strings.Contains(spa, field) {
			t.Errorf("field %q is claimed to be rendered by the settings form, but does not appear "+
				"in the dashboard at all — saving would drop it", field)
		}
	}
}

// TestSecretSentinelMatchesDashboard pins the sentinel the server sends in place
// of a stored secret. The SPA must send it back unchanged to mean "unchanged";
// if the two ever disagree the SPA writes the literal string as the new secret,
// destroying the real one.
func TestSecretSentinelMatchesDashboard(t *testing.T) {
	if !strings.Contains(readDashboard(t), secretSetSentinel) {
		t.Errorf("the dashboard does not mention the secret sentinel %q. The server sends it in "+
			"place of a stored secret and expects it back verbatim to mean 'unchanged' — a "+
			"dashboard that does not recognise it will overwrite the real secret with this string.",
			secretSetSentinel)
	}
}

func readDashboard(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../controlplane/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return ""
	}
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	return tag
}
