//go:build windows

package privatefile

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readRights are the rights that let an account read the file, or grant
// itself access to read it. For a directory, listing (the same bit as
// reading data) and traversing it (which reaches what it contains) count too.
const readRights = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL |
	windows.WRITE_DAC | windows.WRITE_OWNER

const dirRights = readRights | windows.FILE_TRAVERSE | windows.GENERIC_EXECUTE

// check measures the file's effective access, not mode bits. The owner must
// be the current user, SYSTEM or Administrators, and every allow entry that
// grants read (or the right to change the DACL or owner) must name one of
// them. A null DACL, which grants everyone, is refused, and so is any entry
// type this check does not understand.
func check(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	rights := uint32(readRights)
	switch {
	case dir && !info.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	case dir:
		rights = dirRights
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("cannot read the access control list of %s: %w", path, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	trusted := []*windows.SID{user.User.Sid}
	for _, k := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(k)
		if err != nil {
			return err
		}
		trusted = append(trusted, sid)
	}
	isTrusted := func(sid *windows.SID) bool {
		for _, t := range trusted {
			if sid.Equals(t) {
				return true
			}
		}
		return false
	}
	fix := fmt.Sprintf(`restrict it to your account, for example: icacls "%s" /inheritance:r /grant:r "%%USERNAME%%:F"`, path)

	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !isTrusted(owner) {
		return fmt.Errorf("%s is owned by another account; %s", path, fix)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("%s has no access control list, so every account can read it; %s", path, fix)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies to children only, never to this file
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("%s has an access entry of a type this check does not understand (%d); %s", path, ace.Header.AceType, fix)
		}
		if uint32(ace.Mask)&rights == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !isTrusted(sid) {
			return fmt.Errorf("%s is readable by %s; %s", path, sid.String(), fix)
		}
	}
	return nil
}
