//go:build !linux && !windows

package agent

import "os/exec"

// On macOS and the other platforms no mechanism stops the child when the
// launcher is killed outright; the handle's expiry is the guarantee (see
// RunClient).
func bindToLauncher(*exec.Cmd) {}

func afterStart(*exec.Cmd) (func(), error) { return func() {}, nil }
