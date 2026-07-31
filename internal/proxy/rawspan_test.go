package proxy

import (
	"strings"
	"testing"
)

// rawJSONSpan returns the VERBATIM bytes of a value — the whole point is that
// no decode/re-encode round-trip touches them. These tests pin the scanner
// against escaped keys and strings, nested containers, big integers (which a
// float64 decode would destroy), and missing paths.

func TestRawJSONSpanNestedValues(t *testing.T) {
	doc := []byte(`{"a":{"b":[1,{"c":"hi"}],"d":true},"e":[null,{"f":"g"}]}`)
	cases := []struct {
		name string
		path []any
		want string
	}{
		{"object member", []any{"a", "b"}, `[1,{"c":"hi"}]`},
		{"array element object", []any{"a", "b", 1}, `{"c":"hi"}`},
		{"string", []any{"a", "b", 1, "c"}, `"hi"`},
		{"boolean", []any{"a", "d"}, `true`},
		{"top-level array element", []any{"e", 0}, `null`},
		{"deep string", []any{"e", 1, "f"}, `"g"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rawJSONSpan(doc, tc.path...)
			if !ok {
				t.Fatalf("path %v not found", tc.path)
			}
			if string(got) != tc.want {
				t.Errorf("path %v = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestRawJSONSpanEscapes(t *testing.T) {
	// Key and string both contain escaped quotes and backslashes; the value
	// must come back with the provider's exact escaping, not a re-encoding.
	doc := []byte(`{"a\"b":"x\\\"y","k":{"s":"a\"b\\c"}}`)
	got, ok := rawJSONSpan(doc, `a"b`)
	if !ok {
		t.Fatal("escaped key not found")
	}
	if string(got) != `"x\\\"y"` {
		t.Errorf("escaped string = %q, want the provider's verbatim bytes", got)
	}
	got, ok = rawJSONSpan(doc, "k", "s")
	if !ok {
		t.Fatal("nested escaped string not found")
	}
	if string(got) != `"a\"b\\c"` {
		t.Errorf("nested escaped string = %q, want verbatim bytes", got)
	}
}

func TestRawJSONSpanBigIntsUntouched(t *testing.T) {
	// 9007199254740993 is not representable in float64; only raw bytes keep it.
	doc := []byte(`{"args":{"zzz":1,"aaa":9007199254740993}}`)
	got, ok := rawJSONSpan(doc, "args")
	if !ok {
		t.Fatal("args not found")
	}
	if string(got) != `{"zzz":1,"aaa":9007199254740993}` {
		t.Errorf("big int was not preserved verbatim: %s", got)
	}
}

func TestRawJSONSpanMissingPaths(t *testing.T) {
	doc := []byte(`{"a":{"b":1},"c":[10,20]}`)
	for _, tc := range []struct {
		name string
		path []any
	}{
		{"unknown key", []any{"zzz"}},
		{"unknown nested key", []any{"a", "zzz"}},
		{"index past end", []any{"c", 5}},
		{"key on array", []any{"c", "b"}},
		{"index on object", []any{"a", 0}},
		{"missing key then index", []any{"missing", 3}},
		{"non-object key on scalar", []any{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := rawJSONSpan(doc, tc.path...); ok {
				t.Errorf("path %v unexpectedly found: %q", tc.path, got)
			}
		})
	}
	if got, ok := rawJSONSpan(nil, "a"); ok || got != nil {
		t.Errorf("nil doc: got %q, %v; want nil, false", got, ok)
	}
	if got, ok := rawJSONSpan([]byte(`not json`), "a"); ok || got != nil {
		t.Errorf("non-JSON doc: got %q, %v; want nil, false", got, ok)
	}
}

func TestRawJSONSpanStringSlotsKeepQuotes(t *testing.T) {
	// openai/responses carry arguments as a JSON STRING; the span must be the
	// full quoted bytes so the canonical inner text can be decoded from it.
	doc := []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"{\"x\":1}"}}]}}]}`)
	got, ok := rawJSONSpan(doc, "choices", 0, "message", "tool_calls", 0, "function", "arguments")
	if !ok {
		t.Fatal("arguments not found")
	}
	if string(got) != `"{\"x\":1}"` {
		t.Errorf("string slot span = %q, want the full quoted bytes", got)
	}
}

func TestRawJSONSpanOffsetSplice(t *testing.T) {
	// rawJSONSpanAt gives spliceable offsets into the document.
	doc := []byte(`{"keep":{"a":1},"args":{"zzz":1,"aaa":2},"tail":true}`)
	start, end, ok := rawJSONSpanAt(doc, "args")
	if !ok {
		t.Fatal("args not found")
	}
	repl := []byte(`{"new":1}`)
	out := spliceBytes(doc, start, end, repl)
	if !strings.Contains(string(out), `{"new":1}`) {
		t.Errorf("replacement missing: %s", out)
	}
	if !strings.Contains(string(out), `"keep":{"a":1}`) || !strings.Contains(string(out), `"tail":true`) {
		t.Errorf("splice damaged sibling fields: %s", out)
	}
}
