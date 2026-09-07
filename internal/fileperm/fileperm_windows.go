//go:build windows

package fileperm

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows the owner-only invariant lives in the discretionary access
// control list, so that is what this file writes and reads. The mode bits an
// fs.FileInfo carries here are fiction — 0666 for any ordinary readable file —
// which is why info is ignored rather than consulted.

// restrict installs a PROTECTED DACL whose single entry grants the current
// user full control. Protected is the load-bearing half: without it the entry
// this call adds sits alongside whatever the parent directory inherits down,
// so a file in a world-readable directory stays world-readable.
//
// The apply is by NAME because SetSecurityInfo needs a handle opened for
// WRITE_DAC, which os.OpenFile never requests. That is sound only because
// nothing acts on the result: every caller re-checks the OPEN HANDLE
// afterwards (verifyFile), so a name that was swapped between the apply and
// the check is caught rather than trusted.
func restrict(path string, dir bool) error {
	acl, err := ownerOnlyACL(dir)
	if err != nil {
		return fmt.Errorf("build owner-only ACL for %s: %w", path, err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
	if err != nil {
		return fmt.Errorf("apply owner-only ACL to %s: %w", path, err)
	}
	return nil
}

func ownerOnlyACL(dir bool) (*windows.ACL, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	// TrusteeValue holds a raw pointer the GC does not see; pin it for the
	// lifetime of the call that consumes it.
	var pinner runtime.Pinner
	pinner.Pin(sid)
	defer pinner.Unpin()

	// A directory's entry is made inheritable so that everything created
	// inside it starts owner-only too, rather than depending on each writer
	// remembering to ask.
	inheritance := uint32(windows.NO_INHERITANCE)
	if dir {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
}

// verify resolves path by NAME. It is a pre-check only: a name can be made to
// resolve elsewhere between this call and any later open, so nothing that
// reads or writes the file may rely on it alone — see verifyFile.
func verify(path string, _ os.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security descriptor for %s: %w", path, err)
	}
	return verifyDescriptor(sd, path)
}

// verifyFile asks the same question of the object behind an OPEN HANDLE, so
// the answer describes the file that will actually be read or written.
//
// GetNamedSecurityInfo is a NAME lookup and Microsoft documents it as not
// handling races: checking the path and then writing through a handle opened
// separately lets a replacement be verified while the original is used. The
// handle is the object; the path is only a hint about it.
func verifyFile(f *os.File) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security descriptor for %s: %w", f.Name(), err)
	}
	return verifyDescriptor(sd, f.Name())
}

func verifyDescriptor(sd *windows.SECURITY_DESCRIPTOR, name string) error {
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read security descriptor control flags for %s: %w", name, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%s inherits access from its parent directory; it must carry "+
			"a protected owner-only ACL", name)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read DACL for %s: %w", name, err)
	}
	if dacl == nil {
		// A NULL DACL is not "no access" — it grants everyone full control.
		return fmt.Errorf("%s has a NULL DACL, which grants everyone full control", name)
	}
	self, err := currentUserSID()
	if err != nil {
		return err
	}
	return verifyDACL(dacl, self, name)
}

// verifyDACL requires every entry to be one this code understands, and every
// entry that GRANTS anything to name the current user.
//
// The refusal on an unrecognised type is the point. Windows has several other
// access-ALLOWING forms — ACCESS_ALLOWED_OBJECT_ACE_TYPE,
// ACCESS_ALLOWED_CALLBACK_ACE_TYPE, ACCESS_ALLOWED_CALLBACK_OBJECT_ACE_TYPE,
// ACCESS_ALLOWED_COMPOUND_ACE_TYPE — and their SID does not sit at
// ACCESS_ALLOWED_ACE.SidStart, so they cannot be read through this struct. A
// loop that inspected only ACCESS_ALLOWED_ACE_TYPE and skipped the rest let a
// DACL grant another principal full control and still pass. At a
// confidentiality boundary the undecoded case must be a refusal, not a shrug.
func verifyDACL(dacl *windows.ACL, self *windows.SID, name string) error {
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("read ACE %d of %s: %w", i, name, err)
		}
		// ACE_HEADER is at the same offset in every ACE form, so the type is
		// always readable even when the rest of the entry is not.
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			// A deny entry only ever removes access; it cannot widen it.
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.Equals(self) {
				return fmt.Errorf("%s grants access to %s; it must be accessible only "+
					"by its owner", name, sid.String())
			}
		default:
			return fmt.Errorf("%s carries an ACE of type %d that this check cannot "+
				"decode; refusing rather than assuming it grants nothing",
				name, ace.Header.AceType)
		}
	}
	return nil
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("determine the current user: %w", err)
	}
	return user.User.Sid, nil
}
