// Command simca is the pilot simulation's sim-ca job: it writes the run's
// throwaway CA, a server certificate per service and their SPKI pins into
// one directory, the volume the other containers mount
// (docs/DESIGN-PILOT-SIMULATION.md, section 3):
//
//	ca.pem          the CA, which SSL_CERT_FILE and --aimem-ca-file name
//	NAME.pem        the service's certificate, for the DNS name NAME
//	NAME.key        its key, owner only; the service's container installs it
//	pins.env        PIN_NAME=sha256-… per service (NAME upper-cased, - as _)
//
// A directory that already holds ca.pem is left as it is, so a restarted
// stack keeps its pins.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/e2e/testca"
)

// validity outlasts any run's hard bound.
const validity = 24 * time.Hour

type names []string

func (n *names) String() string     { return strings.Join(*n, ",") }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("simca", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "the directory to write into")
	var leaves names
	fs.Var(&leaves, "leaf", "a service's DNS name (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" || len(leaves) == 0 || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: simca -out DIR -leaf NAME [-leaf NAME ...]")
		return 2
	}
	if _, err := os.Stat(filepath.Join(*out, "ca.pem")); err == nil {
		fmt.Fprintf(stdout, "simca: %s already holds the run's CA; nothing written\n", *out)
		return 0
	}
	if err := issue(*out, leaves); err != nil {
		fmt.Fprintln(stderr, "simca:", err)
		return 1
	}
	fmt.Fprintf(stdout, "simca: the CA and %d service certificates are in %s\n", len(leaves), *out)
	return 0
}

func issue(out string, leaves []string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	ca, err := testca.New("aicrew pilot simulation CA", validity)
	if err != nil {
		return err
	}
	var pins strings.Builder
	for _, name := range leaves {
		if name == "" || strings.ContainsAny(name, "/\\ =") {
			return fmt.Errorf("%q is not a DNS name", name)
		}
		l, err := ca.Leaf(name, []string{name}, nil, validity)
		if err != nil {
			return err
		}
		if err := testca.WritePEM(filepath.Join(out, name+".pem"), "CERTIFICATE", l.CertDER, 0o644); err != nil {
			return err
		}
		if err := testca.WritePEM(filepath.Join(out, name+".key"), "EC PRIVATE KEY", l.KeyDER, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(&pins, "PIN_%s=%s\n", strings.ToUpper(strings.ReplaceAll(name, "-", "_")), l.Pin)
	}
	if err := os.WriteFile(filepath.Join(out, "pins.env"), []byte(pins.String()), 0o644); err != nil {
		return err
	}
	// The CA last: its presence marks a complete directory.
	return testca.WritePEM(filepath.Join(out, "ca.pem"), "CERTIFICATE", ca.DER, 0o644)
}
