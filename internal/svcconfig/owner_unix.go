//go:build !windows

package svcconfig

import (
	"os"
	"syscall"
)

// sameOwner gives f the owner and group of prev, when they differ: a config
// rewritten by root stays readable by the account that owned it.
func sameOwner(f *os.File, prev os.FileInfo) error {
	want, ok := prev.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if got, ok := info.Sys().(*syscall.Stat_t); ok && got.Uid == want.Uid && got.Gid == want.Gid {
		return nil
	}
	return f.Chown(int(want.Uid), int(want.Gid))
}
