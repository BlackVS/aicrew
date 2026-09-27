//go:build windows

package privatefile

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The DACL is protected and grants access to the current user, SYSTEM and
// Administrators only.
func assertPrivate(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := sd.String()
	dacl := sddl[strings.Index(sddl, "D:"):]
	if !strings.HasPrefix(dacl, "D:P") {
		t.Fatalf("DACL %s is not protected", dacl)
	}
	want := map[string]bool{sid: true, "SY": true, "BA": true}
	aces := strings.Split(strings.TrimSuffix(strings.TrimPrefix(dacl[3:], "("), ")"), ")(")
	if len(aces) != len(want) {
		t.Fatalf("DACL %s has %d entries, want %d", dacl, len(aces), len(want))
	}
	for _, ace := range aces {
		fields := strings.Split(ace, ";")
		if len(fields) != 6 || fields[0] != "A" || !want[fields[5]] {
			t.Fatalf("DACL %s grants %q", dacl, ace)
		}
	}
}
