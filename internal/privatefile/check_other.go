//go:build !windows

package privatefile

import (
	"fmt"
	"os"
	"syscall"
)

// check refuses a file (or, with dir, a directory) another local account
// could read: it must be owned by the current user with no group or other
// permission.
func check(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	fix := "chmod 600"
	switch {
	case dir && !info.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	case dir:
		fix = "chmod 700"
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other accounts (mode %04o); restrict it with: %s %s", path, perm, fix, path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by the current user", path)
	}
	return nil
}
