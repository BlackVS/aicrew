//go:build windows

package privatefiletest

import "golang.org/x/sys/windows"

// Expose replaces path's DACL with a protected one that also grants Everyone
// read access.
func Expose(path string) error { return grantEveryone(path, "FR") }

// ExposeTraverse replaces the directory path's DACL with a protected one
// that also lets Everyone traverse it, reaching what it contains by name,
// but not list it.
func ExposeTraverse(path string) error { return grantEveryone(path, "0x20") }

func grantEveryone(path, rights string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;" + rights + ";;;WD)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
