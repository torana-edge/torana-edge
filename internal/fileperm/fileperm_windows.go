//go:build windows

package fileperm

import (
	"fmt"
	"io/fs"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows the owner-only invariant lives in the discretionary access
// control list, so that is what these two functions write and read. The mode
// bits an fs.FileInfo carries here are fiction — 0666 for any ordinary
// readable file — which is why info is ignored rather than consulted.

// restrict installs a PROTECTED DACL whose single entry grants the current
// user full control. Protected is the load-bearing half: without it the entry
// this call adds sits alongside whatever the parent directory inherits down,
// so a file in a world-readable directory stays world-readable.
func restrict(path string, dir bool) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
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
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
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

// verify reads the file's DACL back and requires that it is protected and that
// every ACE granting access names the current user. A deny ACE only ever takes
// access away, so those are not this check's business.
func verify(path string, _ fs.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security descriptor for %s: %w", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read security descriptor control flags for %s: %w", path, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%s inherits access from its parent directory; it must carry "+
			"a protected owner-only ACL", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read DACL for %s: %w", path, err)
	}
	if dacl == nil {
		// A NULL DACL is not "no access" — it grants everyone full control.
		return fmt.Errorf("%s has a NULL DACL, which grants everyone full control", path)
	}
	self, err := currentUserSID()
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("read ACE %d of %s: %w", i, path, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(self) {
			return fmt.Errorf("%s grants access to %s; it must be accessible only by its owner",
				path, sid.String())
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
