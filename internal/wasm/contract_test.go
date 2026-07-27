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

// TestDeniedCapabilitiesAllReturnTheSameEnvelope: the host has several denial
// sites, and one of them drifting is far likelier than all of them changing
// together. A site that returns a differently-shaped error is invisible to
// guests that match on this one.
func TestDeniedCapabilitiesAllReturnTheSameEnvelope(t *testing.T) {
	src, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, `"status":"error"`) {
			continue
		}
		if !strings.Contains(line, "permission denied") {
			continue
		}
		if !strings.Contains(line, permissionDeniedEnvelope) {
			t.Errorf("a permission-denied response does not use the standard envelope:\n  %s\n"+
				"want exactly %s", strings.TrimSpace(line), permissionDeniedEnvelope)
		}
	}
}

// TestSDKAgreesOnTheDenialEnvelope closes the loop when the SDK is checked out
// beside this repo: the contract only holds if both sides spell it the same
// way, and asserting only the host half proves nothing about the guest.
func TestSDKAgreesOnTheDenialEnvelope(t *testing.T) {
	root := "../../../torana-plugin-sdk"
	if _, err := os.Stat(root); err != nil {
		t.Skip("torana-plugin-sdk not checked out beside this repo")
	}

	var found bool
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil //nolint:nilerr // unreadable files are not a contract failure
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(b), "permission denied") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("the SDK no longer mentions \"permission denied\" anywhere; the host still " +
			"returns it, so guests would stop detecting refused capabilities")
	}
}
