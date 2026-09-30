package agent

import (
	"errors"
	"io"
	"os"
	"os/signal"
)

// The hidden prompt of the client bootstrap (docs/ONBOARDING-CONTRACT.md,
// "The client's view"): an invitation code is read only from the terminal,
// with echo off. It is never taken from an argument, the environment, a file
// or a pipe. The platform files implement terminal detection and echo
// control with golang.org/x/sys only.

// maxPromptLine bounds one line read at the prompt.
const maxPromptLine = 256

// ErrNotTerminal refuses a hidden read from something that is not a
// terminal.
var ErrNotTerminal = errors.New("not a terminal")

// IsTerminal reports whether f is an interactive terminal (a console on
// Windows).
func IsTerminal(f *os.File) bool { return isTerminal(f) }

// ErrInterrupted is an interrupt at the hidden prompt.
var ErrInterrupted = errors.New("interrupted")

// ReadHidden writes prompt to out and reads one line from in, a terminal,
// with echo turned off, restoring the terminal's mode afterwards, also when
// the prompt is interrupted. The line ending is not part of the result.
func ReadHidden(in *os.File, out io.Writer, prompt string) (string, error) {
	if !isTerminal(in) {
		return "", ErrNotTerminal
	}
	restore, err := echoOff(in)
	if err != nil {
		return "", err
	}
	defer restore()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	if _, err := io.WriteString(out, prompt); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readLine(in)
		done <- result{line, err}
	}()
	defer io.WriteString(out, "\n") // the user's Enter was not echoed
	select {
	case r := <-done:
		return r.line, r.err
	case <-sigs:
		return "", ErrInterrupted // the read stays blocked; the caller exits
	}
}

// readLine reads up to a line ending, one byte at a time so nothing past the
// line is consumed. A line longer than maxPromptLine is refused.
func readLine(r io.Reader) (string, error) {
	var buf []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			switch b[0] {
			case '\n':
				return string(buf), nil
			case '\r':
				continue
			}
			if len(buf) >= maxPromptLine {
				return "", errors.New("the line is too long")
			}
			buf = append(buf, b[0])
		}
		if err == io.EOF {
			if len(buf) == 0 {
				return "", io.ErrUnexpectedEOF
			}
			return string(buf), nil
		}
		if err != nil {
			return "", err
		}
	}
}
