package proxy

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

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
	"auth",
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
	// The fixture is built by reflection from unmanagedProviderFields itself,
	// so adding a field to that list needs no edit here.
	//
	// A hand-written fixture couples this test to every PR that adds an
	// unmanaged field: the two would merge without a textual conflict and land
	// a red main, because neither PR's CI sees the other's change. That is a
	// mechanical failure, not a real finding, and it would train people to
	// treat this test as noise.
	stored := provider.Provider{URL: "https://stored.example"}
	sv := reflect.ValueOf(&stored).Elem()
	typ := sv.Type()

	for _, name := range unmanagedProviderFields {
		idx := fieldIndexByJSONName(typ, name)
		if idx < 0 {
			t.Errorf("unmanagedProviderFields names %q, which is not a field on provider.Provider", name)
			continue
		}
		if !setNonZero(sv.Field(idx)) {
			t.Errorf("cannot build a non-zero %s for %q, so preservation cannot be observed; "+
				"extend setNonZero", sv.Field(idx).Type(), name)
		}
	}

	incoming := map[string]provider.Provider{"p": {URL: "https://incoming.example"}}
	preserveUnmanagedProviderFields(map[string]provider.Provider{"p": stored}, incoming,
		map[string]map[string]struct{}{})

	gotV := reflect.ValueOf(incoming["p"])
	for _, name := range unmanagedProviderFields {
		idx := fieldIndexByJSONName(typ, name)
		if idx < 0 {
			continue
		}
		want := sv.Field(idx).Interface()
		if reflect.ValueOf(want).IsZero() {
			continue // already reported above
		}
		if !reflect.DeepEqual(want, gotV.Field(idx).Interface()) {
			t.Errorf("%q is listed as unmanaged but was not carried forward; "+
				"preserveUnmanagedProviderFields has no case for it", name)
		}
	}
}

func fieldIndexByJSONName(typ reflect.Type, name string) int {
	for i := 0; i < typ.NumField(); i++ {
		if jsonFieldName(typ.Field(i)) == name {
			return i
		}
	}
	return -1
}

// setNonZero gives v a value distinguishable from its zero, so preservation is
// observable. It reports false for a kind it cannot construct, which is a
// prompt to extend it rather than a silent pass.
func setNonZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("non-zero")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem())
		v.Set(m)
	case reflect.Slice:
		v.Set(reflect.Append(v, reflect.New(v.Type().Elem()).Elem()))
	default:
		return false
	}
	return !v.IsZero()
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
		if !dashboardReferences(spa, field) {
			t.Errorf("field %q never appears in the dashboard as a property access or a "+
				"quoted key; claiming the form renders it is then fiction, and saving "+
				"would drop it", field)
		}
	}
}

// dashboardReferences reports whether the SPA reads or writes this field.
//
// The SPA uses property access (p.url, prov.auth) rather than quoted
// keys, so both spellings are accepted — but only as WHOLE identifiers. A plain
// substring search matched "format" inside formatBytes and "url" inside any
// href, which is how the first version of this check could not fail; a bare
// `strings.Contains(spa, "."+field)` had the same hole one layer down.
func dashboardReferences(spa, field string) bool {
	pattern := regexp.MustCompile(`(\.|["'` + "`" + `])` + regexp.QuoteMeta(field) + `\b`)
	return pattern.MatchString(spa)
}

// TestFormRenderedListRejectsAFieldTheDashboardDoesNotHave proves the check
// above can fail, and that it does not match a longer identifier that merely
// starts with the field name — the exact hole the substring version had.
func TestFormRenderedListRejectsAFieldTheDashboardDoesNotHave(t *testing.T) {
	spa := readDashboard(t)

	if dashboardReferences(spa, "definitely_not_a_provider_field_xyzzy") {
		t.Error("matched a field the dashboard cannot contain")
	}
	// "format" is a real field; "formatBytes" is a helper. A check that cannot
	// tell them apart would pass for a field the form does not render.
	if dashboardReferences("const x = formatBytes(n);", "format") {
		t.Error("`.formatBytes(` was accepted as a reference to the `format` field")
	}
	if !dashboardReferences("p.format = 'openai';", "format") {
		t.Error("a genuine property access was not recognised")
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

func TestDashboardSurfacesPluginCompositionContracts(t *testing.T) {
	dashboard := readDashboard(t)
	for _, marker := range []string{
		"p.requires_upstream",
		"p.conflicts_with",
		"other.conflicts_with",
		"Cannot run with:",
		"their manifests declare a plugin conflict",
	} {
		if !strings.Contains(dashboard, marker) {
			t.Errorf("dashboard does not surface/enforce plugin composition contract: missing %q", marker)
		}
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
