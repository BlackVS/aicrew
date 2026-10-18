package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/BlackVS/aicrew/defaults"
	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/version"
)

// The automations an attempt runs, and where each comes from
// (docs/DESIGN-TURNS.md, 9.2, 9.4 and 13.1). Three sources:
//   - the release's defaults, embedded under defaults/automations/;
//   - the process manifest at the attempt's pin, with a script beside the
//     manifest under the same pin, read from the home's clone of the
//     process repository as Digest reads the manifest;
//   - the repository's automations/ at the base branch's merged commit,
//     read from the home's clone of the attempt's repository after a fetch
//     of the base branch, never from the attempt's head.
//
// The process's entry of a name wins over the release default (13.1). The
// launcher keeps the resolved set and reloads it, without a restart, when
// the pin changes or when the base branch's commit changes automations/.

// AutomationSources names where a set of automations is read from. A nil
// Process reads the release defaults alone.
type AutomationSources struct {
	Process    *ProcessPin
	Repository string // the attempt's repository, an https clone URL
	BaseBranch string // the branch the attempt's pull request targets
}

// AutomationEntry is one automation of the effective set: its entry, and
// the commit its script was read at: the pin's commit, the base branch's
// merged commit, or "release " and the release's version. Problem is why its
// script cannot run (missing, too large, or a header this release refuses);
// such an entry stays in the set, and fails when it is run.
type AutomationEntry struct {
	automation.Entry
	Commit  string `json:"commit,omitempty"`
	Problem string `json:"problem,omitempty"`
	// Code is the script's source; Digest its SHA-256, short.
	Code   []byte `json:"-"`
	Digest string `json:"digest,omitempty"`
}

// AutomationSet is an effective set and what it was read from.
type AutomationSet struct {
	Process *ProcessPin `json:"process,omitempty"`
	// Base and BaseCommit are the base branch and the merged commit the
	// repository's scripts were read at; empty when no entry reads one.
	Base       string            `json:"base,omitempty"`
	BaseCommit string            `json:"base_commit,omitempty"`
	Entries    []AutomationEntry `json:"entries"`
	// key is what the set was read from, down to the tree of the base's
	// automations/: an equal key reads an equal set.
	key string
}

// automationCache holds the last set read for each source.
type automationCache struct {
	mu   sync.Mutex
	sets map[string]*AutomationSet
}

// releaseAutomations is the release's defaults, read once.
var releaseAutomations = sync.OnceValues(func() ([]automation.Entry, error) {
	raw, err := fs.ReadFile(defaults.Automations, defaults.AutomationsFile)
	if err != nil {
		return nil, err
	}
	return automation.ParseDefaults(raw)
})

// releaseFS is where the release's scripts are read; tests replace it.
var releaseFS fs.FS = defaults.Automations

// automationGit runs the reads of the process and repository sources, and
// automationSelf is the aicrew-agent their clones' credential helper names;
// nil and empty mean execGit and this executable. Tests replace them.
var (
	automationGit  GitRunner
	automationSelf string
)

// Automations is the effective set for src. It is read again only when what
// it is read from changed: another pin, or a base commit whose
// automations/ differs from the one the kept set was read at. A manifest the
// release cannot read is an error; a script that cannot be read is its
// entry's problem.
func (s *StepServer) Automations(ctx context.Context, src AutomationSources) (*AutomationSet, error) {
	r := automationReader{home: s.home, git: automationGit, self: automationSelf}
	if r.git == nil {
		r.git = execGit
	}
	release, err := releaseAutomations()
	if err != nil {
		return nil, fmt.Errorf("the release's automations: %w", err)
	}
	var process []automation.Entry
	var pinClone string
	if src.Process != nil {
		if pinClone, process, err = r.manifest(ctx, *src.Process); err != nil {
			return nil, err
		}
	}
	set := &AutomationSet{Process: src.Process}
	key := "release " + version.Get().Version
	if src.Process != nil {
		key += "\x00" + src.Process.Repository + "\x00" + src.Process.Commit + "\x00" + src.Process.Manifest
	}
	var repoClone, tree string
	if needsBase(process) {
		if repoClone, set.BaseCommit, tree, err = r.base(ctx, src.Repository, src.BaseBranch); err != nil {
			return nil, err
		}
		set.Base = src.BaseBranch
		key += "\x00" + src.Repository + "\x00" + src.BaseBranch + "\x00" + tree
	}
	set.key = key
	slot := sourceSlot(src)
	s.autos.mu.Lock()
	defer s.autos.mu.Unlock()
	if kept := s.autos.sets[slot]; kept != nil && kept.key == key {
		return kept, nil
	}
	for _, e := range automation.Resolve(release, process) {
		ae := AutomationEntry{Entry: e}
		switch e.Source {
		case automation.SourceRelease:
			ae.Commit = "release " + version.Get().Version
			ae.Code, err = fs.ReadFile(releaseFS, path.Join("automations", e.Script))
		case automation.SourceProcess:
			ae.Commit = src.Process.Commit
			if dir := path.Dir(src.Process.Manifest); dir != "." && !strings.HasPrefix(e.Script, dir+"/") {
				err = fmt.Errorf("the process script %s is not beside the manifest, in %s/", e.Script, dir)
			} else {
				ae.Code, err = r.blob(ctx, pinClone, src.Process.Commit, e.Script)
			}
		case automation.SourceRepository:
			ae.Commit = set.BaseCommit
			ae.Code, err = r.blob(ctx, repoClone, set.BaseCommit, e.Script)
		}
		if e.Source != automation.SourceDisabled {
			if err == nil && len(ae.Code) > automation.MaxSource {
				err = fmt.Errorf("the script is %d bytes, more than %d", len(ae.Code), automation.MaxSource)
			}
			if err == nil {
				_, err = automation.ParseHeader(ae.Code)
			}
			if err != nil {
				ae.Code, ae.Problem = nil, err.Error()
			} else {
				sum := sha256.Sum256(ae.Code)
				ae.Digest = hex.EncodeToString(sum[:8])
			}
		}
		set.Entries = append(set.Entries, ae)
	}
	if s.autos.sets == nil {
		s.autos.sets = map[string]*AutomationSet{}
	}
	s.autos.sets[slot] = set
	return set, nil
}

// sourceSlot names the sources a kept set belongs to: the attempt's
// repository and base, and whether a process applies. A new pin replaces
// the set kept for the old one.
func sourceSlot(src AutomationSources) string {
	return strconv.FormatBool(src.Process != nil) + "\x00" + src.Repository + "\x00" + src.BaseBranch
}

func needsBase(process []automation.Entry) bool {
	for _, e := range process {
		if e.Source == automation.SourceRepository {
			return true
		}
	}
	return false
}

// AttemptAutomations is the effective set of the member's open attempt, or
// the release defaults alone when it has none, or none that recorded its
// pin and repository.
func (s *StepServer) AttemptAutomations(ctx context.Context) (string, *AutomationSet, error) {
	st, err := s.Status(ctx)
	if err != nil {
		return "", nil, err
	}
	var src AutomationSources
	id := ""
	if a := st.Attempt; a != nil && a.Process != nil && a.Repository != nil {
		id = a.ID
		src = AutomationSources{Process: a.Process, Repository: a.Repository.URL, BaseBranch: a.Repository.DefaultBranch}
	}
	set, err := s.Automations(ctx, src)
	return id, set, err
}

// automationReader reads the process and repository sources from the
// home's clones.
type automationReader struct {
	home string
	git  GitRunner
	self string
}

// clone makes or reuses the home's clone of repository, with the per-clone
// credential helper, as Digest and clone do.
func (r automationReader) clone(ctx context.Context, repository string) (string, error) {
	if strings.HasPrefix(repository, "ssh://") || strings.HasPrefix(repository, "git@") {
		return "", errors.New("an ssh repository, which this home cannot fetch: its credential is an https token")
	}
	_, _, clone, err := homeClone(r.home, repository)
	if err != nil {
		return "", err
	}
	self := r.self
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return "", fmt.Errorf("find this aicrew-agent for the credential helper: %w", err)
		}
	}
	helper := credentialHelper(self, r.home)
	if _, err := ensureClone(ctx, r.git, r.home, repository, clone, helper, io.Discard); err != nil {
		return "", err
	}
	if err := gitConfig(ctx, r.git, clone, helperSteps(helper)); err != nil {
		return "", err
	}
	return clone, nil
}

// manifest reads the automations entries of the manifest at the pin. A
// pinned commit never changes, so it is fetched only when the clone lacks
// it.
func (r automationReader) manifest(ctx context.Context, pin ProcessPin) (string, []automation.Entry, error) {
	if !baseShape.MatchString(pin.Commit) || !manifestPath(pin.Manifest) {
		return "", nil, fmt.Errorf("the process pin %s@%s:%s is not a full commit and a clean manifest path", pin.Repository, pin.Commit, pin.Manifest)
	}
	clone, err := r.clone(ctx, pin.Repository)
	if err != nil {
		return "", nil, fmt.Errorf("the process repository: %w", err)
	}
	if !hasCommit(ctx, r.git, clone, pin.Commit) {
		r.git(ctx, clone, gitEnv, "fetch", "--quiet", "origin")
		if !hasCommit(ctx, r.git, clone, pin.Commit) {
			r.git(ctx, clone, gitEnv, "fetch", "--quiet", "origin", pin.Commit)
		}
		if !hasCommit(ctx, r.git, clone, pin.Commit) {
			return "", nil, fmt.Errorf("the pinned commit %s is not in %s", pin.Commit, pin.Repository)
		}
	}
	raw, err := r.blob(ctx, clone, pin.Commit, pin.Manifest)
	if err != nil {
		return "", nil, fmt.Errorf("the manifest: %w", err)
	}
	es, err := automation.ParseManifest(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%s at %s: %w", pin.Manifest, pin.Commit, err)
	}
	return clone, es, nil
}

// base fetches the base branch and returns its merged commit and the tree
// of its automations/ (empty when it has none).
func (r automationReader) base(ctx context.Context, repository, branch string) (clone, commit, tree string, err error) {
	if !branchShape.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") ||
		strings.HasSuffix(branch, "/") {
		return "", "", "", fmt.Errorf("the base branch %q is not a branch name", branch)
	}
	if clone, err = r.clone(ctx, repository); err != nil {
		return "", "", "", fmt.Errorf("the attempt's repository: %w", err)
	}
	ref := "refs/remotes/origin/" + branch
	if _, err := r.git(ctx, clone, gitEnv, "fetch", "--quiet", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return "", "", "", fmt.Errorf("fetch the base branch %s of %s: %w", branch, repository, err)
	}
	out, err := r.git(ctx, clone, gitEnv, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if commit = strings.TrimSpace(out); err != nil || !baseShape.MatchString(commit) {
		return "", "", "", fmt.Errorf("the base branch %s of %s has no commit", branch, repository)
	}
	if out, err := r.git(ctx, clone, gitEnv, "rev-parse", "--verify", "--quiet", commit+":automations"); err == nil {
		tree = strings.TrimSpace(out)
	}
	return clone, commit, tree, nil
}

// blob is the file at p in commit, bounded: a larger one is refused before
// it is read.
func (r automationReader) blob(ctx context.Context, clone, commit, p string) ([]byte, error) {
	obj := commit + ":" + p
	size, err := r.git(ctx, clone, gitEnv, "cat-file", "-s", obj)
	if err != nil {
		return nil, fmt.Errorf("%s is not in the commit %s", p, commit)
	}
	if n, err := strconv.Atoi(strings.TrimSpace(size)); err != nil || n > automation.MaxManifest {
		return nil, fmt.Errorf("%s at %s is %s bytes, more than %d", p, commit, strings.TrimSpace(size), automation.MaxManifest)
	}
	raw, err := r.git(ctx, clone, gitEnv, "cat-file", "blob", obj)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", p, commit, err)
	}
	return []byte(raw), nil
}
