//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package agent

import "os"

// On other platforms there is no hidden prompt: the bootstrap refuses, as
// it does off a terminal.
func isTerminal(*os.File) bool { return false }

func echoOff(*os.File) (func(), error) { return nil, ErrNotTerminal }
