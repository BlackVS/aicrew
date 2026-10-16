//go:build linux

package agent

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// limitMemory limits this process's data segment, which holds the Go
// heap's mappings, to what it holds now and limit more (RLIMIT_DATA, which
// Linux enforces for private writable mappings since 4.7). An allocation
// past it fails, and the Go runtime ends the process with "fatal error:
// out of memory". RLIMIT_AS would not do: the runtime reserves address
// space it never uses, and fails at once under a modest limit. The base is
// what the process holds at the start, so a race-detector build, whose
// shadow memory is mapped before main, keeps the same headroom.
func limitMemory(limit int64) error {
	held, err := vmData()
	if err != nil {
		return err
	}
	l := uint64(held + limit)
	return syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: l, Max: l})
}

// vmData is the process's data segment in bytes, from /proc/self/status.
func vmData() (int64, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmData:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			if err != nil {
				return 0, err
			}
			return kb << 10, nil
		}
	}
	return 0, errors.New("/proc/self/status names no VmData")
}
