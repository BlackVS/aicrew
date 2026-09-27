//go:build !windows

package privatefile

import "os"

func create(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	// The umask can only remove bits; set the mode exactly anyway.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return f, nil
}
