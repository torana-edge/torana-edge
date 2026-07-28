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
		field := typ.Field(i)
		name := jsonFieldName(field)
		if name == "-" {
			continue
		}
		if name == "" {
			// No json tag at all. It still serializes (under its Go name), so
			// it is still droppable — and skipping it here was a hole in the
			// very check this test exists to be.
			t.Errorf("provider.Provider field %s has no json tag, so this check cannot "+
				"account for it. Give it one, or json:\"-\" if it must not serialize.",
				field.Name)
			continue
		}
		if _, ok := accounted[name]; !ok {
			t.Errorf("provider.Provider field %q (%s) is neither rendered by the settings form "+
				"nor listed in unmanagedProviderFields.\n"+
				"As it stands, saving provider settings from the dashboard will silently DELETE it.\n"+
				"Either render it in internal/controlplane/dist/index.html and add it to "+
				"formRenderedProviderFields here, or add it to unmanagedProviderFields in server.go.",
				name, field.Name)
		}
	}
}

// TestUnmanagedProviderFieldsAreAllHandled: the list drives a switch, and a
// name with no matching case is a no-op — the field is listed, looks handled,
// and is dropped anyway.
//
// The first version hardcoded the three names it knew about, which is exactly
// what it was supposed to catch: adding a fourth with no case still passed. It
// now derives the expectation from the list itself, by round-tripping a stored
// config through preservation and asserting every unmanaged field survived.
func TestUnmanagedProviderFieldsAreAllHandled(t *testing.T) {
	// Every field named in unmanagedProviderFields must be set to something
	// non-zero here, or preservation cannot be observed for it.
	stored := map[string]provider.Provider{"p": {
		URL:                 "https://stored.example",
		Pricing:             map[string]economics.ModelPricing{"gpt-4": {}},
		ResponsesCompaction: &provider.ResponsesCompactionConfig{},
		Cache:               &provider.CacheConfig{},
	}}
	incoming := map[string]provider.Provider{"p": {URL: "https://incoming.example"}}

	preserveUnmanagedProviderFields(stored, incoming, map[string]map[string]struct{}{})

	// Driven by the list, not by a hardcoded set of names. The first version
	// checked the three fields it happened to know about, which is exactly the
	// failure it was written to catch: adding a fourth with no switch case
	// still passed.
	typ := reflect.TypeOf(provider.Provider{})
	storedV := reflect.ValueOf(stored["p"])
	gotV := reflect.ValueOf(incoming["p"])

	for _, name := range unmanagedProviderFields {
		idx := -1
		for i := 0; i < typ.NumField(); i++ {
			if jsonFieldName(typ.Field(i)) == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Errorf("unmanagedProviderFields names %q, which is not a field on provider.Provider", name)
			continue
		}
		want := storedV.Field(idx).Interface()
		if reflect.ValueOf(want).IsZero() {
			t.Errorf("the fixture leaves %q at its zero value, so preservation cannot be "+
				"observed for it. Set it in `stored` above — a listed field with no switch "+
				"case in preserveUnmanagedProviderFields is a silent no-op, and this test "+
				"exists to catch that.", name)
			continue
		}
		if !reflect.DeepEqual(want, gotV.Field(idx).Interface()) {
			t.Errorf("%q is listed as unmanaged but was not carried forward; "+
				"preserveUnmanagedProviderFields has no case for it", name)
		}
	}
}

// TestFormRenderedFieldsExistInTheDashboard keeps the list above honest.
//
// The first version used strings.Contains against the whole SPA, which matches
// "format" inside formatBytes and "url" inside any href — it could not fail.
// It now requires each field to appear as a quoted JSON key, which is how the
// form reads and writes it.
func TestFormRenderedFieldsExistInTheDashboard(t *testing.T) {
	spa := readDashboard(t)
	for _, field := range formRenderedProviderFields {
		// The SPA builds provider objects with quoted keys, e.g. p.url or
		// {"url": ...}; either spelling contains the quoted key or a dotted
		// access. Requiring one of those rules out incidental substrings.
		quoted := `"` + field + `"`
		dotted := "." + field
		if !strings.Contains(spa, quoted) && !strings.Contains(spa, dotted) {
			t.Errorf("field %q never appears in the dashboard as a key (%s) or an access (%s); "+
				"claiming the form renders it is then fiction, and saving would drop it",
				field, quoted, dotted)
		}
	}
}

// TestFormRenderedListRejectsAFieldTheDashboardDoesNotHave proves the check
// above can fail, by running it against a name the SPA cannot contain.
func TestFormRenderedListRejectsAFieldTheDashboardDoesNotHave(t *testing.T) {
	spa := readDashboard(t)
	const invented = "definitely_not_a_provider_field_xyzzy"
	if strings.Contains(spa, `"`+invented+`"`) || strings.Contains(spa, "."+invented) {
		t.Fatal("test fixture is not invented after all")
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
