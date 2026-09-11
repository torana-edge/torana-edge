package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// What a valid JSON object IS, stated independently of any implementation.
//
// An earlier version of this test compared validateObjectShape against
// validateObject — which now DELEGATES to it, so they agreed by construction.
// Gutting validateObjectShape to accept every non-empty byte sequence still
// passed every row. A test that two functions agree proves nothing when one
// calls the other; the expectation has to be written down.
var objectValidationTable = []struct {
	name   string
	raw    string
	accept bool
}{
	{"empty object", `{}`, true},
	{"one member", `{"a":1}`, true},
	{"nested", `{"a":{"b":[1,2,3]},"c":"x"}`, true},
	{"surrounding whitespace", `  {"a":1}  `, true},
	{"large integer lexeme", `{"n":12345678901234567890}`, true},

	{"empty input", ``, false},
	{"array", `[1,2]`, false},
	{"string scalar", `"a string"`, false},
	{"null", `null`, false},
	{"number scalar", `42`, false},
	{"truncated", `{`, false},
	{"missing value", `{"a":}`, false},
	{"trailing comma", `{"a":1,}`, false},
	{"trailing value", `{"a":1}{"b":2}`, false},
	{"duplicate key", `{"a":1,"a":2}`, false},
	{"escape-equivalent duplicate key", `{"a":1,"\u0061":2}`, false},
	{"invalid utf-8", "{\"a\":\"\xff\xfe\"}", false},
	{"lone surrogate", `{"a":"\ud800"}`, false},
	{"whitespace only", `   `, false},
}

// validateObjectShape must implement that table.
func TestValidateObjectShapeMatchesTheContract(t *testing.T) {
	for _, tc := range objectValidationTable {
		t.Run(tc.name, func(t *testing.T) {
			err := validateObjectShape([]byte(tc.raw))
			if tc.accept && err != nil {
				t.Fatalf("rejected %q, which is a valid JSON object: %v", tc.raw, err)
			}
			if !tc.accept && err == nil {
				t.Fatalf("accepted %q, which is not a valid JSON object", tc.raw)
			}
		})
	}
}

// And so must validateObject, checked against the same written-down table
// rather than against validateObjectShape.
func TestValidateObjectMatchesTheContract(t *testing.T) {
	for _, tc := range objectValidationTable {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateObject([]byte(tc.raw), false)
			if tc.accept && err != nil {
				t.Fatalf("rejected %q, which is a valid JSON object: %v", tc.raw, err)
			}
			if !tc.accept && err == nil {
				t.Fatalf("accepted %q, which is not a valid JSON object", tc.raw)
			}
		})
	}
}

// validateObject still returns a copy the caller cannot use to reach back into
// the authority's bytes. Dropping the copy where it is not needed must not
// drop it where it is.
func TestValidateObjectStillReturnsADefensiveCopy(t *testing.T) {
	raw := []byte(`{"a":1}`)
	out, err := validateObject(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if &out[0] == &raw[0] {
		t.Fatal("validateObject returned the caller's own backing array; a later write " +
			"through it would mutate an authority nobody thinks is mutable")
	}
	out[2] = 'X'
	if string(raw) != `{"a":1}` {
		t.Fatalf("input was mutated through the returned slice: %s", raw)
	}
}

// The mutation helpers must still refuse to hand back a broken splice. This is
// the property the discarded copy was incidentally carrying.
func TestMutationHelpersStillRejectABrokenResult(t *testing.T) {
	// A value that is not valid JSON cannot be spliced in.
	if _, err := setMember([]byte(`{"a":1}`), "b", json.RawMessage(`{`)); err == nil {
		t.Error("setMember accepted a malformed value")
	}
	if _, err := setMember([]byte(`[1,2]`), "b", json.RawMessage(`1`)); err == nil {
		t.Error("setMember accepted a non-object target")
	}
	if _, err := deleteMember([]byte(`[1,2]`), "a"); err == nil {
		t.Error("deleteMember accepted a non-object target")
	}
}

// The result is still byte-exact, which is the whole point of splicing rather
// than re-encoding: large integers and key order survive.
func TestSetMemberPreservesLexemesAfterTheChange(t *testing.T) {
	raw := []byte(`{"big":12345678901234567890,"z":1,"a":2}`)
	out, err := setMember(raw, "added", json.RawMessage(`"v"`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `12345678901234567890`) {
		t.Errorf("a large integer lost its lexeme: %s", got)
	}
	if strings.Index(got, `"z"`) > strings.Index(got, `"a"`) {
		t.Errorf("member order was not preserved: %s", got)
	}
}
