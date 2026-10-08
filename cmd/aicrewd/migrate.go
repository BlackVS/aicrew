package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/BlackVS/aicrew/internal/svcconfig"
)

// exitIncomplete is migrate's exit status for a file aicrewd still refuses
// because a hub has no hub_id: written or left as it was, but not to be
// started with yet.
const exitIncomplete = 3

// configCommand answers `aicrewd config migrate` and `aicrewd config show`;
// ok is false for any other command line.
func configCommand(args []string, stdout, stderr io.Writer, now time.Time) (int, bool) {
	if len(args) == 0 || args[0] != "config" {
		return 0, false
	}
	switch {
	case len(args) >= 2 && args[1] == "migrate":
		return migrate(args[2:], stdout, stderr, now), true
	case len(args) >= 2 && args[1] == "show":
		return show(args[2:], stdout, stderr), true
	}
	fmt.Fprintln(stderr, "usage: aicrewd config migrate -config PATH [-name NAME] [-hub-id ID]\n"+
		"         [-team-register-token-file FILE] [-team-read-token-file FILE] [-service-id ID]\n"+
		"         [-cred-dir DIR]\n"+
		"       aicrewd config show -config PATH")
	return 2, true
}

// show prints what an installer needs of a configuration, one name=value
// per line, so that it parses no JSON: the store, the listen address, the
// service ID, and whether the file still has the aimem block of 0.2.0. A
// file aicrewd refuses for any other reason than a missing hub_id is
// refused.
func show(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrewd config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to aicrewd's JSON configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	c, err := svcconfig.ShowConfig(*path)
	if err != nil {
		fmt.Fprintf(stderr, "aicrewd config show: %v\n", err)
		return 1
	}
	legacy := "no"
	if c.Aimem != nil {
		legacy = "yes"
	}
	fmt.Fprintf(stdout, "store_path=%s\nlisten_addr=%s\nservice_id=%s\nlegacy_aimem_block=%s\n", c.StorePath, c.ListenAddr, c.ServiceID, legacy)
	return 0
}

func migrate(args []string, stdout, stderr io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("aicrewd config migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to aicrewd's JSON configuration file")
	opt := svcconfig.MigrateOptions{Now: now}
	fs.StringVar(&opt.Name, "name", "", "the hub's alias in aimem_hubs (default \""+svcconfig.LegacyHubName+"\")")
	fs.StringVar(&opt.HubID, "hub-id", "", "the hub's ID, as `aimem identity peer list` shows it")
	fs.StringVar(&opt.TeamRegisterTokenFile, "team-register-token-file", "", "the file holding the team.register credential")
	fs.StringVar(&opt.TeamReadTokenFile, "team-read-token-file", "", "the file holding the team.read credential")
	fs.StringVar(&opt.ServiceID, "service-id", "", "a new service_id, the peer ID the hub lists for this service")
	fs.StringVar(&opt.CredDir, "cred-dir", "", "the directory aimem identity peer provision wrote: the hub ID and the two team credential files come from it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	r, err := svcconfig.MigrateConfig(*path, opt)
	if err != nil {
		fmt.Fprintf(stderr, "aicrewd config migrate: %v; nothing was written\n", err)
		return 1
	}
	if r.Migrated {
		fmt.Fprintf(stdout, "%s: the aimem block is now an aimem_hubs entry; the previous file is kept as %s\n", *path, r.Backup)
	} else {
		fmt.Fprintf(stdout, "%s: no aimem block to migrate; nothing changed\n", *path)
	}
	if len(r.Missing) > 0 {
		fmt.Fprintln(stdout, "Still to supply:")
		for _, m := range r.Missing {
			fmt.Fprintf(stdout, "  %s: %s\n", m.Field, m.From)
		}
	}
	if r.Hubs > 0 {
		fmt.Fprintf(stdout, "Check: service_id %q must be the peer ID each hub lists for this service (`aimem identity peer list`).\n", r.ServiceID)
	}
	if r.Incomplete() {
		fmt.Fprintln(stdout, "aicrewd refuses this file until every hub has its hub_id: do not restart it yet.")
		return exitIncomplete
	}
	if r.Migrated {
		fmt.Fprintln(stdout, "Restart aicrewd to apply it.")
	}
	return 0
}
