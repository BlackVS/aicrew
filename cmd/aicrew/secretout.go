package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

// A command that reveals a secret (a credential's bearer, an invitation's
// code, an operator token) writes it only where --output names (merged
// proposal section 6.4): a new owner-only file, or standard output for a
// pipe with "-". The value never reaches a terminal: "-" is refused when
// standard output is one, and the command's metadata then goes to stderr,
// so standard output carries the secret alone.

// outputFlags is --output and the flag name it replaces, which still works
// for one release, with a notice, and is removed in 0.4.0.
type outputFlags struct {
	output, old *string
	oldName     string
}

func addOutput(fs *flag.FlagSet, oldName, what string) *outputFlags {
	return &outputFlags{
		output:  fs.String("output", "", "new owner-only file that receives the "+what+", or - for a pipe"),
		old:     fs.String(oldName, "", "the name of --output before 0.3.0"),
		oldName: oldName,
	}
}

// value is the output the command was given, and false when both names are.
func (o *outputFlags) value(set map[string]bool, stderr io.Writer) (string, bool) {
	if set["output"] && set[o.oldName] {
		fmt.Fprintf(stderr, "aicrew: give --output or -%s, not both\n", o.oldName)
		return "", false
	}
	if set[o.oldName] {
		fmt.Fprintf(stderr, "aicrew: -%s is now --output; the old name is removed in 0.4.0\n", o.oldName)
		return *o.old, true
	}
	return *o.output, true
}

// isTerminal reports whether w is an interactive terminal, where no secret
// is ever written. Tests replace it.
var isTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// writeSecret writes a secret and its newline; tests replace it to fail.
var writeSecret = func(w io.Writer, secret string) error {
	if _, err := io.WriteString(w, secret+"\n"); err != nil {
		return err
	}
	if f, ok := w.(*os.File); ok {
		return f.Sync()
	}
	return nil
}

// secretOutput is where one command writes the one secret it reveals.
type secretOutput struct {
	path   string   // the --output value; "-" is standard output
	file   *os.File // the new file, nil for "-"
	stdout io.Writer
}

// openSecretOutput prepares the output before anything is issued, so a
// refused output issues nothing.
func openSecretOutput(path string, stdout io.Writer) (*secretOutput, error) {
	if path == "-" {
		if isTerminal(stdout) {
			return nil, errors.New("--output - writes to a pipe, and standard output is a terminal; name a new file instead")
		}
		return &secretOutput{path: path, stdout: stdout}, nil
	}
	f, err := privatefile.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create the output file (it must not exist): %w", err)
	}
	return &secretOutput{path: path, file: f, stdout: stdout}, nil
}

// write writes the secret and closes the file. A file that could not be
// written is removed.
func (o *secretOutput) write(secret string) error {
	if o.file == nil {
		return writeSecret(o.stdout, secret)
	}
	if err := errors.Join(writeSecret(o.file, secret), o.file.Close()); err != nil {
		os.Remove(o.path)
		return err
	}
	return nil
}

// discard removes the file of a secret that was never issued.
func (o *secretOutput) discard() {
	if o.file != nil {
		o.file.Close()
		os.Remove(o.path)
	}
}

// info is where the command's metadata goes: standard output, unless the
// secret goes there.
func (o *secretOutput) info(stderr io.Writer) io.Writer {
	if o.file == nil {
		return stderr
	}
	return o.stdout
}
