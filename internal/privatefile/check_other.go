//go:build !windows

package privatefile

import (
	"fmt"
	"os"
	"syscall"
)

// check refuses a file another local account could read: it must be a
// regular file owned by the current user with no group or other permission.
func check(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other accounts (mode %04o); restrict it with: chmod 600 %s", path, perm, path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by the current user", path)
	}
	return nil
}
