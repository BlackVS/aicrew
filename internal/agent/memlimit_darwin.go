//go:build darwin

package agent

import "golang.org/x/sys/unix"

// limitMemory sets RLIMIT_DATA to limit, and watches the Go runtime's
// memory: macOS does not enforce the data limit for the anonymous mappings
// the runtime allocates with, so the watchdog is the limit there.
func limitMemory(limit int64) error {
	var cur unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_DATA, &cur); err == nil && (cur.Max == unix.RLIM_INFINITY || uint64(limit) < cur.Max) {
		_ = unix.Setrlimit(unix.RLIMIT_DATA, &unix.Rlimit{Cur: uint64(limit), Max: uint64(limit)})
	}
	watchMemory(limit)
	return nil
}
