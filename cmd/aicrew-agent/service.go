package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/service"
)

const serviceUsage = `usage: aicrew-agent service install|uninstall|start|stop|restart|status (--home DIR | --all) [--json]

The member's turn loop (aicrew-agent serve) as a service of this account: a
systemd user unit, a launchd agent or a logon task, one per home in turns
mode. It starts at login and after a failure, and outlives the terminal.

  install    write and register the home's service and start it (join does
             this for a home in turns mode); rerun to refresh it
  uninstall  stop and remove it
  start      refresh and start an installed service
  stop       stop it; it starts again at the next login
  restart    stop, then start
  status     installed, running, or a serve started by hand

--all acts on every home with a service (not install). A serve started by
hand is reported and never duplicated: stop it, then start the service.`

// newServiceManager is this platform's service manager; tests replace it.
var newServiceManager = func(getenv func(string) string) (*service.Manager, error) {
	reg, err := service.DefaultRegistry(getenv)
	if err != nil {
		return nil, err
	}
	b, err := service.NewBackend(runtime.GOOS, service.ExecRunner, reg.Dir())
	if err != nil {
		return nil, err
	}
	return &service.Manager{Backend: b, Registry: reg}, nil
}

// serviceExecutable is the aicrew-agent a service runs: this one.
var serviceExecutable = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

// serviceSpec is the home's service, after checking that the home can run
// one: an agent home in turns mode.
func serviceSpec(home string, getenv func(string) string) (service.Spec, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return service.Spec{}, err
	}
	if _, err := agent.LoadConfig(abs); err != nil {
		return service.Spec{}, fmt.Errorf("not a usable agent home: %w", err)
	}
	if agent.ReadWakeSettings(abs).Mode != agent.WakeTurns {
		return service.Spec{}, errors.New("this home is in stop-hook mode: its member runs `aicrew-agent run` in a " +
			"terminal. For the service, put it in turns mode first: aicrew-agent join --home " + abs + " --wake turns")
	}
	exe, err := serviceExecutable()
	if err != nil {
		return service.Spec{}, err
	}
	return service.NewSpec(abs, agent.HomeLabel(abs), exe, getenv("PATH")), nil
}

func serviceCmd(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, serviceUsage)
		return exitUsage
	}
	verb := args[0]
	fs := flag.NewFlagSet("aicrew-agent service", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "the agent home")
	all := fs.Bool("all", false, "every home with a service")
	asJSON := fs.Bool("json", false, "print JSON")
	known := map[string]bool{"install": true, "uninstall": true, "start": true, "stop": true, "restart": true, "status": true}
	if err := fs.Parse(args[1:]); err != nil || !known[verb] || fs.NArg() != 0 || (*home == "") == !*all ||
		(*all && verb == "install") {
		fmt.Fprintln(stderr, serviceUsage)
		return exitUsage
	}
	m, err := newServiceManager(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "aicrew-agent service:", err)
		return exitFailed
	}
	var homes []string
	if *all {
		entries, err := m.Registry.Entries()
		if err != nil {
			fmt.Fprintln(stderr, "aicrew-agent service:", err)
			return exitFailed
		}
		for _, e := range entries {
			homes = append(homes, e.Home)
		}
	} else {
		abs, err := filepath.Abs(*home)
		if err != nil {
			fmt.Fprintln(stderr, "aicrew-agent service:", err)
			return exitFailed
		}
		homes = []string{abs}
	}
	rc := exitOK
	var report []service.Status
	for _, h := range homes {
		st, err := serviceAct(ctx, m, verb, h, getenv)
		if st.Name != "" {
			report = append(report, st)
		}
		if !*asJSON && st.Name != "" {
			fmt.Fprintf(stdout, "%s: %s (%s, %s)\n", st.Name, st.State(), st.Kind, h)
			for _, n := range st.Notes {
				fmt.Fprintln(stdout, "  note:", n)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "aicrew-agent service %s %s: %v\n", verb, h, err)
			rc = exitFailed
		}
	}
	if *asJSON {
		if report == nil {
			report = []service.Status{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return exitFailed
		}
	}
	return rc
}

func serviceAct(ctx context.Context, m *service.Manager, verb, home string, getenv func(string) string) (service.Status, error) {
	label := agent.HomeLabel(home)
	switch verb {
	case "status":
		return m.Status(ctx, home, label)
	case "stop":
		return m.Stop(ctx, home, label)
	case "uninstall":
		return m.Uninstall(ctx, home, label)
	}
	spec, err := serviceSpec(home, getenv)
	if err != nil {
		st, _ := m.Status(ctx, home, label)
		return st, err
	}
	switch verb {
	case "install":
		return m.Install(ctx, spec)
	case "restart":
		if st, err := m.Stop(ctx, home, label); err != nil {
			return st, err
		}
	}
	return m.Start(ctx, spec)
}
