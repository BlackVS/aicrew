package agent

import (
	"os"

	"golang.org/x/sys/windows"
)

func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// echoOff turns off the console's echo, keeping line input and Ctrl+C, and
// returns the function that restores the mode.
func echoOff(f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return nil, err
	}
	quiet := old&^windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	if err := windows.SetConsoleMode(h, quiet); err != nil {
		return nil, err
	}
	return func() { _ = windows.SetConsoleMode(h, old) }, nil
}
