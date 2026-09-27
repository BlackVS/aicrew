//go:build linux

package agent

import (
	"os/exec"
	"syscall"
)

// bindToLauncher asks the kernel to send the child SIGTERM when the
// launcher dies, however it dies.
func bindToLauncher(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

func afterStart(*exec.Cmd) (func(), error) { return func() {}, nil }
