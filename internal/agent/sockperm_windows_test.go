//go:build windows

package agent

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// assertSocketPrivate checks the socket's own DACL: every account holding
// the Bypass Traverse Checking privilege reaches a file by its path whatever
// its directory allows, so the socket must inherit the owner-only entries.
func assertSocketPrivate(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	_, entries, _ := strings.Cut(sddl[strings.Index(sddl, "D:"):], "(")
	aces := strings.Split(strings.TrimSuffix(entries, ")"), ")(")
	if len(aces) != 3 {
		t.Fatalf("socket DACL %s has %d entries, want the user, SYSTEM and Administrators", sddl, len(aces))
	}
	for _, ace := range aces {
		fields := strings.Split(ace, ";")
		if len(fields) != 6 || fields[0] != "A" {
			t.Fatalf("socket DACL %s has entry %q", sddl, ace)
		}
		sid, err := windows.StringToSid(fields[5])
		if err != nil {
			t.Fatalf("socket DACL %s: %v", sddl, err)
		}
		if !sid.Equals(user.User.Sid) && fields[5] != "SY" && fields[5] != "BA" {
			t.Fatalf("socket DACL %s grants %q", sddl, ace)
		}
	}
}
