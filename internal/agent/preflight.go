package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/BlackVS/aicrew/internal/tlstrust"
)

// The preflight (01a1e7dd-90ed): what a person checks on a member's host
// before join, without spending anything. It reaches aicrewd's health
// route through the client join builds, with the trust join will record,
// and reads whether the client has a login stored. It sends no credential,
// redeems nothing, opens no session and calls no model.

// Preflight answers about aicrewd.
const (
	ReachReachable   = "reachable"
	ReachName        = "name"        // the host name does not resolve
	ReachRefused     = "refused"     // nothing answers at the address, or it times out
	ReachCertificate = "certificate" // the certificate does not match the trust
	ReachClock       = "clock"       // the certificate is not valid at this host's time
	ReachWrong       = "wrong_address"
)

// Preflight answers about the client's login.
const (
	LoginStored     = "stored"     // the client reports a stored login; its validity is not checked
	LoginNone       = "none"       // the client reports no login
	LoginNoCheck    = "no_check"   // the client has no check without a model call
	LoginNoClient   = "not_found"  // the client is not installed
	LoginUnreadable = "unreadable" // the client's answer could not be read
)

// PreflightOptions are join's connection flags and its client.
type PreflightOptions struct {
	URL    string
	Trust  tlstrust.Binding
	Client string
}

// PreflightDeps are the preflight's outside world; tests replace them.
type PreflightDeps struct {
	// Roots replaces the system roots for ca_dns trust; nil means the
	// system roots, as join uses.
	Roots *x509.CertPool
	// Now is this host's time, read for the clock answer.
	Now func() time.Time
	// AuthStatus runs `claude auth status --json` and returns its standard
	// output and exit code; an error means it could not run.
	AuthStatus func(ctx context.Context) ([]byte, int, error)
	// Dial replaces the connection's dialer; nil is join's, the system's.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// PreflightAicrewd is the answer about aicrewd.
type PreflightAicrewd struct {
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
	Detail  string `json:"detail"`
}

// PreflightLogin is the answer about the client's login.
type PreflightLogin struct {
	Client string `json:"client"`
	State  string `json:"state"`
	Method string `json:"method,omitempty"`
	Detail string `json:"detail"`
}

// PreflightReport is the preflight's whole answer. OK is true only when
// aicrewd is reachable and, where the client can say so, a login is
// stored.
type PreflightReport struct {
	OK      bool             `json:"ok"`
	URL     string           `json:"url"`
	Aicrewd PreflightAicrewd `json:"aicrewd"`
	Login   *PreflightLogin  `json:"login,omitempty"`
}

// turnModelNote names the one check that proves a login works, and its cost.
const turnModelNote = "only check's turn-model line, after join, proves that the login works: it makes one minimal model call (about $0.002)"

// Preflight checks o before join. An invalid URL or trust is an error; every
// other finding is in the report.
func Preflight(ctx context.Context, o PreflightOptions, d PreflightDeps) (PreflightReport, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return PreflightReport{}, errors.New("--url must be an https URL of aicrewd")
	}
	if err := o.Trust.Check(u.Hostname()); err != nil {
		return PreflightReport{}, err
	}
	if o.Client != "" && o.Client != "claude" && o.Client != "opencode" {
		return PreflightReport{}, fmt.Errorf("unknown client %q: claude or opencode", o.Client)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	rep := PreflightReport{URL: strings.TrimSuffix(o.URL, "/")}
	rep.Aicrewd = reachAicrewd(ctx, o, d)
	rep.OK = rep.Aicrewd.State == ReachReachable
	if o.Client != "" {
		l := clientLogin(ctx, o.Client, d)
		rep.Login = &l
		rep.OK = rep.OK && (l.State == LoginStored || l.State == LoginNoCheck)
	}
	return rep, nil
}

// reachAicrewd calls GET /healthz with join's client and names what failed.
func reachAicrewd(ctx context.Context, o PreflightOptions, d PreflightDeps) PreflightAicrewd {
	crew, err := NewCrew(Config{URL: o.URL, Trust: o.Trust}, d.Roots)
	if err != nil {
		return PreflightAicrewd{State: ReachCertificate, Detail: err.Error()}
	}
	if d.Dial != nil {
		crew.client.Transport.(*http.Transport).DialContext = d.Dial
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, crew.base+"/healthz", nil)
	if err != nil {
		return PreflightAicrewd{State: ReachWrong, Detail: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := crew.client.Do(req)
	if err != nil {
		return reachFailure(err, o, d.Now())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var h struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &h) != nil || h.Status != "ok" || h.Version == "" {
		return PreflightAicrewd{State: ReachWrong, Detail: fmt.Sprintf("something answers at %s, but not aicrewd's "+
			"health route (HTTP %d): check the URL's host, port and path", crew.base, resp.StatusCode)}
	}
	return PreflightAicrewd{State: ReachReachable, Version: h.Version,
		Detail: fmt.Sprintf("aicrewd %s answers at %s, with the certificate this trust accepts", h.Version, crew.base)}
}

// reachFailure names a failed request: the name, the connection, the clock
// or the certificate.
func reachFailure(err error, o PreflightOptions, now time.Time) PreflightAicrewd {
	var dns *net.DNSError
	var inv x509.CertificateInvalidError
	switch {
	case errors.As(err, &dns):
		return PreflightAicrewd{State: ReachName, Detail: fmt.Sprintf("the name %s does not resolve on this host: "+
			"check the URL and this host's DNS (%v)", dns.Name, dns.Err)}
	case errors.As(err, &inv) && inv.Reason == x509.Expired:
		// Only ca_dns checks the dates: under spki_sha256 the pin replaces
		// the chain check, so this answer cannot come from a pin.
		return PreflightAicrewd{State: ReachClock, Detail: fmt.Sprintf("aicrewd's certificate is not valid at this "+
			"host's time %s (valid from %s to %s): check this host's clock first, then the certificate",
			now.UTC().Format(time.RFC3339), inv.Cert.NotBefore.UTC().Format(time.RFC3339),
			inv.Cert.NotAfter.UTC().Format(time.RFC3339))}
	case tlstrust.Untrusted(err):
		what := "the CA this host trusts or the name " + o.Trust.Value
		if o.Trust.Mode == tlstrust.SPKI {
			what = "the pinned key"
		}
		return PreflightAicrewd{State: ReachCertificate, Detail: fmt.Sprintf("aicrewd's certificate does not match "+
			"%s: check --tls-trust-mode and --tls-trust-value with the operator (%v)", what, err)}
	case errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) ||
		isDial(err):
		return PreflightAicrewd{State: ReachRefused, Detail: fmt.Sprintf("nothing answers at %s: check that "+
			"aicrewd runs, its port, and any firewall between (%v)", o.URL, err)}
	}
	return PreflightAicrewd{State: ReachWrong, Detail: fmt.Sprintf("the request to %s failed: %v", o.URL, err)}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isDial(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// clientLogin reads whether the client has a login stored, without a model
// call. Claude Code's `auth status` reads the stored login only: observed
// in 2.1.286 to report a fake, an expired and an offline login alike as
// logged in. So a stored login is all it shows.
func clientLogin(ctx context.Context, client string, d PreflightDeps) PreflightLogin {
	if client != "claude" {
		return PreflightLogin{Client: client, State: LoginNoCheck, Detail: client + " offers no check of its login " +
			"without a model call: " + turnModelNote}
	}
	run := d.AuthStatus
	if run == nil {
		run = claudeAuthStatus
	}
	out, code, err := run(ctx)
	if errors.Is(err, exec.ErrNotFound) {
		return PreflightLogin{Client: client, State: LoginNoClient, Detail: "claude is not installed: run " +
			"`aicrew-agent check` for the install line of the tested version"}
	}
	var st struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err != nil || json.Unmarshal(out, &st) != nil || st.LoggedIn == nil {
		detail := "`claude auth status --json` gave no answer this preflight reads"
		if err != nil {
			detail += ": " + err.Error()
		}
		return PreflightLogin{Client: client, State: LoginUnreadable, Detail: detail}
	}
	if !*st.LoggedIn || code != 0 {
		return PreflightLogin{Client: client, State: LoginNone, Detail: "claude has no login stored for this OS " +
			"user: run `claude` and log in"}
	}
	return PreflightLogin{Client: client, State: LoginStored, Method: st.AuthMethod, Detail: "claude has a login " +
		"stored (" + st.AuthMethod + "); whether it is valid is not checked here: " + turnModelNote}
}

// claudeAuthStatus runs `claude auth status --json` from PATH.
func claudeAuthStatus(ctx context.Context) ([]byte, int, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, 0, exec.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "auth", "status", "--json").Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode(), nil
	}
	return out, 0, err
}
