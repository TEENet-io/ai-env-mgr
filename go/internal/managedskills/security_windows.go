//go:build windows

package managedskills

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Protect grants SYSTEM and Administrators full access and ordinary users read
// and execute, with inheritance to new children. No localized account names,
// PowerShell, recursive icacls grants, or installer changes are required.
func Protect(root string) error {
	return protectDirectory(root, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;BU)")
}

func ProtectPrivate(root string) error {
	return protectDirectory(root, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
}

func trustedSID(sid *windows.SID) bool {
	if sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return true
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && windows.EqualSid(sid, user.User.Sid)
}

func protectDirectory(root, descriptor string) error {
	// A pre-created employee-owned directory must not become a trusted source
	// of SYSTEM-managed skills or an ownership journal.
	if err := safePath(root); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(root), root} {
		old, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		owner, _, err := old.Owner()
		if err != nil {
			return err
		}
		if !trustedSID(owner) {
			return fmt.Errorf("untrusted owner of company skill directory: %s", dir)
		}
	}
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, admins, nil, dacl, nil); err != nil {
		return err
	}
	// Inherited ACEs are replaced by Windows. Refuse pre-existing children
	// with a separate writable ACL instead of trusting top-level protection.
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := safePath(path); err != nil {
			return err
		}
		security, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		owner, _, err := security.Owner()
		if err != nil {
			return err
		}
		if !trustedSID(owner) {
			return fmt.Errorf("untrusted skill file owner: %s", path)
		}
		acl, _, err := security.DACL()
		if err != nil {
			return err
		}
		if acl == nil {
			return fmt.Errorf("unrestricted skill ACL: %s", path)
		}
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				return err
			}
			if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
				continue
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
				return fmt.Errorf("unsupported skill ACL: %s", path)
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			const writeRights = 0x156 | 0x10000 | 0x40000 | 0x80000 | 0x40000000 | 0x10000000
			if uint32(ace.Mask)&writeRights != 0 && !trustedSID(sid) {
				return fmt.Errorf("company skill path grants non-admin write access: %s", path)
			}
		}
		return nil
	})
}

func rejectReparse(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("managed skill path contains a reparse point: %s", path)
	}
	return nil
}
