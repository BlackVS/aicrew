// Command stubagent stands in for aicrew-agent (and aicrew) in
// TestBootRestartsTheServices. `version` prints the version it was built
// with (-X main.version). `service ...` appends its version, the name of
// the directory it runs from and its arguments to the file STUB_LOG names,
// and, for `service stop --all` with STUB_SERVICES=1, prints a stopped
// service as aicrew-agent does.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var version = "v0.0.0"

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "version" {
		fmt.Printf("aicrew-agent %s (stub)\n", version)
		return
	}
	if len(args) > 0 && args[0] == "service" {
		exe, err := os.Executable()
		if err != nil {
			os.Exit(1)
		}
		f, err := os.OpenFile(os.Getenv("STUB_LOG"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			os.Exit(1)
		}
		fmt.Fprintf(f, "%s %s %s\n", version, filepath.Base(filepath.Dir(exe)), strings.Join(args, " "))
		f.Close()
		if strings.Join(args, " ") == "service stop --all" && os.Getenv("STUB_SERVICES") == "1" {
			fmt.Println("aicrew-member-sol-0a1b2c3d: stopped (stub, /h/sol)")
		}
		return
	}
	os.Exit(2)
}
