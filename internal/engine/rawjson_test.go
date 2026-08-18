package engine

// Adversarial matrix for the three durable JSON wrappers (PR A commit 2,
// reassessment revision). Carried from round 1: strict construction
// (duplicates, surrogates, bad UTF-8, malformed, wrong shape), absence
// semantics, defensive copies, span-splicing mutation discipline (untouched
// lexemes/order/whitespace survive; set-then-delete byte-exact), UTF-8
// member keys, escape-equivalent lookup. New reference proofs: absent
// optional wrappers retain their type-level shape; requiredness is durable
// (zero = canonical `{}`); whitespace-only empty objects round-trip via the
// structural closing-brace path.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWrapperParseRejects(t *testing.T) {
	bad := []string{
		`{"a":1,"a":2}`,       // duplicate
		`{"a":1,"\u0061":2}`,  // escape-equivalent duplicate
		`{"a":{"b":1,"b":2}}`, // nested duplicate
		`{"a":"\ud800"}`,      // lone high surrogate
		`{"a":"\udc00"}`,      // lone low surrogate
		"{\"a\":\"\xff\"}",    // invalid UTF-8
		`{"a":`,               // malformed
		`{"a":1} {}`,          // trailing value
		`[1,2]`,               // wrong shape object
		`"str"`,               // scalar
		`null`,                // null
	}
	for _, raw := range bad {
		t.Run("object:"+raw, func(t *testing.T) {
			if _, err := ParseOptionalJSONObject([]byte(raw)); err == nil {
				t.Fatalf("ParseOptionalJSONObject accepted %q", raw)
			}
			if _, err := ParseRequiredJSONObject([]byte(raw)); err == nil {
				t.Fatalf("ParseRequiredJSONObject accepted %q", raw)
			}
		})
	}
	for _, raw := range []string{`{"a":1}`, `"str"`, `1`, `null`} {
		t.Run("array:"+raw, func(t *testing.T) {
			if _, err := ParseOptionalJSONArray([]byte(raw)); err == nil {
				t.Fatalf("ParseOptionalJSONArray accepted %q", raw)
			}
		})
	}
	// Strict asserted-wire parsing: nil/empty is refused for the REQUIRED
	// constructor (normalization builds the zero/canonical wrapper instead).
	if _, err := ParseRequiredJSONObject(nil); err == nil {
		t.Fatal("ParseRequiredJSONObject(nil) accepted")
	}
	if _, err := ParseRequiredJSONObject([]byte(``)); err == nil {
		t.Fatal("ParseRequiredJSONObject(empty) accepted")
	}
}

func TestWrapperParseAccepts(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"z":1,"a":2}`, `{"big":1e999,"int":18446744073709551615}`,
		`{ "a" : 1 , "b" : 2 }`, "  {\n\t } \r\n",
	} {
		o, err := ParseOptionalJSONObject([]byte(raw))
		if err != nil || o.IsAbsent() || o.String() != raw {
			t.Fatalf("ParseOptionalJSONObject(%q) = %q, %v", raw, o.String(), err)
		}
		r, err := ParseRequiredJSONObject([]byte(raw))
		if err != nil || r.String() != raw {
			t.Fatalf("ParseRequiredJSONObject(%q) = %q, %v", raw, r.String(), err)
		}
	}
	for _, raw := range []string{`[]`, `[{"a":1},{"b":2}]`, `[1e999, 18446744073709551615]`} {
		a, err := ParseOptionalJSONArray([]byte(raw))
		if err != nil || a.IsAbsent() || a.String() != raw {
			t.Fatalf("ParseOptionalJSONArray(%q) = %q, %v", raw, a.String(), err)
		}
	}
	// Absence semantics: optional constructors map nil/empty to ABSENT;
	// required never is.
	o, err := ParseOptionalJSONObject(nil)
	if err != nil || !o.IsAbsent() {
		t.Fatalf("ParseOptionalJSONObject(nil) = %v %v", o, err)
	}
	a, err := ParseOptionalJSONArray(nil)
	if err != nil || !a.IsAbsent() {
		t.Fatalf("ParseOptionalJSONArray(nil) = %v %v", a, err)
	}
	empty, err := ParseOptionalJSONObject([]byte(`{}`))
	if err != nil || empty.IsAbsent() {
		t.Fatalf("empty object must not be absent: %v %v", empty, err)
	}
}

func TestWrapperDefensiveCopies(t *testing.T) {
	src := []byte(`{"z":1,"a":2}`)
	o, err := ParseOptionalJSONObject(src)
	if err != nil {
		t.Fatal(err)
	}
	src[0] = '['
	if o.String() != `{"z":1,"a":2}` {
		t.Fatalf("authority aliases constructor input: %q", o.String())
	}
	b := o.Bytes()
	b[0] = '['
	if o.String() != `{"z":1,"a":2}` {
		t.Fatalf("authority aliases a Bytes view: %q", o.String())
	}
	vals, _, err := o.DecodeObject()
	if err != nil {
		t.Fatal(err)
	}
	vals["z"] = json.RawMessage(`999`)
	if o.String() != `{"z":1,"a":2}` {
		t.Fatalf("authority aliases a Decode view: %q", o.String())
	}
}

func TestWrapperSetMember(t *testing.T) {
	o, err := ParseOptionalJSONObject([]byte(`{"z":1e999,"a":18446744073709551615,"m":{"k":"v"}}`))
	if err != nil {
		t.Fatal(err)
	}
	upd, err := o.SetMember("a", json.RawMessage(`0.5`))
	if err != nil {
		t.Fatal(err)
	}
	if upd.String() != `{"z":1e999,"a":0.5,"m":{"k":"v"}}` {
		t.Fatalf("SetMember(existing) = %q", upd.String())
	}
	if !strings.Contains(upd.String(), "1e999") || !strings.Contains(upd.String(), `{"k":"v"}`) {
		t.Fatalf("untouched lexemes changed: %q", upd.String())
	}
	added, err := upd.SetMember("new", json.RawMessage(`1e999`))
	if err != nil {
		t.Fatal(err)
	}
	if added.String() != `{"z":1e999,"a":0.5,"m":{"k":"v"},"new":1e999}` {
		t.Fatalf("SetMember(new) = %q", added.String())
	}
	// Whitespace variant keeps the key's whitespace.
	ws, err := ParseOptionalJSONObject([]byte(`{ "a" : 1 , "b" : 2 }`))
	if err != nil {
		t.Fatal(err)
	}
	ws2, err := ws.SetMember("b", json.RawMessage(`9`))
	if err != nil {
		t.Fatal(err)
	}
	if ws2.String() != `{ "a" : 1 , "b" : 9 }` {
		t.Fatalf("whitespace variant = %q", ws2.String())
	}
	// Optional absent materializes deterministically; required zero too.
	mat, err := (OptionalJSONObject{}).SetMember("k", json.RawMessage(`1`))
	if err != nil || mat.String() != `{"k":1}` {
		t.Fatalf("optional absent materialize = %q %v", mat.String(), err)
	}
	rmat, err := (RequiredJSONObject{}).SetMember("k", json.RawMessage(`1`))
	if err != nil || rmat.String() != `{"k":1}` {
		t.Fatalf("required zero materialize = %q %v", rmat.String(), err)
	}
	// Invalid values and keys refused.
	if _, err := o.SetMember("a", json.RawMessage(`{"d":1,"d":2}`)); err == nil {
		t.Fatal("duplicate-key member value accepted")
	}
	if _, err := o.SetMember(string([]byte{0xff}), json.RawMessage(`1`)); err == nil {
		t.Fatal("invalid UTF-8 key accepted")
	}
}

func TestWrapperDeleteMember(t *testing.T) {
	o, err := ParseOptionalJSONObject([]byte(`{"z":1e999,"a":2,"m":{"k":"v"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, want string
	}{
		{"a", `{"z":1e999,"m":{"k":"v"}}`},
		{"z", `{"a":2,"m":{"k":"v"}}`},
		{"m", `{"z":1e999,"a":2}`},
	} {
		del, err := o.DeleteMember(tc.key)
		if err != nil {
			t.Fatal(err)
		}
		if del.String() != tc.want {
			t.Fatalf("DeleteMember(%s) = %q, want %q", tc.key, del.String(), tc.want)
		}
	}
	single, err := ParseOptionalJSONObject([]byte(`{"only":1}`))
	if err != nil {
		t.Fatal(err)
	}
	gone, err := single.DeleteMember("only")
	if err != nil {
		t.Fatal(err)
	}
	if gone.String() != `{}` {
		t.Fatalf("DeleteMember(only) = %q", gone.String())
	}
	// Missing key: no-op, identical bytes.
	noop, err := o.DeleteMember("nope")
	if err != nil || noop.String() != o.String() {
		t.Fatalf("DeleteMember(missing) = %q %v", noop.String(), err)
	}
	// Optional absent: no-op. Required zero: no-op yielding canonical {}.
	if _, err := (OptionalJSONObject{}).DeleteMember("x"); err != nil {
		t.Fatalf("optional absent delete: %v", err)
	}
	rz, err := (RequiredJSONObject{}).DeleteMember("x")
	if err != nil || rz.String() != `{}` {
		t.Fatalf("required zero delete = %q %v", rz.String(), err)
	}
}

func TestWrapperSetDeleteRoundTripByteExact(t *testing.T) {
	for _, original := range []string{
		`{"z":1e999,"a":18446744073709551615,"m":{"k":"v"},"n":1.0}`,
		`{ "a":1 , "b":2 }`,
		"  {\n\t \"a\" : 1 , \"b\" : 2 } \r\n",
	} {
		o, err := ParseOptionalJSONObject([]byte(original))
		if err != nil {
			t.Fatal(err)
		}
		with, err := o.SetMember("tmp", json.RawMessage(`"x"`))
		if err != nil {
			t.Fatal(err)
		}
		back, err := with.DeleteMember("tmp")
		if err != nil {
			t.Fatal(err)
		}
		if back.String() != original {
			t.Fatalf("round trip not byte-exact:\n got %q\nwant %q", back.String(), original)
		}
	}
}

func TestOptionalJSONObjectWithoutMembersSinglePass(t *testing.T) {
	for _, row := range []struct {
		name string
		raw  string
		keys []string
		want string
	}{
		{name: "none", raw: `{ "a" : 1 , "b" : 2 }`, keys: []string{"missing"}, want: `{ "a" : 1 , "b" : 2 }`},
		{name: "first", raw: "{ \"a\" : 1 ,\n\t\"b\" : 2 , \"c\":3 }", keys: []string{"a"}, want: `{ "b" : 2, "c":3 }`},
		{name: "middle", raw: "{ \"a\" : 1 ,\n\t\"b\" : 2 , \"c\":3 }", keys: []string{"b"}, want: `{ "a" : 1, "c":3 }`},
		{name: "last", raw: "{ \"a\" : 1 ,\n\t\"b\" : 2 , \"c\":3 }", keys: []string{"c"}, want: "{ \"a\" : 1,\n\t\"b\" : 2 }"},
		{name: "separated", raw: "{ \"a\" : 1 ,\n\t\"b\" : {\"raw\":1e999} , \"c\":3, \"d\":\"x\" }", keys: []string{"b", "c"}, want: `{ "a" : 1, "d":"x" }`},
		{name: "all", raw: "  {\n\t\"a\":1, \"b\":2 } \r\n", keys: []string{"a", "b"}, want: "  {\n\t } \r\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			o, err := ParseOptionalJSONObject([]byte(row.raw))
			if err != nil {
				t.Fatal(err)
			}
			got, err := o.WithoutMembers(row.keys...)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != row.want {
				t.Fatalf("WithoutMembers = %q, want %q", got.String(), row.want)
			}
			if o.String() != row.raw {
				t.Fatalf("input mutated: %q", o.String())
			}
		})
	}

	o := OptionalJSONObject{}
	if _, err := o.WithoutMembers(string([]byte{0xff})); err == nil {
		t.Fatal("absent object accepted an invalid UTF-8 key")
	}
}

// Reference proof 3: whitespace-only empty objects round-trip exactly via
// the structural closing-brace path.
func TestWrapperEmptyObjectWhitespaceRoundTrip(t *testing.T) {
	for _, original := range []string{
		`{ }`,
		"  {\n\t } \r\n",
		"{\n\t}",
	} {
		o, err := ParseOptionalJSONObject([]byte(original))
		if err != nil {
			t.Fatal(err)
		}
		with, err := o.SetMember("x", json.RawMessage(`1`))
		if err != nil {
			t.Fatal(err)
		}
		back, err := with.DeleteMember("x")
		if err != nil {
			t.Fatal(err)
		}
		if back.String() != original {
			t.Fatalf("whitespace empty round trip not byte-exact:\n got %q\nwant %q", back.String(), original)
		}
	}
	// Required zero round trip: materialize then delete -> canonical {}.
	with, err := (RequiredJSONObject{}).SetMember("x", json.RawMessage(`1`))
	if err != nil {
		t.Fatal(err)
	}
	back, err := with.DeleteMember("x")
	if err != nil {
		t.Fatal(err)
	}
	if back.String() != `{}` {
		t.Fatalf("required zero round trip = %q", back.String())
	}
}

// Reference proof 1: absent optional wrappers retain their type-level
// shape — Replace of the wrong top-level type is refused even while absent.
func TestWrapperAbsentShapeIdentity(t *testing.T) {
	obj, err := ParseOptionalJSONObject(nil)
	if err != nil || !obj.IsAbsent() {
		t.Fatal(err)
	}
	if _, err := obj.Replace([]byte(`[1]`)); err == nil {
		t.Fatal("absent optional object replaced by an array")
	}
	if _, err := obj.Replace([]byte(`{}`)); err != nil {
		t.Fatalf("absent optional object replaced by an object: %v", err)
	}
	if _, err := obj.SetMember("k", json.RawMessage(`1`)); err != nil {
		t.Fatalf("absent optional object materialize: %v", err)
	}
	if _, err := obj.DeleteMember("k"); err != nil {
		t.Fatalf("absent optional object delete no-op: %v", err)
	}
	// Replace(nil) returns to absence.
	back, err := obj.Replace(nil)
	if err != nil || !back.IsAbsent() {
		t.Fatalf("Replace(nil) must return to absence: %v %v", back, err)
	}
	arr, err := ParseOptionalJSONArray(nil)
	if err != nil || !arr.IsAbsent() {
		t.Fatal(err)
	}
	if _, err := arr.Replace([]byte(`{}`)); err == nil {
		t.Fatal("absent optional array replaced by an object")
	}
	if _, err := arr.Replace([]byte(`[]`)); err != nil {
		t.Fatalf("absent optional array replaced by an array: %v", err)
	}
}

// Reference proof 2: requiredness is durable — the zero value is the
// canonical {}, Replace(nil) is refused, and the shape never changes.
func TestWrapperRequiredDurable(t *testing.T) {
	z := RequiredJSONObject{}
	if z.String() != `{}` || string(z.Bytes()) != `{}` {
		t.Fatalf("zero required = %q", z.String())
	}
	vals, order, err := z.DecodeObject()
	if err != nil || len(vals) != 0 || len(order) != 0 {
		t.Fatalf("zero required DecodeObject = %v %v %v", vals, order, err)
	}
	if _, err := z.Replace(nil); err == nil {
		t.Fatal("required Replace(nil) accepted")
	}
	if _, err := z.Replace([]byte(``)); err == nil {
		t.Fatal("required Replace(empty) accepted")
	}
	if _, err := z.Replace([]byte(`[1]`)); err == nil {
		t.Fatal("required Replace(array) accepted")
	}
	// Requiredness survives Replace.
	r, err := z.Replace([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Replace(nil); err == nil {
		t.Fatal("required Replace(nil) after Replace accepted")
	}
	// Deleting the last member yields the canonical {}.
	del, err := r.DeleteMember("a")
	if err != nil || del.String() != `{}` {
		t.Fatalf("last delete = %q %v", del.String(), err)
	}
	// Zero-value Bytes() is a defensive copy.
	b := z.Bytes()
	b[0] = '['
	if z.String() != `{}` {
		t.Fatalf("zero required Bytes aliased: %q", z.String())
	}
}

func TestWrapperDecodeViews(t *testing.T) {
	o, err := ParseOptionalJSONObject([]byte(`{"big":1e999,"i":18446744073709551615,"o":1.0,"s":"\ud83d\ude00"}`))
	if err != nil {
		t.Fatal(err)
	}
	vals, order, err := o.DecodeObject()
	if err != nil {
		t.Fatal(err)
	}
	if string(vals["big"]) != "1e999" || string(vals["i"]) != "18446744073709551615" || string(vals["o"]) != "1.0" {
		t.Fatalf("lexemes changed: %q %q %q", vals["big"], vals["i"], vals["o"])
	}
	if order[0] != "big" || order[1] != "i" || order[2] != "o" || order[3] != "s" {
		t.Fatalf("wire order lost: %v", order)
	}
	arr, err := ParseOptionalJSONArray([]byte(`[1e999, 2]`))
	if err != nil {
		t.Fatal(err)
	}
	els, err := arr.DecodeArray()
	if err != nil {
		t.Fatal(err)
	}
	if string(els[0]) != "1e999" {
		t.Fatalf("array lexeme changed: %q", els[0])
	}
	// Array authorities have no object view at all (type-level shape).
	if _, _, err := o.DecodeObject(); err != nil {
		t.Fatalf("DecodeObject on an object failed: %v", err)
	}
	// (Array views exist only on OptionalJSONArray; object authorities have
	// no array view — type-level shape.)
	if _, err := (OptionalJSONArray{}).DecodeArray(); err == nil {
		t.Fatal("DecodeArray on absent accepted")
	}
}

func TestWrapperEscapeEquivalentLookup(t *testing.T) {
	esc, err := ParseOptionalJSONObject([]byte(`{"\u0061":1,"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	upd, err := esc.SetMember("a", json.RawMessage(`9`))
	if err != nil {
		t.Fatal(err)
	}
	if upd.String() != `{"\u0061":9,"b":2}` {
		t.Fatalf("escape-equivalent update = %q", upd.String())
	}
	del, err := esc.DeleteMember("a")
	if err != nil {
		t.Fatal(err)
	}
	if del.String() != `{"b":2}` {
		t.Fatalf("escape-equivalent delete = %q", del.String())
	}
	// Valid non-ASCII and JSON-hostile keys round trip.
	o, err := ParseOptionalJSONObject([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"héllo", `a"b`, `a\b`, "ctrl\x01", "雪"} {
		with, err := o.SetMember(k, json.RawMessage(`1`))
		if err != nil {
			t.Fatalf("SetMember(%q): %v", k, err)
		}
		back, err := with.DeleteMember(k)
		if err != nil {
			t.Fatalf("DeleteMember(%q): %v", k, err)
		}
		if back.String() != `{}` {
			t.Fatalf("key %q round trip = %q", k, back.String())
		}
	}
}

// TestWrapperRequiredLastDeleteCanonical: deleting the last member of a
// REQUIRED object yields the canonical `{}` regardless of the original
// spelling (compact, spaced, leading/trailing CRLF, escape-spelled key,
// already-empty). The optional wrapper keeps its exact preserved spelling.
func TestWrapperRequiredLastDeleteCanonical(t *testing.T) {
	rows := []struct {
		name string
		raw  string
		key  string
	}{
		{"compact", `{"a":1}`, "a"},
		{"spaced", `{ "a":1 }`, "a"},
		{"leading trailing crlf", "  {\n\t\"a\":1\n} \r\n", "a"},
		{"escape-spelled key", `{"\u0061":1}`, "a"},
		{"already empty spaced", `{ }`, "x"},
		{"already empty crlf", "  {\n\t} \r\n", "x"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			r, err := ParseRequiredJSONObject([]byte(row.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			del, err := r.DeleteMember(row.key)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if del.String() != `{}` || string(del.Bytes()) != `{}` {
				t.Fatalf("required last-delete = %q, want canonical {}", del.String())
			}
		})
	}
	// Multi-member required object: deleting down to empty also canonicalizes.
	r, err := ParseRequiredJSONObject([]byte(`{ "a":1 , "b":2 }`))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b"} {
		r, err = r.DeleteMember(k)
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.String() != `{}` {
		t.Fatalf("required delete-all = %q, want {}", r.String())
	}
	// Optional present empty keeps its preserved spelling (still present).
	o, err := ParseOptionalJSONObject([]byte(`{ "a":1 }`))
	if err != nil {
		t.Fatal(err)
	}
	od, err := o.DeleteMember("a")
	if err != nil {
		t.Fatal(err)
	}
	if od.String() != `{  }` || od.IsAbsent() {
		t.Fatalf("optional last-delete = %q absent=%v, want preserved {  }", od.String(), od.IsAbsent())
	}
}

// TestWrapperDeleteKeyValidationStateIndependent: the UTF-8 key rule runs
// BEFORE the absent/zero no-op on both wrappers, so invalid keys are refused
// on absent, zero, empty, and populated authorities alike.
func TestWrapperDeleteKeyValidationStateIndependent(t *testing.T) {
	bad := string([]byte{0xff})
	bad2 := string([]byte{0xc3, 0x28})
	populated, err := ParseOptionalJSONObject([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	rpop, err := ParseRequiredJSONObject([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		del  func(string) error
	}{
		{"optional absent", func(k string) error { _, e := (OptionalJSONObject{}).DeleteMember(k); return e }},
		{"optional empty", func(k string) error {
			empty, e := ParseOptionalJSONObject([]byte(`{}`))
			if e != nil {
				return e
			}
			_, e = empty.DeleteMember(k)
			return e
		}},
		{"optional populated", func(k string) error { _, e := populated.DeleteMember(k); return e }},
		{"required zero", func(k string) error { _, e := (RequiredJSONObject{}).DeleteMember(k); return e }},
		{"required populated", func(k string) error { _, e := rpop.DeleteMember(k); return e }},
	}
	for _, c := range cases {
		for _, k := range []string{bad, bad2} {
			t.Run(c.name, func(t *testing.T) {
				if err := c.del(k); err == nil {
					t.Fatal("invalid UTF-8 delete key accepted")
				}
			})
		}
	}
	// Valid non-ASCII, control, and escape-equivalent keys still work on the
	// zero/absent states (no-op) and populated states.
	for _, k := range []string{"héllo", "ctrl\x01", "雪"} {
		if _, err := (OptionalJSONObject{}).DeleteMember(k); err != nil {
			t.Fatalf("valid key %q on absent refused: %v", k, err)
		}
		if _, err := (RequiredJSONObject{}).DeleteMember(k); err != nil {
			t.Fatalf("valid key %q on zero refused: %v", k, err)
		}
	}
	if _, err := populated.DeleteMember("a"); err != nil {
		t.Fatalf("escape-equivalent delete on populated: %v", err)
	}
}

// TestWrapperWhitespaceDeletionMatrix: DIRECT deletion of original
// first/middle/last/only members with whitespace on both sides of commas,
// tab/CR/LF separators, leading/trailing document whitespace, and
// escape-heavy nested values — against BOTH wrappers, with wrapper-specific
// empty-result expectations.
func TestWrapperWhitespaceDeletionMatrix(t *testing.T) {
	rows := []struct {
		name    string
		raw     string
		delKey  string
		wantOpt string // optional wrapper result
		wantReq string // required wrapper result (canonical {} when empty)
	}{
		{"first spaced", `{ "a":1 , "b":2 , "c":3 }`, "a", `{ "b":2 , "c":3 }`, `{ "b":2 , "c":3 }`},
		{"middle spaced", `{ "a":1 , "b":2 , "c":3 }`, "b", `{ "a":1 , "c":3 }`, `{ "a":1 , "c":3 }`},
		{"last spaced", `{ "a":1 , "b":2 , "c":3 }`, "c", `{ "a":1 , "b":2 }`, `{ "a":1 , "b":2 }`},
		{"only spaced", `{ "only" : 1 }`, "only", `{  }`, `{}`},
		{"first tabs", "{\n\t\"a\":1,\r\n\t\"b\":2\n}", "a", "{\n\t\"b\":2\n}", "{\n\t\"b\":2\n}"},
		{"middle tabs", "{\n\t\"a\":1,\r\n\t\"b\":2,\n\t\"c\":3\n}", "b", "{\n\t\"a\":1,\n\t\"c\":3\n}", "{\n\t\"a\":1,\n\t\"c\":3\n}"},
		{"last crlf", "{\r\n\"a\":1,\r\n\"b\":2\r\n}", "b", "{\r\n\"a\":1\r\n}", "{\r\n\"a\":1\r\n}"},
		{"leading trailing ws", `  { "a":1 , "b":2 }  `, "a", `  { "b":2 }  `, `  { "b":2 }  `},
		{"escape-heavy nested", `{"m":{"k":"v","z":"\ud83d\ude00"},"a":1e999}`, "m", `{"a":1e999}`, `{"a":1e999}`},
		{"escape-spelled key", `{"\u0061":1,"b":2}`, "a", `{"b":2}`, `{"b":2}`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			o, err := ParseOptionalJSONObject([]byte(row.raw))
			if err != nil {
				t.Fatalf("optional parse: %v", err)
			}
			del, err := o.DeleteMember(row.delKey)
			if err != nil {
				t.Fatalf("optional delete: %v", err)
			}
			if del.String() != row.wantOpt {
				t.Fatalf("optional delete(%s) = %q, want %q", row.delKey, del.String(), row.wantOpt)
			}
			r, err := ParseRequiredJSONObject([]byte(row.raw))
			if err != nil {
				t.Fatalf("required parse: %v", err)
			}
			rdel, err := r.DeleteMember(row.delKey)
			if err != nil {
				t.Fatalf("required delete: %v", err)
			}
			if rdel.String() != row.wantReq {
				t.Fatalf("required delete(%s) = %q, want %q", row.delKey, rdel.String(), row.wantReq)
			}
		})
	}
}
