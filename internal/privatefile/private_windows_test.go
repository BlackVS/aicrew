//go:build windows

package privatefile

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The DACL is protected and grants access to the current user, SYSTEM and
// Administrators only. SIDs are compared as SIDs: SDDL may print a
// well-known account as an alias (the built-in Administrator as "LA").
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
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatal(err)
	}
	allowed := []*windows.SID{user.User.Sid, system, admins}

	sddl := sd.String()
	dacl := sddl[strings.Index(sddl, "D:"):]
	// The flags before the first entry: P (protected), and AI on a
	// directory whose entries are inheritable.
	flags, entries, _ := strings.Cut(dacl[2:], "(")
	if !strings.Contains(flags, "P") {
		t.Fatalf("DACL %s is not protected", dacl)
	}
	aces := strings.Split(strings.TrimSuffix(entries, ")"), ")(")
	if len(aces) != len(allowed) {
		t.Fatalf("DACL %s has %d entries, want %d", dacl, len(aces), len(allowed))
	}
	for _, ace := range aces {
		fields := strings.Split(ace, ";")
		if len(fields) != 6 || fields[0] != "A" {
			t.Fatalf("DACL %s has entry %q", dacl, ace)
		}
		sid, err := windows.StringToSid(fields[5])
		if err != nil {
			t.Fatalf("DACL %s: entry %q: %v", dacl, ace, err)
		}
		ok := false
		for _, a := range allowed {
			ok = ok || sid.Equals(a)
		}
		if !ok {
			t.Fatalf("DACL %s grants %q", dacl, ace)
		}
	}
}

func assertPrivateDir(t *testing.T, path string) {
	t.Helper()
	assertPrivate(t, path)
}

// assertInherited checks that a file created in dir with no security
// attributes of its own gets the directory's owner-only protection.
func assertInherited(t *testing.T, dir string) {
	t.Helper()
	path := dir + `\inherited`
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(path)
	if err := Check(path); err != nil {
		t.Fatalf("a file created in the directory: %v", err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	if strings.Contains(sddl, ";;;WD)") || strings.Contains(sddl, ";;;BU)") || strings.Contains(sddl, ";;;AU)") {
		t.Fatalf("a file created in the directory is shared: %s", sddl)
	}
}
