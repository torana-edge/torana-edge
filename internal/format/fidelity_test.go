package format_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/format"

	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// Round-trip fidelity: what a caller sends must reach the provider.
//
// Torana is a transparent proxy. Every adapter decodes a provider request into
// the IR and re-encodes it, and anything the IR cannot represent is silently
// gone by the time the provider sees it — the caller gets no error, and the
// model gets a request the harness never wrote. That failure mode is invisible
// to every other test here, which check the shapes an adapter DOES model.
//
// This suite checks the opposite direction: it feeds real provider wire shapes
// through Unmarshal → Marshal and asserts the facts that must survive. A case
// here is a promise to the caller, so a new adapter field or a normalization
// that drops one fails loudly rather than reaching a provider as an empty
// string.
//
// Shape normalization is allowed and expected (a one-element content array may
// re-emit as a scalar string); losing the CONTENT is not. Cases assert content
// with survives, and pin exact structure only where the shape itself carries
// meaning — `strict` under `function`, say, where the wrong nesting is ignored
// by the provider.

type fidelityCase struct {
	name   string
	format string
	body   string

	// survives lists substrings that must appear in the re-encoded body.
	// Use for payloads whose exact position may legitimately be normalized.
	survives []string

	// paths pins exact values at exact locations, e.g.
	// "tools[0].function.strict": true. Use where the shape is the point.
	paths map[string]any

	// absentPaths must not resolve. Use where emitting a member is itself the
	// bug (a member the provider rejects, or one this adapter's own parser
	// would refuse on the way back in).
	absentPaths []string

	// wantUnmarshalErr marks a body the adapter must refuse. Refusing is a
	// valid outcome; silently dropping is not.
	wantUnmarshalErr bool
	// wantMarshalErr marks an accepted body the adapter cannot re-encode.
	wantMarshalErr bool
}

func TestRoundTripFidelity(t *testing.T) {
	for _, tc := range fidelityCases {
		t.Run(tc.format+"/"+tc.name, func(t *testing.T) {
			f := format.Lookup(tc.format)
			if f == nil {
				t.Fatalf("format %q is not registered", tc.format)
			}

			chat, err := f.Request.Unmarshal([]byte(tc.body))
			if tc.wantUnmarshalErr {
				if err == nil {
					t.Fatalf("expected the adapter to refuse this body, but it was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v\n  body: %s", err, tc.body)
			}

			out, err := f.Request.Marshal(chat)
			if tc.wantMarshalErr {
				if err == nil {
					t.Fatalf("expected Marshal to refuse, got: %s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("Marshal: %v\n  body: %s", err, tc.body)
			}

			for _, want := range tc.survives {
				if !strings.Contains(string(out), want) {
					t.Errorf("LOST %q on the way to the provider\n  in:  %s\n  out: %s", want, tc.body, out)
				}
			}

			var decoded any
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("re-encoded body is not valid JSON: %v\n  out: %s", err, out)
			}
			for path, want := range tc.paths {
				got, ok := jsonPath(decoded, path)
				if !ok {
					t.Errorf("path %s is missing\n  in:  %s\n  out: %s", path, tc.body, out)
					continue
				}
				if !sameJSON(got, want) {
					t.Errorf("path %s = %#v, want %#v\n  out: %s", path, got, want, out)
				}
			}
			for _, path := range tc.absentPaths {
				if got, ok := jsonPath(decoded, path); ok {
					t.Errorf("path %s should be absent, got %#v\n  out: %s", path, got, out)
				}
			}
		})
	}
}

// jsonPath resolves a dotted path with bracket indexes, e.g.
// "messages[1].content[0].text".
func jsonPath(doc any, path string) (any, bool) {
	cur := doc
	for _, seg := range splitPath(path) {
		if idx, err := strconv.Atoi(seg); err == nil {
			arr, ok := cur.([]any)
			if !ok || idx < 0 || idx >= len(arr) {
				return nil, false
			}
			cur = arr[idx]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func splitPath(path string) []string {
	var out []string
	for _, part := range strings.Split(path, ".") {
		for {
			open := strings.IndexByte(part, '[')
			if open < 0 {
				break
			}
			shut := strings.IndexByte(part, ']')
			if shut < open {
				break
			}
			if open > 0 {
				out = append(out, part[:open])
			}
			out = append(out, part[open+1:shut])
			part = part[shut+1:]
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// sameJSON compares against literals written in the table, so a test can say
// `true` or `1` without spelling out encoding/json's float64.
func sameJSON(got, want any) bool {
	return fmt.Sprint(got) == fmt.Sprint(want)
}
