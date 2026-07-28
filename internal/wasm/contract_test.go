package wasm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The host tells a guest that a capability was refused by returning this exact
// JSON. Nothing checks it: the host writes a string literal, and every SDK
// parses one. If either side edits its literal the other simply stops
// recognising the refusal — and a plugin that cannot tell "denied" from a
// normal empty result carries on as though the call had succeeded.
//
// That is the failure mode #210 already produced once, in the SDK's
// PluginConfig(), which returned the denial envelope to the plugin as if it
// were configuration.
const permissionDeniedEnvelope = `{"status":"error","message":"permission denied"}`

// TestPermissionDeniedEnvelopeIsStable pins the wire literal itself. Changing
// it is a breaking ABI change for every already-published plugin binary, which
// cannot be recompiled by this repository.
func TestPermissionDeniedEnvelopeIsStable(t *testing.T) {
	src, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), permissionDeniedEnvelope) {
		t.Errorf("the host no longer returns %s on a denied capability.\n"+
			"Guests match this envelope verbatim to detect refusal; a plugin that stops "+
			"recognising it treats a denied host call as an ordinary empty result and "+
			"continues as though it had succeeded.\n"+
			"Already-published plugin binaries cannot be recompiled from here, so this "+
			"literal is a wire contract, not an implementation detail.",
			permissionDeniedEnvelope)
	}
}

// TestDeniedCapabilitiesAllReturnTheSameEnvelope is now structural rather than
// textual: runtime.go has ONE named constant and every denial site returns it,
// so "one site drifted from the others" cannot happen.
//
// The version this replaces scanned for lines containing both `"status":"error"`
// and "permission denied" and then checked those lines matched the envelope —
// so it could only fail on whitespace inside an already-correct one-liner, and
// a differently-worded denial was filtered out before the assertion ran.
func TestDeniedCapabilitiesAllReturnTheSameEnvelope(t *testing.T) {
	if permissionDeniedJSON != permissionDeniedEnvelope {
		t.Fatalf("the host constant changed to %s; this is a wire contract that "+
			"already-published plugin binaries match verbatim", permissionDeniedJSON)
	}

	src, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one occurrence of the literal — the constant's own declaration.
	// A second means someone reintroduced an inline copy, which is how the two
	// halves drift apart.
	if n := strings.Count(string(src), permissionDeniedEnvelope); n != 1 {
		t.Errorf("the denial envelope literal appears %d times in runtime.go, want 1 "+
			"(the permissionDeniedJSON declaration). An inline copy can drift from the "+
			"constant, and guests match it verbatim.", n)
	}
}

// TestSDKAgreesOnTheDenialEnvelope closes the loop when the SDK is checked out
// beside this repo. Asserting only the host half proves nothing about the guest.
//
// It looks for the EXACT envelope, not the phrase "permission denied" — the
// previous version was satisfied by a comment mentioning it.
func TestSDKAgreesOnTheDenialEnvelope(t *testing.T) {
	root := "../../../torana-plugin-sdk"
	if _, err := os.Stat(root); err != nil {
		t.Skip("torana-plugin-sdk not checked out beside this repo")
	}

	var matches []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable entries are not a contract failure
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".rs" {
			return nil
		}
		if strings.Contains(path, "/target/") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(b), permissionDeniedEnvelope) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Errorf("no SDK source contains the exact envelope %s.\n"+
			"The host returns it on every denied capability; an SDK that does not match it "+
			"byte for byte treats a refusal as an ordinary result and continues as though "+
			"the call had succeeded — which is what #210 produced in PluginConfig.",
			permissionDeniedEnvelope)
	}
	t.Logf("SDK sources matching the envelope: %v", matches)
}
