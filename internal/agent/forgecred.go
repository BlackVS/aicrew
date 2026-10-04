package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

// A member's own forge credentials (docs/proposals/PILOT-1-FOLLOWUPS.md,
// sections 3.1 to 3.4). `join --cred <host>=<file|->` verifies each token
// with the forge's read-only "who am I" and writes it to
// creds/<service>.<account>.<purpose>, WORKSPACE's credential reference with
// the host encoded as its service. agent.json's "forge" section records,
// per host, the dialect, account, purpose, file and commit identity, never a
// value; the file is found through that record, never by parsing its name.

// forgePurpose is every credential's purpose until the team's requirements
// reach the home (354c-1a): the highest access a granted project needs.
const forgePurpose = "repo-write"

// ForgeCred is one --cred: a forge host and where its token is read from, a
// file or "-" for standard input.
type ForgeCred struct {
	Host   string
	Source string
}

// ParseForgeCred reads one --cred value, host=file or host=-. A value that
// names no existing file is refused without being quoted, since it may be a
// token passed by mistake.
func ParseForgeCred(v string) (ForgeCred, error) {
	host, src, ok := strings.Cut(v, "=")
	if !ok || src == "" {
		return ForgeCred{}, errors.New("--cred takes HOST=FILE or HOST=-")
	}
	h, err := forge.NormalizeHost(host)
	if err != nil {
		return ForgeCred{}, fmt.Errorf("--cred: %w", err)
	}
	if src != "-" {
		if _, err := os.Stat(src); err != nil {
			return ForgeCred{}, fmt.Errorf("--cred for %s names no readable file: it takes a file or -, never a token", h)
		}
	}
	return ForgeCred{Host: h, Source: src}, nil
}

// checkForgeCreds refuses two credentials for one host, or two that read
// standard input.
func checkForgeCreds(creds []ForgeCred) error {
	hosts, stdin := map[string]bool{}, 0
	for _, c := range creds {
		if hosts[c.Host] {
			return fmt.Errorf("--cred names %s twice: one credential per host", c.Host)
		}
		hosts[c.Host] = true
		if c.Source == "-" {
			stdin++
		}
	}
	if stdin > 1 {
		return errors.New("only one --cred can read standard input")
	}
	return nil
}

// ForgeAPI is what provisioning and the check ask of a forge; forge.Client
// is the real one.
type ForgeAPI interface {
	Detect(ctx context.Context, host string) (forge.Kind, error)
	WhoAmI(ctx context.Context, host string, k forge.Kind, token string) (forge.Identity, error)
}

// forgeEntry is a home's record of one forge credential, by host.
type forgeEntry struct {
	Kind        string `json:"kind"`
	Account     string `json:"account"`
	Purpose     string `json:"purpose"`
	File        string `json:"file"` // the credential reference, under creds/
	CommitName  string `json:"commit_name"`
	CommitEmail string `json:"commit_email"`
}

// refShape is a credential reference in WORKSPACE's grammar: three parts of
// lowercase letters, digits and '-'. A recorded file of any other shape is
// never opened or removed, so an edited agent.json cannot point outside
// creds/.
var refShape = regexp.MustCompile(`^[a-z0-9-]+\.[a-z0-9-]+\.[a-z0-9-]+$`)

// forgeEntries reads agent.json's "forge" section. An entry whose file is
// not a credential reference keeps its host but loses its file.
func (d agentDoc) forgeEntries() map[string]forgeEntry {
	out := map[string]forgeEntry{}
	if raw, ok := d.top["forge"]; ok {
		_ = json.Unmarshal(raw, &out)
	}
	for h, e := range out {
		if !refShape.MatchString(e.File) {
			e.File = ""
			out[h] = e
		}
	}
	return out
}

// ForgeCredReport is one host's outcome in a join.
type ForgeCredReport struct {
	Host    string `json:"host"`
	State   string `json:"state"` // provisioned, unchanged, unreachable or refused
	Kind    string `json:"kind,omitempty"`
	Account string `json:"account,omitempty"`
	File    string `json:"file,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// The states of a forge credential in join and check reports.
const (
	ForgeProvisioned = "provisioned"
	ForgeUnchanged   = "unchanged"
	ForgeUnreachable = "unreachable"
	ForgeRefused     = "refused"
	ForgeVerified    = "verified"
	ForgeMissing     = "missing"
)

// forgeToken is a read token, kept only in memory.
type forgeToken struct {
	ForgeCred
	token string
}

// readForgeTokens reads every --cred's token before anything changes, so a
// bad file stops the run cleanly. Errors never quote the content.
func readForgeTokens(creds []ForgeCred, readStdin func(host string) (string, error)) ([]forgeToken, error) {
	out := make([]forgeToken, 0, len(creds))
	for _, c := range creds {
		var raw []byte
		if c.Source == "-" {
			if readStdin == nil {
				return nil, fmt.Errorf("--cred %s=-: standard input cannot be read here", c.Host)
			}
			s, err := readStdin(c.Host)
			if err != nil {
				return nil, fmt.Errorf("--cred %s=-: %w", c.Host, err)
			}
			raw = []byte(s)
		} else {
			b, err := readOwnerOnly(c.Source)
			if err != nil {
				return nil, fmt.Errorf("--cred %s: %w", c.Host, err)
			}
			raw = b
		}
		tok, err := tokenLine(raw)
		if err != nil {
			return nil, fmt.Errorf("--cred %s: %w", c.Host, err)
		}
		out = append(out, forgeToken{ForgeCred: c, token: tok})
	}
	return out, nil
}

// provisionForge verifies and writes each credential and records it. A host
// that cannot be reached, or a token the forge refuses, is reported and not
// written; the other hosts proceed.
func provisionForge(ctx context.Context, home string, doc *agentDoc, toks []forgeToken, api ForgeAPI, out io.Writer) ([]ForgeCredReport, error) {
	if len(toks) == 0 {
		return nil, nil
	}
	entries := doc.forgeEntries()
	if err := privatefile.MakeDir(filepath.Join(home, "creds")); err != nil {
		return nil, err
	}
	var reps []ForgeCredReport
	for _, t := range toks {
		r := ForgeCredReport{Host: t.Host}
		kind := forge.Kind(entries[t.Host].Kind)
		if !kind.Valid() {
			k, err := api.Detect(ctx, t.Host)
			if err != nil {
				r.State, r.Detail = ForgeUnreachable, err.Error()+"; rerun join with this --cred when the host answers"
				reps = append(reps, r)
				continue
			}
			kind = k
		}
		r.Kind = string(kind)
		id, err := api.WhoAmI(ctx, t.Host, kind, t.token)
		switch {
		case errors.Is(err, forge.ErrUnreachable):
			r.State, r.Detail = ForgeUnreachable, err.Error()+"; rerun join with this --cred when the host answers"
		case errors.Is(err, forge.ErrRejected):
			r.State, r.Detail = ForgeRefused, "the forge rejected the token: ask the operator for a valid one"
		case err != nil:
			r.State, r.Detail = ForgeRefused, err.Error()
		}
		if err != nil {
			reps = append(reps, r)
			continue
		}
		r.Account = id.Account
		file := forge.Service(t.Host) + "." + forge.EncodePart(id.Account) + "." + forgePurpose
		if other := collidingHost(entries, t.Host, file); other != "" {
			r.State, r.Detail = ForgeRefused, fmt.Sprintf("its file %s would be %s's file: the encoded names collide; "+
				"use another account on one of the two hosts", file, other)
			reps = append(reps, r)
			continue
		}
		path := filepath.Join(home, "creds", file)
		changed, err := writeSecretFile(path, []byte(t.token+"\n"))
		if err != nil {
			return reps, fmt.Errorf("write the credential for %s: %w", t.Host, err)
		}
		prev, had := entries[t.Host]
		next := forgeEntry{Kind: string(kind), Account: id.Account, Purpose: forgePurpose, File: file,
			CommitName: id.Name, CommitEmail: id.CommitEmail}
		if had && prev.File != "" && prev.File != file {
			// Rotation to another account: the new file is confirmed, so the
			// old one goes.
			os.Remove(filepath.Join(home, "creds", prev.File))
		}
		entries[t.Host] = next
		r.File = file
		r.State = ForgeProvisioned
		if !changed && had && prev == next {
			r.State = ForgeUnchanged
		}
		fmt.Fprintf(out, "Forge credential for %s: %s, account %s, file creds/%s.\n", t.Host, r.State, id.Account, file)
		reps = append(reps, r)
	}
	doc.set(doc.top, "forge", entries)
	return reps, nil
}

// collidingHost is the other host whose recorded file is file, if any.
func collidingHost(entries map[string]forgeEntry, host, file string) string {
	hosts := make([]string, 0, len(entries))
	for h := range entries {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		if h != host && entries[h].File == file {
			return h
		}
	}
	return ""
}

// ForgeCheck is one recorded forge credential in a check: never a blocker.
type ForgeCheck struct {
	Host    string `json:"host"`
	Kind    string `json:"kind"`
	Account string `json:"account"`
	Purpose string `json:"purpose"`
	File    string `json:"file"`
	State   string `json:"state"` // verified, missing, refused or unreachable
	Detail  string `json:"detail,omitempty"`
}

// checkForge verifies every recorded credential with "who am I". A missing,
// refused or unreachable one is a notice: it narrows what work the member
// can take, and never blocks the home (only the aimem credential does).
func checkForge(ctx context.Context, home string, doc *agentDoc, api ForgeAPI) []ForgeCheck {
	entries := doc.forgeEntries()
	hosts := make([]string, 0, len(entries))
	for h := range entries {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	var out []ForgeCheck
	for _, h := range hosts {
		e := entries[h]
		fc := ForgeCheck{Host: h, Kind: e.Kind, Account: e.Account, Purpose: e.Purpose, File: e.File}
		raw, err := []byte(nil), errors.New("agent.json records no credential file for it")
		if e.File != "" {
			raw, err = readOwnerOnly(filepath.Join(home, "creds", e.File))
		}
		var tok string
		if err == nil {
			tok, err = tokenLine(raw)
		}
		if err != nil {
			fc.State, fc.Detail = ForgeMissing, fmt.Sprintf("creds/%s: %v; provision it again with "+
				"aicrew-agent join --home %s --cred %s=FILE", e.File, err, quoteArg(home), h)
			out = append(out, fc)
			continue
		}
		id, err := api.WhoAmI(ctx, h, forge.Kind(e.Kind), tok)
		switch {
		case err == nil && id.Account == e.Account:
			fc.State = ForgeVerified
		case err == nil:
			fc.State, fc.Detail = ForgeRefused, fmt.Sprintf("the token now authenticates as %s, not %s: "+
				"rerun join with the right --cred", id.Account, e.Account)
		case errors.Is(err, forge.ErrUnreachable):
			fc.State, fc.Detail = ForgeUnreachable, err.Error()
		case errors.Is(err, forge.ErrRejected):
			fc.State, fc.Detail = ForgeRefused, "the forge rejected the token (revoked or expired): ask the operator for a new one"
		default:
			fc.State, fc.Detail = ForgeRefused, err.Error()
		}
		out = append(out, fc)
	}
	return out
}

// forgeTokenFor reads the home's recorded credential for host, for a caller
// that reads the forge as the member (base resolution, clones).
func forgeTokenFor(home string, doc agentDoc, host string) (forgeEntry, string, error) {
	e, ok := doc.forgeEntries()[host]
	if !ok || e.File == "" {
		return forgeEntry{}, "", fmt.Errorf("this home holds no forge credential for %s: provision one with "+
			"aicrew-agent join --home %s --cred %s=FILE", host, quoteArg(home), host)
	}
	raw, err := readOwnerOnly(filepath.Join(home, "creds", e.File))
	if err != nil {
		return forgeEntry{}, "", fmt.Errorf("creds/%s: %w", e.File, err)
	}
	tok, err := tokenLine(raw)
	if err != nil {
		return forgeEntry{}, "", fmt.Errorf("creds/%s: %w", e.File, err)
	}
	return e, tok, nil
}

// readOwnerOnly reads a small secret file, which must be readable by its
// owner only.
func readOwnerOnly(path string) ([]byte, error) {
	if err := privatefile.Check(path); err != nil {
		return nil, fmt.Errorf("the file must be readable by its owner only: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxToken {
		return nil, errors.New("the file is larger than a token")
	}
	return raw, nil
}

// tokenLine is a token alone on one line: printable, without spaces.
func tokenLine(raw []byte) (string, error) {
	s := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if s == "" {
		return "", errors.New("the token is empty")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", errors.New("the token must be alone on one line")
		}
	}
	return s, nil
}

// writeSecretFile writes data to path atomically and owner-only, and reports
// whether the file changed. An identical file is kept only while it is
// owner-only; one that others can read is replaced, so a rerun repairs it.
func writeSecretFile(path string, data []byte) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) && privatefile.Check(path) == nil {
		return false, nil
	}
	tmp := path + ".new"
	os.Remove(tmp)
	f, err := privatefile.Create(tmp)
	if err != nil {
		return false, err
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// BaseAPI is what base resolution asks of a forge; forge.Client is the real
// one.
type BaseAPI interface {
	DefaultBranch(ctx context.Context, host string, k forge.Kind, token, path string) (string, error)
	BranchHead(ctx context.Context, host string, k forge.Kind, token, path, branch string) (string, error)
}

// BaseResolution is what ResolveBase found: never a secret.
type BaseResolution struct {
	Host          string `json:"host"`
	Repository    string `json:"repository"`
	DefaultBranch string `json:"default_branch"`
	BaseCommit    string `json:"base_commit"`
	Kept          bool   `json:"kept,omitempty"` // the body named its own base
}

// ResolveBase fills an offer's or a claim's base_commit from the forge
// (docs/proposals/PILOT-1-FOLLOWUPS.md, section 3.5): the repository's
// default branch and that branch's head, read with the home's own
// credential for the repository's host. A body that already names a base
// commit keeps it; the branch stays the body's.
func ResolveBase(ctx context.Context, home string, api BaseAPI, cloneURL string, body []byte) ([]byte, BaseResolution, error) {
	host, path, err := forge.Repository(cloneURL)
	if err != nil {
		return nil, BaseResolution{}, err
	}
	res := BaseResolution{Host: host, Repository: path}
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			return nil, res, errors.New("the body is not a JSON object")
		}
	}
	var given string
	if raw, ok := fields["base_commit"]; ok {
		_ = json.Unmarshal(raw, &given)
	}
	if given != "" {
		res.BaseCommit, res.Kept = given, true
		return body, res, nil
	}
	doc, _, err := readAgentDoc(home)
	if err != nil {
		return nil, res, err
	}
	e, tok, err := forgeTokenFor(home, doc, host)
	if err != nil {
		return nil, res, err
	}
	k := forge.Kind(e.Kind)
	if res.DefaultBranch, err = api.DefaultBranch(ctx, host, k, tok, path); err != nil {
		return nil, res, fmt.Errorf("read %s's default branch on %s: %w", path, host, err)
	}
	if res.BaseCommit, err = api.BranchHead(ctx, host, k, tok, path, res.DefaultBranch); err != nil {
		return nil, res, fmt.Errorf("read the head of %s on %s: %w", res.DefaultBranch, host, err)
	}
	fields["base_commit"], _ = json.Marshal(res.BaseCommit)
	out, err := json.Marshal(fields)
	return out, res, err
}
