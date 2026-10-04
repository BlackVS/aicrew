package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/filelock"
	"github.com/BlackVS/aicrew/internal/invitecode"
	"github.com/BlackVS/aicrew/internal/tlstrust"
)

// The client bootstrap (docs/ONBOARDING-CONTRACT.md, "The client's view"):
// check the individual aimem credential, read the invitation code at a
// hidden prompt, redeem it (begin, aimem proof, complete), and prepare the
// agent home (docs/WORKSPACE.md). It opens no session and writes no secret:
// the code and the receipt live only in memory and in the request bodies.

// Join statuses.
const (
	JoinReady           = "ready"
	JoinRestartRequired = "restart_required"
	JoinBlocked         = "blocked"
)

// Bounds of one run. A begin with a new key counts an invitation attempt,
// and the invitation locks at its fifth, so a run spends at most three.
const (
	maxBegins     = 3
	maxProofs     = 3 // new receipts for one challenge after proof_invalid
	maxRetries    = 5 // same-key retries of one call
	maxCodeTries  = 3 // prompts for a malformed code
	joinRetryBase = 2 * time.Second
	joinRetryCap  = 30 * time.Second
)

var (
	labelShape    = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	aimemHubShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// JoinOptions is one bootstrap run.
type JoinOptions struct {
	Home         string
	Label        string // required on the first run; kept from agent.json after
	URL          string // aicrewd's https origin; may be omitted on a linked home
	Trust        tlstrust.Binding
	AimemHub     string // aimem's name for the hub; required on the first run
	AimemCommand string // recorded when set
	// Terminal reports whether the code can be read at a hidden prompt. The
	// code is never taken from anywhere else.
	Terminal bool
	// Clients are the clients the home is for (claude, opencode): required
	// on the first run, kept in agent.json after.
	Clients []string
	// AimemURL, AimemTokenFile and AimemCAFile provision the home's aimem
	// installation on the first run (provision.go): the hub's origin, the
	// file holding the member's token (or "-" for the hidden prompt) and,
	// for a hub on a private CA, its CA bundle.
	AimemURL, AimemTokenFile, AimemCAFile string
}

// JoinDeps are the bootstrap's collaborators; tests replace them.
type JoinDeps struct {
	Crew     func(Config) (InvitationAPI, error)
	Aimem    func(command, hub, home string) JoinAimem // the home's installation
	ReadCode func() (string, error)                    // the hidden prompt
	// ReadToken reads the member's aimem token at a hidden prompt, for
	// --aimem-token-file -.
	ReadToken func() (string, error)
	Out       io.Writer // the plan and progress, never a secret
	Sleep     func(context.Context, time.Duration) error
	// check runs the dependency and client check at the end of a run; nil
	// means the real one (runCheck). Package tests replace it.
	check func(ctx context.Context, o CheckOptions, doc *agentDoc, newHome bool) (CheckReport, error)
}

// JoinAimem is what the bootstrap asks of aimem.
type JoinAimem interface {
	// Credential reports the installation's individual credential for the
	// hub. known is false when this aimem cannot answer (it predates the
	// command); the proof then decides.
	Credential(ctx context.Context) (st CredentialStatus, known bool, err error)
	Proof(ctx context.Context, serviceID, hubID, challengeID string) (string, error)
	// Provision gives the home's installation the hub and the member's task
	// credential, the token on aimem's standard input only.
	Provision(ctx context.Context, hubURL, caFile, token string) error
}

// JoinReport is the bootstrap's result. It never carries a secret.
type JoinReport struct {
	Status      string       `json:"status"`
	Reason      string       `json:"reason,omitempty"`
	Instruction string       `json:"instruction,omitempty"`
	Home        string       `json:"home"`
	AgentID     string       `json:"agent_id,omitempty"`
	TeamID      string       `json:"team_id,omitempty"`
	Role        string       `json:"role,omitempty"`
	Redeemed    bool         `json:"redeemed"` // this run redeemed an invitation
	Changes     []FileChange `json:"changes,omitempty"`
	Next        string       `json:"next,omitempty"`
	// Check is the dependency and client check of the home (1a81-5a).
	Check *CheckReport `json:"check,omitempty"`
}

// ErrJoinUsage is an invalid option; the report's instruction says which.
var ErrJoinUsage = errors.New("invalid join options")

// DefaultHome is the default agent home for a label (docs/WORKSPACE.md).
func DefaultHome(label string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "aicrew", "agents", label), nil
}

type joiner struct {
	o     JoinOptions
	deps  JoinDeps
	doc   agentDoc
	aimem JoinAimem // set by precheck for an unlinked home
}

func blocked(o JoinOptions, reason, instruction string) JoinReport {
	return JoinReport{Status: JoinBlocked, Reason: reason, Instruction: instruction, Home: o.Home}
}

// Join runs the bootstrap. A blocked report is a clean stop that a rerun
// resumes once its instruction is followed; an error is a local failure
// (the file system, or an option: ErrJoinUsage).
func Join(ctx context.Context, o JoinOptions, deps JoinDeps) (JoinReport, error) {
	if deps.Sleep == nil {
		deps.Sleep = sleepCtx
	}
	if deps.check == nil {
		deps.check = runCheck
	}
	if deps.Out == nil {
		deps.Out = io.Discard
	}
	if o.Home == "" {
		return blocked(o, "usage", "--home or --label is required"), ErrJoinUsage
	}
	if abs, err := filepath.Abs(o.Home); err == nil {
		o.Home = abs
	}
	doc, _, err := readAgentDoc(o.Home)
	if err != nil {
		return blocked(o, "agent_json_invalid", err.Error()+"; fix or move it, then rerun"), nil
	}
	if v, ok := doc.layout(); ok && v != LayoutVersion {
		return blocked(o, "layout_unsupported", fmt.Sprintf("agent.json has layout version %v; this aicrew-agent "+
			"creates and understands version %d and migrates nothing. Use a matching aicrew-agent or a new --home.",
			string(doc.top["layout"]), LayoutVersion)), nil
	}
	j := &joiner{o: o, deps: deps, doc: doc}
	if err := j.options(); err != nil {
		return blocked(o, "usage", err.Error()), ErrJoinUsage
	}
	// The checks without side effects come first: a refused run creates
	// nothing.
	if r, stop := j.precheck(ctx); stop {
		return r, nil
	}
	if err := makeLayout(j.o.Home); err != nil {
		return JoinReport{}, err
	}
	// One run at a time per home. A run that waited here reads what the
	// other wrote, so it never writes back an agent.json read before the
	// home was linked.
	unlock, err := filelock.Lock(ctx, filepath.Join(j.o.Home, "state", homeLockName))
	if err != nil {
		return JoinReport{}, err
	}
	defer unlock()
	linked := j.doc.linked()
	if j.doc, _, err = readAgentDoc(j.o.Home); err != nil {
		return blocked(o, "agent_json_invalid", err.Error()+"; fix or move it, then rerun"), nil
	}
	if j.doc.linked() != linked {
		if err := j.options(); err != nil {
			return blocked(o, "usage", err.Error()), ErrJoinUsage
		}
		if r, stop := j.precheck(ctx); stop {
			return r, nil
		}
	}
	if j.doc.linked() {
		fmt.Fprintf(j.deps.Out, "This home is linked to agent %s in team %s: refreshing its files and checking its clients.\n",
			str(j.doc.aicrew, "agent_id"), str(j.doc.aicrew, "team_id"))
		return j.prepare(ctx, false)
	}
	return j.join(ctx)
}

// options completes the options from agent.json and checks them.
func (j *joiner) options() error {
	o := &j.o
	if o.Label == "" {
		o.Label = str(j.doc.top, "label")
	}
	if !labelShape.MatchString(o.Label) {
		return errors.New("--label must be 1 to 32 lowercase letters, digits and '-'")
	}
	if o.AimemHub != "" && !aimemHubShape.MatchString(o.AimemHub) {
		return errors.New("--aimem-hub must be an aimem hub name: lowercase letters, digits and '-'")
	}
	if _, err := selectClients(o.Clients, j.doc); err != nil {
		return err
	}
	if err := o.checkProvisionOptions(j.doc.linked()); err != nil {
		return err
	}
	if j.doc.linked() {
		return nil // refresh checks the flags against the recorded binding
	}
	if len(o.Clients) == 0 && len(j.doc.clients()) == 0 {
		return errors.New("--client claude or --client opencode is required to join: the client this home is for")
	}
	if o.URL == "" || o.Trust.Mode == "" || o.AimemHub == "" {
		return errors.New("--url, --tls-trust-mode, --tls-trust-value and --aimem-hub are required to join")
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("--url must be an https origin without credentials, path, query or fragment")
	}
	o.URL = strings.TrimSuffix(o.URL, "/")
	if err := o.Trust.Check(u.Hostname()); err != nil {
		return err
	}
	return nil
}

// otherBinding refuses options naming another binding for a linked home: a
// home serves one team. A rerun only refreshes the layout and the managed
// files.
func (j *joiner) otherBinding() (JoinReport, bool) {
	o, a := j.o, j.doc.aicrew
	for _, c := range []struct{ flag, given, recorded string }{
		{"-url", strings.TrimSuffix(o.URL, "/"), str(a, "url")},
		{"-tls-trust-mode", o.Trust.Mode, str(a, "tls_trust_mode")},
		{"-tls-trust-value", o.Trust.Value, str(a, "tls_trust_value")},
		{"-aimem-hub", o.AimemHub, str(a, "aimem_hub")},
	} {
		if c.given != "" && c.given != c.recorded {
			return blocked(o, "home_linked", fmt.Sprintf("this home is already linked to agent %s in team %s "+
				"with a different %s; one home serves one team: use a new --home to join another",
				str(a, "agent_id"), str(a, "team_id"), c.flag)), true
		}
	}
	return JoinReport{}, false
}

// precheck decides, without changing anything, whether the run may go on:
// a linked home must not be asked to serve another binding; an unlinked one
// needs a terminal for the code and an individual aimem credential (D1).
func (j *joiner) precheck(ctx context.Context) (JoinReport, bool) {
	if j.doc.linked() {
		return j.otherBinding()
	}
	if !j.o.Terminal {
		return blocked(j.o, "no_terminal", "run aicrew-agent join in an interactive terminal: the invitation "+
			"code is read only at its hidden prompt, never from an argument, a pipe, a file or the environment"), true
	}
	j.aimem = j.deps.Aimem(j.aimemCommand(), j.o.AimemHub, j.o.Home)
	if r, stop := j.provision(ctx); stop {
		return r, true
	}
	return j.checkCredential(ctx, j.aimem)
}

// join links an unlinked home.
func (j *joiner) join(ctx context.Context) (JoinReport, error) {
	o, am := j.o, j.aimem
	j.bind()
	if err := j.doc.write(o.Home); err != nil {
		return JoinReport{}, err
	}
	crew, err := j.deps.Crew(Config{Home: o.Home, URL: o.URL, Trust: o.Trust})
	if err != nil {
		return blocked(o, "tls_trust", err.Error()), nil
	}
	code, r, ok := j.readCode()
	if !ok {
		return r, nil
	}
	st, err := loadJoinState(o.Home, code, o.URL)
	if err != nil {
		return JoinReport{}, err
	}
	res, r, ok, err := j.redeem(ctx, crew, am, code, st)
	if err != nil || !ok {
		return r, err
	}
	j.doc.set(j.doc.aicrew, "agent_id", res.AgentID)
	j.doc.set(j.doc.aicrew, "team_id", res.TeamID)
	if err := j.doc.write(o.Home); err != nil {
		return JoinReport{}, err
	}
	if err := clearJoinState(o.Home); err != nil {
		return JoinReport{}, err
	}
	fmt.Fprintf(j.deps.Out, "Linked: agent %s, team %s, role %s.\n", res.AgentID, res.TeamID, res.Role)
	rep, err := j.prepare(ctx, true)
	rep.Role = res.Role
	return rep, err
}

func (j *joiner) aimemCommand() string {
	if j.o.AimemCommand != "" {
		return j.o.AimemCommand
	}
	if c := str(j.doc.aicrew, "aimem_command"); c != "" {
		return c
	}
	return "aimem"
}

// bind records the unlinked home's nonsecret binding in agent.json.
func (j *joiner) bind() {
	o, d := j.o, j.doc
	d.set(d.top, "layout", LayoutVersion)
	d.set(d.top, "label", o.Label)
	d.set(d.aicrew, "url", o.URL)
	d.set(d.aicrew, "tls_trust_mode", o.Trust.Mode)
	d.set(d.aicrew, "tls_trust_value", o.Trust.Value)
	d.set(d.aicrew, "aimem_hub", o.AimemHub)
	if o.AimemCommand != "" {
		d.set(d.aicrew, "aimem_command", o.AimemCommand)
	}
}

// credentialInstruction tells how to provision the home's aimem
// installation with the individual credential.
func credentialInstruction(home, hub string) string { return provisionInstruction(home, hub) }

// checkCredential is decision D1: an individual aimem credential must be
// installed and accepted before an invitation attempt is spent.
func (j *joiner) checkCredential(ctx context.Context, am JoinAimem) (JoinReport, bool) {
	o := j.o
	st, known, err := am.Credential(ctx)
	switch {
	case err != nil && strings.Contains(err.Error(), "is not configured"):
		// The home's installation does not know the hub yet.
		return blocked(o, "credential_missing", credentialInstruction(o.Home, o.AimemHub)), true
	case err != nil:
		return blocked(o, "aimem_failed", err.Error()), true
	case !known:
		fmt.Fprintln(j.deps.Out, "This aimem cannot report its credential; the identity proof will check it.")
		return JoinReport{}, false
	case st.Credential != "set":
		return blocked(o, "credential_missing", credentialInstruction(o.Home, o.AimemHub)), true
	case st.State == "refused":
		return blocked(o, "credential_refused", fmt.Sprintf("the aimem hub %q does not accept the stored "+
			"individual credential (revoked, expired or unknown): ask your aimem operator to reissue it, install "+
			"it with `aimem hub task-token %s --token-file -` under the home's AIMEM_STATE_DIR=%s and AIMEM_SOCKET=%s, "+
			"then rerun", o.AimemHub, o.AimemHub, AimemDir(o.Home), AimemSocket(o.Home))), true
	case st.State != "active":
		return blocked(o, "aimem_unreachable", fmt.Sprintf("the aimem hub %q could not confirm the credential "+
			"(%s: %s); rerun when it is reachable", o.AimemHub, st.State, st.Detail)), true
	}
	fmt.Fprintf(j.deps.Out, "aimem credential for hub %q: active (user %s).\n", o.AimemHub, st.UserID)
	return JoinReport{}, false
}

// readCode reads the code at the hidden prompt and checks its form, so a
// typing error never costs an invitation attempt.
func (j *joiner) readCode() (string, JoinReport, bool) {
	for try := 1; ; try++ {
		code, err := j.deps.ReadCode()
		if err != nil {
			return "", blocked(j.o, "no_code", "the invitation code could not be read: "+err.Error()), false
		}
		norm, err := invitecode.Normalize(code)
		if err == nil {
			return norm, JoinReport{}, true
		}
		fmt.Fprintf(j.deps.Out, "That is not a valid invitation code (%s).\n", strings.TrimPrefix(err.Error(),
			invitecode.ErrMalformed.Error()+": "))
		if try == maxCodeTries {
			return "", blocked(j.o, "code_malformed", "check the invitation code with the operator who issued "+
				"it, then rerun"), false
		}
	}
}

// stops are the refusals that end a run, with what to do about them.
var stops = map[string]string{
	"invitation_invalid": "the invitation is expired, revoked, already redeemed, locked or unknown: ask the " +
		"operator for a new invitation",
	"identity_already_linked": "this aimem user is already linked to another aicrew agent (one user, one " +
		"agent): ask the operator for an invitation for that agent, or use a different aimem user",
	"role_conflict": "the agent is already a member of this team with another role; role changes are " +
		"operator operations: ask the operator",
	"identity_mismatch": "the proven aimem identity is not the one the invitation is for (another hub or " +
		"user, or the agent is linked to another user): check --aimem-hub, then ask the operator",
	"work_outstanding": "the agent has outstanding work, so it cannot be rebound yet: finish or release " +
		"it, then rerun",
	"credential_inactive": "the aimem hub no longer accepts this installation's credential: ask your aimem " +
		"operator to reissue it, install it with aimem hub task-token, then rerun",
	"aimem_unconfigured": "aicrewd cannot verify aimem identities: tell the aicrew operator",
}

// redeem runs begin, proof and complete with the contract's recovery
// (docs/ONBOARDING-CONTRACT.md, "Retries and lost replies"). The join state
// is saved before each call, so a rerun with the same code reuses its keys.
func (j *joiner) redeem(ctx context.Context, crew InvitationAPI, am JoinAimem, code string,
	st *joinState) (LinkResult, JoinReport, bool, error) {
	save := func() error { return saveJoinState(j.o.Home, st) }
	for begins := 0; ; {
		if st.Challenge == nil {
			if begins == maxBegins {
				return LinkResult{}, blocked(j.o, "challenge_invalid", "the challenge kept expiring; rerun "+
					"with the same code"), false, nil
			}
			begins++
			if st.BeginKey == "" {
				st.BeginKey = newKey("join-begin")
				if err := save(); err != nil {
					return LinkResult{}, JoinReport{}, false, err
				}
			}
			var ch Challenge
			err := j.retry(ctx, func() (err error) { ch, err = crew.BeginInvitation(ctx, st.BeginKey, code); return })
			if codeOf(err) == "challenge_invalid" {
				st.BeginKey = "" // the recorded challenge is gone: a new key begins again
				continue
			}
			if err != nil {
				return LinkResult{}, j.stop(err), false, nil
			}
			st.Challenge = &savedChallenge{ID: ch.ID, HubID: ch.HubID, ServiceID: ch.ServiceID, ExpiresAt: ch.ExpiresAt}
			st.CompleteKey = newKey("join-complete")
			if err := save(); err != nil {
				return LinkResult{}, JoinReport{}, false, err
			}
		}
		res, err := j.complete(ctx, crew, am, code, st)
		if codeOf(err) == "challenge_invalid" {
			st.BeginKey, st.Challenge, st.CompleteKey = "", nil, ""
			if err := save(); err != nil {
				return LinkResult{}, JoinReport{}, false, err
			}
			continue
		}
		if err != nil {
			return LinkResult{}, j.stop(err), false, nil
		}
		return res, JoinReport{}, true, nil
	}
}

// complete proves the aimem identity for the challenge and completes with
// the saved key. Every try gets a new receipt: a receipt is single use, and
// a completion the server recorded is answered without one being redeemed.
func (j *joiner) complete(ctx context.Context, crew InvitationAPI, am JoinAimem, code string,
	st *joinState) (LinkResult, error) {
	ch := st.Challenge
	for proofs, tries := 0, 0; ; {
		receipt, err := am.Proof(ctx, ch.ServiceID, ch.HubID, ch.ID)
		if err != nil {
			return LinkResult{}, &proofError{err}
		}
		res, err := crew.CompleteInvitation(ctx, st.CompleteKey, code, ch.ID, receipt)
		switch {
		case err == nil:
			return res, nil
		case codeOf(err) == "proof_invalid" && proofs < maxProofs:
			proofs++
		case Retryable(err) && tries < maxRetries:
			if err := j.deps.Sleep(ctx, joinBackoff(tries, err)); err != nil {
				return LinkResult{}, err
			}
			tries++
		default:
			return LinkResult{}, err
		}
	}
}

// retry repeats call with the same arguments while its failure is
// retryable.
func (j *joiner) retry(ctx context.Context, call func() error) error {
	for tries := 0; ; tries++ {
		err := call()
		if err == nil || !Retryable(err) || tries == maxRetries {
			return err
		}
		if err := j.deps.Sleep(ctx, joinBackoff(tries, err)); err != nil {
			return err
		}
	}
}

func joinBackoff(tries int, err error) time.Duration {
	d := min(joinRetryBase<<min(tries, 4), joinRetryCap)
	var r *Refusal
	if errors.As(err, &r) && r.RetryAfter > d {
		d = r.RetryAfter
	}
	return d
}

// proofError is aimem's failure to prove the identity.
type proofError struct{ err error }

func (e *proofError) Error() string { return e.err.Error() }

// stop turns a failed redemption into its blocked report. The join state
// is kept, so a rerun with the same code resumes with the same keys.
func (j *joiner) stop(err error) JoinReport {
	var pe *proofError
	var te *TransportError
	var r *Refusal
	switch {
	case errors.As(err, &pe) && strings.Contains(pe.Error(), "no individual credential"):
		// An aimem that cannot report its credential names it here.
		return blocked(j.o, "credential_missing", credentialInstruction(j.o.Home, j.o.AimemHub))
	case errors.As(err, &pe):
		return blocked(j.o, "proof_failed", "aimem could not prove the identity: "+pe.Error()+
			"; fix it and rerun with the same code")
	case errors.As(err, &te):
		return blocked(j.o, "aicrew_unreachable", te.Error()+"; rerun with the same code: it resumes where "+
			"this run stopped")
	case errors.As(err, &r) && stops[r.Code] != "":
		return blocked(j.o, r.Code, stops[r.Code])
	case errors.As(err, &r):
		return blocked(j.o, r.Code, r.Message+" "+r.NextAction)
	}
	return blocked(j.o, "failed", err.Error())
}

// prepare creates the layout and applies the home files, showing the plan
// first, and reports the result.
func (j *joiner) prepare(ctx context.Context, redeemed bool) (JoinReport, error) {
	o := j.o
	if err := makeLayout(o.Home); err != nil {
		return JoinReport{}, err
	}
	rec := j.doc.managed()
	plan, err := planFiles(o.Home, rec)
	if err != nil {
		return JoinReport{}, err
	}
	fmt.Fprintln(j.deps.Out, "Planned changes:")
	for _, c := range plan {
		fmt.Fprintf(j.deps.Out, "  %-9s %s\n", c.Action, c.Path)
	}
	replaced, err := applyFiles(o.Home, plan, rec)
	if err != nil {
		return JoinReport{}, err
	}
	j.doc.set(j.doc.top, "layout", LayoutVersion)
	j.doc.set(j.doc.top, "label", o.Label)
	j.doc.set(j.doc.top, "managed", rec)
	if err := j.doc.write(o.Home); err != nil {
		return JoinReport{}, err
	}
	rep := JoinReport{Status: JoinReady, Home: o.Home, AgentID: str(j.doc.aicrew, "agent_id"),
		TeamID: str(j.doc.aicrew, "team_id"), Redeemed: redeemed, Changes: plan,
		Next: fmt.Sprintf("aicrew-agent session start --home %s", quoteArg(o.Home))}
	if replaced {
		rep.Status = JoinRestartRequired
		rep.Instruction = "managed guidance changed: restart any client already open in this home"
	}
	for _, c := range plan {
		if c.Action == "conflict" {
			rep.Instruction = strings.TrimPrefix(rep.Instruction+"; ", "; ") + "a managed file was edited " +
				"locally, so its new version was written beside it as <file>.aicrew-new: merge it by hand"
			break
		}
	}
	// The home is ready only when its dependencies and clients are (K7).
	fmt.Fprintln(j.deps.Out, "Client wiring:")
	crep, err := j.deps.check(ctx, CheckOptions{Home: o.Home, Clients: o.Clients, Out: j.deps.Out}, &j.doc, redeemed)
	if err != nil {
		return JoinReport{}, err
	}
	if err := j.doc.write(o.Home); err != nil {
		return JoinReport{}, err
	}
	rep.Check = &crep
	switch {
	case crep.Status == JoinBlocked:
		rep.Status, rep.Reason = JoinBlocked, crep.Reason
	case crep.Status == JoinRestartRequired && rep.Status == JoinReady:
		rep.Status = JoinRestartRequired
	}
	return rep, nil
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"'") {
		return `"` + s + `"`
	}
	return s
}

// joinState is the nonsecret recovery record of a redemption in progress,
// state/aicrew-join.json. It holds the idempotency keys and the challenge,
// and a digest that tells a rerun with the same code; never the code or a
// receipt.
type joinState struct {
	Version     int             `json:"version"`
	CodeDigest  string          `json:"code_digest"`
	URL         string          `json:"url"`
	BeginKey    string          `json:"begin_key,omitempty"`
	Challenge   *savedChallenge `json:"challenge,omitempty"`
	CompleteKey string          `json:"complete_key,omitempty"`
}

type savedChallenge struct {
	ID        string    `json:"id"`
	HubID     string    `json:"hub_id"`
	ServiceID string    `json:"service_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func joinStatePath(home string) string { return filepath.Join(home, "state", "aicrew-join.json") }

// joinCodeDigest is tagged apart from aicrewd's own code digest.
func joinCodeDigest(norm string) string {
	sum := sha256.Sum256([]byte("aicrew-agent-join-v1:" + norm))
	return hex.EncodeToString(sum[:])
}

// loadJoinState returns the record for this code and server, or a new one
// when there is none or it belongs to another invitation.
func loadJoinState(home, norm, crewURL string) (*joinState, error) {
	fresh := &joinState{Version: 1, CodeDigest: joinCodeDigest(norm), URL: crewURL}
	raw, err := os.ReadFile(joinStatePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return fresh, nil
	}
	if err != nil {
		return nil, err
	}
	var st joinState
	if json.Unmarshal(raw, &st) != nil || st.Version != 1 || st.CodeDigest != fresh.CodeDigest || st.URL != crewURL {
		return fresh, nil
	}
	return &st, nil
}

func saveJoinState(home string, st *joinState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(joinStatePath(home), append(raw, '\n'))
}

func clearJoinState(home string) error {
	err := os.Remove(joinStatePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
