package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// validateObjectShape must reject exactly what validateObject rejects. Only
// the defensive copy was dropped, not the check — the mutation helpers still
// refuse to return bytes that are not a well-formed JSON object.
func TestValidateObjectShapeAgreesWithValidateObject(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"a":1}`,
		`{"a":{"b":[1,2,3]},"c":"x"}`,
		`  {"a":1}  `,

		// Rejected.
		``,
		`[1,2]`,
		`"a string"`,
		`null`,
		`42`,
		`{`,
		`{"a":}`,
		`{"a":1,}`,
		`{"a":1}{"b":2}`,
		`{"a":1,"a":2}`, // duplicate key: the shared validator's rule
		"{\"a\":\"\xff\xfe\"}",
	} {
		t.Run(raw, func(t *testing.T) {
			_, copyErr := validateObject([]byte(raw), false)
			shapeErr := validateObjectShape([]byte(raw))
			if (copyErr == nil) != (shapeErr == nil) {
				t.Fatalf("disagreement on %q: validateObject err=%v, validateObjectShape err=%v",
					raw, copyErr, shapeErr)
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
