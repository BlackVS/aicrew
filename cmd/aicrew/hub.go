package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/svcconfig"
)

// aicrew hub add: binds an aimem hub in aicrewd.json from the directory
// aimem identity peer provision wrote (01a119aa-85ea). It edits the file on
// the machine aicrewd runs on; it does not call aicrewd.

const hubUsage = `usage:
  aicrew hub add NAME --config PATH --base-url URL --tls-trust-mode ca_dns|spki_sha256
                      --tls-trust-value VALUE --cred-dir DIR
`

// hubRemedies are what the operator does about a hub's refusal of the
// team read hub add makes.
var hubRemedies = map[string]string{
	"peer_unauthenticated": "the credentials in --cred-dir are not this hub's, or are revoked: run aimem identity peer provision on the hub again",
	"peer_unknown":         "the hub has no peer of this service_id: check service_id in aicrewd.json against aimem identity peer list",
	"peer_forbidden":       "the credentials belong to another peer than service_id: check service_id in aicrewd.json against aimem identity peer list",
	"profile_disabled":     "the peer is disabled on the hub: enable it, or provision this service again",
	"unsupported_version":  "the hub's aimem is older than this aicrew supports: upgrade it",
	"tls_required":         "the hub must terminate TLS itself: check --base-url",
	"rate_limited":         "the hub is busy: run the same command again shortly",
	"identity_unavailable": "the hub cannot read its identity store now: run the same command again shortly",
	hubteams.CodeUnavailable: "the hub could not be reached, or its TLS identity is not the one --tls-trust-mode and --tls-trust-value name: " +
		"check them and the network",
}

// exitIncomplete is hub add's exit status, as aicrewd config migrate's, for
// a file written but still refused by aicrewd: another hub has no hub_id.
const exitIncomplete = 3

// runHub is aicrew hub: 0 on success, 1 on a failure, 2 on a usage error,
// 3 when the file is written but another hub still lacks its hub_id.
func runHub(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) int {
	if len(args) < 2 || args[0] != "add" || strings.HasPrefix(args[1], "-") {
		fmt.Fprint(stderr, hubUsage)
		return 2
	}
	req := svcconfig.HubRequest{Name: args[1], Now: now}
	fs := flag.NewFlagSet("aicrew hub add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "aicrewd's configuration file (aicrewd.json)")
	fs.StringVar(&req.BaseURL, "base-url", "", "the hub's https origin")
	fs.StringVar(&req.TLSTrustMode, "tls-trust-mode", "", "ca_dns or spki_sha256")
	fs.StringVar(&req.TLSTrustValue, "tls-trust-value", "", "the hub's host name (ca_dns) or sha256- pin (spki_sha256)")
	fs.StringVar(&req.CredDir, "cred-dir", "", "the directory aimem identity peer provision wrote")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 ||
		*path == "" || req.BaseURL == "" || req.TLSTrustMode == "" || req.TLSTrustValue == "" || req.CredDir == "" {
		fmt.Fprint(stderr, hubUsage)
		return 2
	}
	r, err := svcconfig.AddHub(*path, req, liveTeamRead(ctx))
	if err != nil {
		fmt.Fprintf(stderr, "aicrew hub add: %v; nothing was written\n", err)
		return 1
	}
	verb := "added as"
	if r.Updated {
		verb = "updated in place at"
	}
	fmt.Fprintf(stdout, "hub %s (hub ID %s) %s aimem_hubs[%d]; the previous file is kept as %s\n", r.Hub.Name, r.Hub.HubID, verb, r.Index, r.Backup)
	if r.ReadScope != "" {
		fmt.Fprintf(stdout, "%s is not used: hub %s serves the reservation read scope, and only one hub may\n", svcconfig.ReadTokenFile, r.ReadScope)
	}
	if len(r.Pending) > 0 {
		fmt.Fprintf(stdout, "aicrewd refuses this file until these hubs have their hub_id: %s (run aicrew hub add for each); do not restart it yet\n", strings.Join(r.Pending, ", "))
		return exitIncomplete
	}
	fmt.Fprintln(stdout, "Restart aicrewd to apply it.")
	return 0
}

// liveTeamRead is hub add's probe: one read of the peer's teams with the
// team.read credential, which proves the hub is reachable under the trust
// binding and accepts this service's credential.
func liveTeamRead(ctx context.Context) svcconfig.Probe {
	return func(h svcconfig.AimemHub, serviceID string) error {
		cl, err := hubteams.New(hubteams.Config{BaseURL: h.BaseURL, ServiceID: serviceID, TLSMode: h.TLSTrustMode,
			TLSValue: h.TLSTrustValue, ReadTokenFile: h.TeamReadTokenFile})
		if err != nil {
			return err
		}
		if _, err := cl.ReadTeams(ctx); err != nil {
			code := hubteams.Code(err)
			if remedy, ok := hubRemedies[code]; ok {
				return fmt.Errorf("the hub refused the team read with %s: %s", code, remedy)
			}
			return fmt.Errorf("the team read failed: %w", err)
		}
		return nil
	}
}
