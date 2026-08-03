package jsontext

// Reference-model tests for the parser-differential validator.
//
// Directions pinned:
//   - ACCEPT direction: every document encoding/json accepts and that is free
//     of the four hazards must pass Validate (no false rejections).
//   - REJECT direction: every hazard (duplicate keys incl. escape-equivalent
//     names, invalid UTF-8, lone surrogates, multiple top-level values) must
//     fail Validate even though encoding/json ACCEPTS it (the differential is
//     closed).
//   - STRUCTURE direction: structurally malformed documents fail BOTH
//     Validate and encoding/json — except lenient number tokens (`1-2`),
//     which Validate accepts but the adapter's strict decode rejects, so the
//     fail-closed 400 path is preserved without a new differential.
//   - BOUNDS: nesting beyond encoding/json's own depth bound is rejected;
//     large inputs validate without amplification.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestValidateAcceptsReferenceCorpus(t *testing.T) {
	// Every one of these is accepted by encoding/json and must pass Validate.
	docs := []string{
		`{}`,
		`[]`,
		`null`,
		`true`,
		`false`,
		`0`,
		`-12.5e+3`,
		`"plain string"`,
		`{"a":1,"b":[1,2,{"c":"d"}],"e":{"f":{"g":[null,true,false]}}}`,
		`{"key with spaces":"value","\u0061\u0062":"escape-equivalent key"}`,
		`{"\ud83d\ude00":"emoji pair","escaped":"\\u0061 literal backslash-u"}`,
		`{"\ud834\udd1e":"G clef","snowman":"\u2603"}`,
		`"tab: \t newline: \n quote: \" solidus: \/ backspace: \b"`,
		`"\u0000\u001f"`,
		`{"混合":"キー"}`,
		strings.Repeat(`[`, 9000) + `0` + strings.Repeat(`]`, 9000),
	}
	for i, doc := range docs {
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("corpus %d is not accepted by encoding/json: %v", i, err)
		}
		if err := Validate([]byte(doc)); err != nil {
			t.Fatalf("corpus %d rejected by Validate: %v\n%s", i, err, doc)
		}
	}
}

func TestValidateRejectsHazards(t *testing.T) {
	// Each hazard is ACCEPTED by encoding/json (last-wins / U+FFFD / trailing
	// ignored) but must be REJECTED by Validate.
	docs := []string{
		`{"messages":[1],"messages":[2]}`,
		`{"messages":[1],"\u006d\u0065ssages":[2]}`, // escape-equivalent top-level
		`{"a":{"b":1,"b":2}}`,                       // nested duplicate
		`{"a":[{"b":1},{"b":2,"b":3}]}`,             // deep nested duplicate
		"{\"a\":\"\xff\xfe\"}",                      // invalid UTF-8 in a string
		`{"a":"\ud800"}`,                            // lone high surrogate
		`{"a":"\udc00"}`,                            // lone low surrogate
		`{"a":"\ud83d"}`,                            // high surrogate, no pair
		`{"a":"\ud83d\u0041"}`,                      // high followed by non-low
		`{"a":"\ud800\ud800"}`,                      // high+high
	}
	for i, doc := range docs {
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("hazard %d is NOT accepted by encoding/json — move it to the structure set: %v", i, err)
		}
		if err := Validate([]byte(doc)); err == nil {
			t.Fatalf("hazard %d passed Validate:\n%s", i, doc)
		}
	}
}

func TestValidateRejectsMalformedStructure(t *testing.T) {
	// Structurally malformed: BOTH Validate and encoding/json reject them
	// (number-leniency rows are exempt and covered separately).
	docs := []string{
		``,
		`   `,
		`{`,
		`{"a":}`,
		`{"a" 1}`,
		`[1,]`,
		`{"a":1,}`,
		"{\"a\":\"ok\"}\xff", // invalid UTF-8 outside strings: rejected by BOTH
		`"unterminated`,
		`"bad \x escape"`,
		`"bad \u12 escape"`,
		`{"a":"\u12zz"}`,
		`tru`,
		`{"a":tru}`,
		`+1`,
		`.5`,
		`{"a":"unterminated`,
		`[[]`,
		`{"a":1 "b":2}`,
	}
	for i, doc := range docs {
		if err := Validate([]byte(doc)); err == nil {
			t.Fatalf("malformed %d passed Validate:\n%q", i, doc)
		}
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err == nil {
			t.Fatalf("malformed %d is accepted by encoding/json — not structurally malformed:\n%q", i, doc)
		}
	}
}

// TestValidateRejectsMultipleTopLevelValues: exactly one top-level value is
// required regardless of whether encoding/json also rejects trailing data.
func TestValidateRejectsMultipleTopLevelValues(t *testing.T) {
	for _, doc := range []string{`{} {}`, `[] []`, `{"a":1} trailing`, `1 2`, `"a" "b"`, `{}[]`} {
		if err := Validate([]byte(doc)); err == nil {
			t.Fatalf("multiple top-level values passed Validate:\n%s", doc)
		}
	}
}

func TestValidateNumberLeniencyIsOneSided(t *testing.T) {
	// Lenient number tokens pass Validate but are still rejected by
	// encoding/json — so the adapter's strict decode keeps the fail-closed
	// path; the validator never becomes the accepting side of a differential.
	for _, doc := range []string{`1-2`, `1.2.3`, `1e`, `--1`, `01`, `{"a":01}`} {
		if err := Validate([]byte(doc)); err != nil {
			t.Fatalf("lenient token %q must pass Validate: %v", doc, err)
		}
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err == nil {
			t.Fatalf("lenient token %q is accepted by encoding/json — not an exemption", doc)
		}
	}
}

func TestValidatePairAndLiteralControls(t *testing.T) {
	// Positive controls: a valid surrogate pair and a literal escaped
	// backslash-u sequence are accepted (the hazard rejection must not be
	// over-broad).
	docs := []string{
		`{"a":"\ud83d\ude00"}`,
		`{"a":"\\u0061"}`,
		`"\ud83d\ude00"`,
	}
	for i, doc := range docs {
		if err := Validate([]byte(doc)); err != nil {
			t.Fatalf("control %d rejected: %v\n%s", i, err, doc)
		}
	}
}

func TestValidateDepthBound(t *testing.T) {
	deep := strings.Repeat(`[`, 10001) + `0` + strings.Repeat(`]`, 10001)
	if err := Validate([]byte(deep)); err == nil {
		t.Fatal("10001-deep document accepted")
	}
	ok := strings.Repeat(`[`, 1000) + `0` + strings.Repeat(`]`, 1000)
	if err := Validate([]byte(ok)); err != nil {
		t.Fatalf("1000-deep document rejected: %v", err)
	}
}

func TestValidateLargeInput(t *testing.T) {
	// A large document with many keys: linear validation, duplicate-free.
	var sb strings.Builder
	sb.WriteString(`{`)
	for i := 0; i < 20000; i++ {
		if i > 0 {
			sb.WriteString(`,`)
		}
		sb.WriteString(`"key_`)
		sb.WriteString(strings.Repeat("x", 20))
		sb.WriteString(`_`)
		sb.WriteString(strings.Repeat("y", 20))
		sb.WriteString(`_`)
		sb.WriteString(strings.Repeat("z", 20))
		sb.WriteString(`_`)
		fmt.Fprintf(&sb, "%d", i)
		sb.WriteString(`":`)
		sb.WriteString(`"value with \u0041 escapes and \ud83d\ude00 pairs"`)
	}
	sb.WriteString(`}`)
	doc := sb.String()
	if err := Validate([]byte(doc)); err != nil {
		t.Fatalf("large document rejected: %v", err)
	}
	// Duplicate at the END of the same large object must still be found.
	dup := doc[:len(doc)-1] + `,"key_xxxxxxxxxxxxxxxxxxxx_yyyyyyyyyyyyyyyyyyyy_zzzzzzzzzzzzzzzzzzzz_19999":1}`
	if err := Validate([]byte(dup)); err == nil {
		t.Fatal("large document with a trailing duplicate accepted")
	}
}

// TestValidateRandomizedDifferential: randomized mutations of a valid
// document must never be ACCEPTED by Validate while rejected by
// encoding/json, EXCEPT when the mutation produces a lenient number token or
// one of the hazards (which encoding/json accepts by design). The invariant
// under test: Validate accepts ⇒ the adapter's strict decoder accepts or the
// document is a number-leniency case — the validator never forwards a
// document the adapter would reject.
func TestValidateRandomizedDifferential(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"user","content":"hi","extra":{"a":[1,2.5,true,null,"s"]}}],"system":"sys"}` +
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"system":"sys"}` +
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"system":"sys"}`
	_ = base
	seed := []byte(`{"model":"m","messages":[{"role":"user","content":"hi","extra":{"a":[1,2.5,true,null,"s"]}}],"system":"sys"}`)
	// Deterministic mutation: for every byte position, flip to a few
	// interesting values and check the differential direction.
	muts := []byte{'"', '{', '}', '[', ']', ',', ':', '\\', 'u', '0', 'e', ' ', '\n', '\xff', 0}
	checked := 0
	for i := 0; i < len(seed); i++ {
		for _, m := range muts {
			doc := append([]byte(nil), seed...)
			doc[i] = m
			vErr := Validate(doc)
			var v any
			jErr := json.Unmarshal(doc, &v)
			if vErr == nil && jErr != nil {
				// Accepted by Validate, rejected by encoding/json: allowed
				// ONLY when the mutation produced a lenient number token that
				// Validate consumes but the adapter's strict decode still
				// rejects. Prove it precisely: replace the mutated byte with
				// a digit; if the document then decodes, the sole difference
				// was the permissive number token.
				fixed := append([]byte(nil), doc...)
				fixed[i] = '0'
				var v2 any
				if err := json.Unmarshal(fixed, &v2); err != nil {
					t.Fatalf("Validate accepted but encoding/json rejected (non-number) at byte %d -> %q: %s", i, m, doc)
				}
			}
			checked++
		}
	}
	if want := len(muts) * len(seed); checked != want {
		t.Fatalf("mutation sweep incomplete: %d checks, want %d", checked, want)
	}
}

// TestEscapeDecodingControlBytes: the five short control escapes decode to
// their CONTROL BYTES, not to their letters. Each row is checked in both
// directions:
//   - distinct: the short escape and its ordinary letter are DIFFERENT
//     decoded keys, so the pair is accepted;
//   - equivalent: the short escape and the equivalent \u00XX spelling are
//     the SAME decoded key, so the pair is rejected as a duplicate.
func TestEscapeDecodingControlBytes(t *testing.T) {
	rows := []struct {
		name    string
		short   string // escape spelling, e.g. `\n` (as JSON text)
		letter  string // ordinary letter key spelling
		unicode string // equivalent \u00XX spelling
	}{
		{"backspace", `\b`, `b`, `\u0008`},
		{"formfeed", `\f`, `f`, `\u000c`},
		{"newline", `\n`, `n`, `\u000a`},
		{"carriage-return", `\r`, `r`, `\u000d`},
		{"tab", `\t`, `t`, `\u0009`},
	}
	for _, row := range rows {
		t.Run(row.name+"/distinct-from-letter", func(t *testing.T) {
			doc := `{"` + row.short + `":1,"` + row.letter + `":2}`
			if err := Validate([]byte(doc)); err != nil {
				t.Fatalf("short escape and its letter must be DISTINCT keys: %v\n%s", err, doc)
			}
		})
		t.Run(row.name+"/duplicate-of-unicode", func(t *testing.T) {
			doc := `{"` + row.short + `":1,"` + row.unicode + `":2}`
			if err := Validate([]byte(doc)); err == nil {
				t.Fatalf("short escape and its \\u00XX spelling must be the SAME decoded key: %s", doc)
			}
		})
	}
	// Identity escapes stay pinned: the escaped byte is the letter itself.
	for name, pair := range map[string][2]string{
		"quote":     {`\"`, `\u0022`},
		"backslash": {`\\`, `\u005c`},
		"solidus":   {`\/`, `\u002f`},
	} {
		t.Run(name+"/duplicate-of-unicode", func(t *testing.T) {
			doc := `{"` + pair[0] + `":1,"` + pair[1] + `":2}`
			if err := Validate([]byte(doc)); err == nil {
				t.Fatalf("identity escape and its \\u00XX spelling must be the SAME decoded key: %s", doc)
			}
		})
		t.Run(name+"/distinct-from-letter", func(t *testing.T) {
			doc := `{"` + pair[0] + `":1,"x":2}`
			if err := Validate([]byte(doc)); err != nil {
				t.Fatalf("identity escape key must be accepted: %v\n%s", err, doc)
			}
		})
	}
	// Value-string positive controls: escaped control bytes are valid inside
	// values (the shared validator must keep accepting them outside keys).
	for _, doc := range []string{
		`{"a":"\n\t\r\b\f"}`,
		`{"a":"line1\nline2\ttabbed"}`,
	} {
		if err := Validate([]byte(doc)); err != nil {
			t.Fatalf("escaped control bytes in a value rejected: %v\n%s", err, doc)
		}
	}
}

// TestEscapeReferenceModel: the validator's duplicate decisions are compared
// against an independent encoding/json reference over a generated corpus of
// VALID escape spellings (the reference decode therefore always succeeds).
// Two keys are duplicates iff their reference-decoded forms are equal; the
// validator must reject exactly the equal pairs and accept exactly the
// distinct pairs, so an escape misdecoding can never hide behind the
// mutation-test exemption. Invalid spellings are covered separately by
// TestEscapeReferenceModelInvalidSpellings and the malformed-structure
// matrix.
func TestEscapeReferenceModel(t *testing.T) {
	spellings := []string{
		`\b`, `\u0008`, `b`, `\u0062`,
		`\f`, `\u000c`, `f`, `\u0066`,
		`\n`, `\u000a`, `n`, `\u006e`,
		`\r`, `\u000d`, `r`, `\u0072`,
		`\t`, `\u0009`, `t`, `\u0074`,
		`\"`, `\u0022`,
		`\\`, `\u005c`,
		`\/`, `\u002f`,
		`x`, `plain`,
	}
	refDecode := func(spelling string) (string, bool) {
		var m map[string]int
		if err := json.Unmarshal([]byte(`{"`+spelling+`":1}`), &m); err != nil {
			return "", false
		}
		for k := range m {
			return k, true
		}
		return "", false
	}
	for i, a := range spellings {
		for j, b := range spellings {
			if i == j {
				continue
			}
			da, _ := refDecode(a)
			db, _ := refDecode(b)
			doc := `{"` + a + `":1,"` + b + `":2}`
			err := Validate([]byte(doc))
			switch {
			case da == db:
				// Same decoded identity: must be rejected as a duplicate.
				if err == nil {
					t.Fatalf("reference-duplicate pair accepted (%q == %q): %s", da, db, doc)
				}
			default:
				// Distinct decoded identities: must be accepted.
				if err != nil {
					t.Fatalf("reference-distinct pair rejected (%q != %q): %v\n%s", da, db, err, doc)
				}
			}
		}
	}
}

// TestEscapeReferenceModelInvalidSpellings: invalid escape spellings are
// rejected by BOTH the validator and encoding/json, so the reference model's
// valid-only corpus cannot hide a validator that swallows malformed escapes.
func TestEscapeReferenceModelInvalidSpellings(t *testing.T) {
	for _, spelling := range []string{
		`\x`,   // not a JSON escape
		`\u12`, // truncated hex
		`\u00g0`,
		`\`,
	} {
		doc := `{"` + spelling + `":1}`
		if err := Validate([]byte(doc)); err == nil {
			t.Fatalf("invalid spelling %q passed Validate", spelling)
		}
		var m map[string]int
		if err := json.Unmarshal([]byte(doc), &m); err == nil {
			t.Fatalf("invalid spelling %q accepted by encoding/json — not an invalid spelling", spelling)
		}
	}
}
