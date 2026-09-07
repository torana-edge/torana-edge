//go:build windows

package fileperm

import (
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The DACL walk is tested directly, against hand-built lists, because the
// cases that matter cannot be produced by asking Windows to protect a file:
// they are DACLs somebody ELSE wrote, which is exactly the situation
// verification exists for.

func selfSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	return sid
}

// aclGranting builds a one-entry DACL of the given ACE type granting sid full
// control. Windows will only construct the ordinary allow/deny forms through
// ACLFromEntries, so an alternate ALLOW type is produced by rewriting the
// header of a built entry — the header sits at the same offset in every ACE
// form, which is the whole reason a type this code cannot decode is still
// recognisable as one it must refuse.
func aclGranting(t *testing.T, sid *windows.SID, mode windows.ACCESS_MODE, aceType uint8) *windows.ACL {
	t.Helper()
	var pinner runtime.Pinner
	pinner.Pin(sid)
	defer pinner.Unpin()

	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        mode,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatalf("GetAce: %v", err)
	}
	ace.Header.AceType = aceType
	return acl
}

func TestVerifyDACLAcceptsOnlyTheOwner(t *testing.T) {
	self := selfSID(t)
	acl := aclGranting(t, self, windows.SET_ACCESS, windows.ACCESS_ALLOWED_ACE_TYPE)
	if err := verifyDACL(acl, self, "owner-only"); err != nil {
		t.Errorf("a DACL granting only the owner was refused: %v", err)
	}
}

func TestVerifyDACLRejectsAnotherPrincipal(t *testing.T) {
	self := selfSID(t)
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	acl := aclGranting(t, everyone, windows.SET_ACCESS, windows.ACCESS_ALLOWED_ACE_TYPE)
	err = verifyDACL(acl, self, "world-readable")
	if err == nil {
		t.Fatal("verifyDACL accepted a DACL granting Everyone full control")
	}
	if !strings.Contains(err.Error(), "S-1-1-0") {
		t.Errorf("error does not name the offending principal: %v", err)
	}
}

// The finding this pins: a walk that inspected only ACCESS_ALLOWED_ACE_TYPE
// and skipped everything else let these through. Their SID does not sit at
// ACCESS_ALLOWED_ACE.SidStart, so they cannot be read through that struct —
// the only safe answer is to refuse.
func TestVerifyDACLRejectsUndecodableAllowACETypes(t *testing.T) {
	const (
		accessAllowedCompoundACEType       = 4
		accessAllowedObjectACEType         = 5
		accessAllowedCallbackACEType       = 9
		accessAllowedCallbackObjectACEType = 11
	)
	self := selfSID(t)
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	for _, tc := range []struct {
		name    string
		aceType uint8
	}{
		{"compound", accessAllowedCompoundACEType},
		{"object", accessAllowedObjectACEType},
		{"callback", accessAllowedCallbackACEType},
		{"callback object", accessAllowedCallbackObjectACEType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acl := aclGranting(t, everyone, windows.SET_ACCESS, tc.aceType)
			err := verifyDACL(acl, self, "alternate-allow")
			if err == nil {
				t.Fatalf("verifyDACL accepted an access-allowing ACE of type %d "+
					"that it cannot decode; a DACL can grant another principal "+
					"full control through this form and still pass", tc.aceType)
			}
			if !strings.Contains(err.Error(), "cannot") {
				t.Errorf("error does not say the type was undecodable: %v", err)
			}
		})
	}
}

// A deny entry only ever removes access, so it is not this check's business —
// but it must not be mistaken for an unknown type and refused either.
func TestVerifyDACLAllowsDenyEntries(t *testing.T) {
	self := selfSID(t)
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	acl := aclGranting(t, everyone, windows.DENY_ACCESS, windows.ACCESS_DENIED_ACE_TYPE)
	if err := verifyDACL(acl, self, "deny-everyone"); err != nil {
		t.Errorf("a deny entry was treated as granting access: %v", err)
	}
}
