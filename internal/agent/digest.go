package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The instruction digest of an offer's process pin, from the home's own
// clone of the process repository (01a1153d-6006). The clone is made and
// fetched as `aicrew-agent clone` makes an attempt's: under
// repos/<service>/<owner>/<name>, with the per-clone credential helper, which
// answers with the home's credential for the host when it holds one, and with
// nothing otherwise, so a public process repository needs none. The worker
// never handles a token to verify an offer.

// DigestOptions is one digest of a process pin.
type DigestOptions struct {
	Home       string
	Repository string // the pin's repository, an https clone URL
	Commit     string // the pin's full commit
	Manifest   string // the manifest's path in the repository
	Self       string // the aicrew-agent the credential helper runs; empty means this one
	Out        io.Writer
	Git        GitRunner
}

// DigestReport is a pin's digest and where it was read. It never carries a
// secret.
type DigestReport struct {
	Digest   string `json:"instruction_digest"`
	Clone    string `json:"clone"`
	Commit   string `json:"commit"`
	Manifest string `json:"manifest"`
	Cloned   bool   `json:"cloned"` // this run made the clone
}

// Digest fetches the pinned process repository into the home and returns
// "sha256:" and the lowercase hex SHA-256 of the manifest's exact bytes at
// the pinned commit: the instruction digest as ROLES.md defines it.
func Digest(ctx context.Context, o DigestOptions) (DigestReport, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Git == nil {
		o.Git = execGit
	}
	if abs, err := filepath.Abs(o.Home); err == nil {
		o.Home = abs
	}
	if strings.HasPrefix(o.Repository, "ssh://") || strings.HasPrefix(o.Repository, "git@") {
		return DigestReport{}, errors.New("the pin names an ssh repository, which this home cannot fetch: " +
			"its credential is an https token; decline the offer")
	}
	_, _, clone, err := homeClone(o.Home, o.Repository)
	if err != nil {
		return DigestReport{}, err
	}
	if !baseShape.MatchString(o.Commit) {
		return DigestReport{}, errors.New("--commit must be a full commit ID: 40 lowercase hex characters")
	}
	if !manifestPath(o.Manifest) {
		return DigestReport{}, errors.New("--manifest must be a clean relative path inside the repository")
	}
	if _, found, err := readAgentDoc(o.Home); err != nil {
		return DigestReport{}, err
	} else if !found {
		return DigestReport{}, errors.New("this is not an agent home: it has no agent.json; join it first")
	}
	self := o.Self
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return DigestReport{}, fmt.Errorf("find this aicrew-agent for the credential helper: %w", err)
		}
	}
	helper := credentialHelper(self, o.Home)
	rep := DigestReport{Clone: clone, Commit: o.Commit, Manifest: o.Manifest}
	if rep.Cloned, err = ensureClone(ctx, o.Git, o.Home, o.Repository, clone, helper, o.Out); err != nil {
		return rep, err
	}
	if err := gitConfig(ctx, o.Git, clone, helperSteps(helper)); err != nil {
		return rep, err
	}
	if _, err := o.Git(ctx, clone, gitEnv, "fetch", "--quiet", "origin"); err != nil {
		return rep, fmt.Errorf("fetch %s: %w", o.Repository, err)
	}
	if !hasCommit(ctx, o.Git, clone, o.Commit) {
		// A pinned commit on no branch: ask for it by name, which forges
		// allow for a commit they hold.
		o.Git(ctx, clone, gitEnv, "fetch", "--quiet", "origin", o.Commit)
		if !hasCommit(ctx, o.Git, clone, o.Commit) {
			return rep, fmt.Errorf("the pinned commit %s is not in %s", o.Commit, o.Repository)
		}
	}
	raw, err := o.Git(ctx, clone, gitEnv, "cat-file", "blob", o.Commit+":"+o.Manifest)
	if err != nil {
		return rep, fmt.Errorf("the manifest %s is not in %s at %s", o.Manifest, o.Repository, o.Commit)
	}
	sum := sha256.Sum256([]byte(raw))
	rep.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return rep, nil
}

func hasCommit(ctx context.Context, git GitRunner, clone, commit string) bool {
	_, err := git(ctx, clone, gitEnv, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// manifestPath reports whether m is a pin's manifest path, as the offer
// route takes it: a clean relative slash path of at most 256 bytes that
// stays inside the repository.
func manifestPath(m string) bool {
	if m == "" || len(m) > 256 || strings.HasPrefix(m, "/") || strings.Contains(m, "\\") ||
		strings.ContainsAny(m, " \t\r\n") {
		return false
	}
	return path.Clean(m) == m && m != "." && m != ".." && !strings.HasPrefix(m, "../")
}
