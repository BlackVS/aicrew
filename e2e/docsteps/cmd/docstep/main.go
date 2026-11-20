// Command docstep gives a scenario the command lines of docs/BRING-UP.md
// (package docsteps) and checks, at the end of its run, that it ran every
// step it declares.
//
//	docstep line -doc FILE -id ID [-set NAME=VALUE]... [-add TEXT]... [-per WORD] [-alt]
//	docstep verify -scenarios FILE -scenario NAME -ran FILE
//
// line prints the step's line with its placeholders filled. verify reads
// the ids the run recorded in -ran, one per line, and fails naming each id
// the scenario declares in -scenarios and did not run.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/BlackVS/aicrew/e2e/docsteps"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: docstep line|verify ...")
		return 2
	}
	fs := flag.NewFlagSet("docstep "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch args[0] {
	case "line":
		doc := fs.String("doc", "", "the document")
		id := fs.String("id", "", "the step's id")
		per := fs.String("per", "", "the member's value for the step's per= word")
		alt := fs.Bool("alt", false, "the step's marked alternative")
		var sets, adds list
		fs.Var(&sets, "set", "NAME=VALUE for a placeholder")
		fs.Var(&adds, "add", "a marked addition of the step")
		if fs.Parse(args[1:]) != nil || *doc == "" || *id == "" || fs.NArg() != 0 {
			return 2
		}
		d, err := docsteps.Load(*doc)
		if err != nil {
			fmt.Fprintln(stderr, "docstep:", err)
			return 1
		}
		values := map[string]string{}
		for _, s := range sets {
			k, v, ok := strings.Cut(s, "=")
			if !ok {
				fmt.Fprintf(stderr, "docstep: -set %q is not NAME=VALUE\n", s)
				return 2
			}
			values[k] = v
		}
		line, err := d.Line(*id, docsteps.Options{Values: values, Per: *per, Adds: adds, Alt: *alt})
		if err != nil {
			fmt.Fprintln(stderr, "docstep:", err)
			return 1
		}
		fmt.Fprintln(stdout, line)
		return 0
	case "verify":
		scen := fs.String("scenarios", "", "the scenarios' declarations")
		name := fs.String("scenario", "", "the scenario")
		ran := fs.String("ran", "", "the ids the run recorded")
		if fs.Parse(args[1:]) != nil || *scen == "" || *name == "" || *ran == "" || fs.NArg() != 0 {
			return 2
		}
		decl, err := docsteps.LoadScenarios(*scen)
		if err != nil {
			fmt.Fprintln(stderr, "docstep:", err)
			return 1
		}
		ids, ok := decl[*name]
		if !ok {
			fmt.Fprintf(stderr, "docstep: scenario %s declares no steps in %s\n", *name, *scen)
			return 1
		}
		raw, err := os.ReadFile(*ran)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(stderr, "docstep:", err)
			return 1
		}
		done := map[string]bool{}
		for _, l := range strings.Fields(string(raw)) {
			done[l] = true
		}
		var missing []string
		for _, id := range ids {
			if !done[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(stderr, "docstep: scenario %s did not run the document's steps %s\n", *name, strings.Join(missing, ", "))
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "usage: docstep line|verify ...")
	return 2
}
