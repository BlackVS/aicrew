package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// manifestBytes are a process manifest's exact bytes, with a CRLF and no
// final newline, so a digest of anything but the raw bytes differs.
const manifestBytes = "process: delivery\r\nskills:\n  - oh-code-review"

// processForge is a bare process repository holding the manifest on main, and
// a pinned commit on no branch (refs/pins/old), which a plain fetch does not
// bring. The runner maps the github.com URL, which the home holds a
// credential for, and a gitlab URL, which it does not, to it.
func processForge(t *testing.T) (home, onMain, offBranch string, runner GitRunner, argv *[][]string) {
	t.Helper()
	home, _, _, _, _ = cloneHome(t)
	dir := t.TempDir()
	src, bare := filepath.Join(dir, "src"), filepath.Join(dir, "process.git")
	os.MkdirAll(filepath.Join(src, "processes"), 0o755)
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "config", "core.autocrlf", "false")
	os.WriteFile(filepath.Join(src, "processes", "delivery.yaml"), []byte("old\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "old")
	offBranch = git(t, src, "rev-parse", "HEAD")
	git(t, src, "update-ref", "refs/pins/old", offBranch)
	git(t, src, "checkout", "-q", "--orphan", "fresh")
	os.WriteFile(filepath.Join(src, "processes", "delivery.yaml"), []byte(manifestBytes), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "fresh")
	git(t, src, "branch", "-q", "-D", "main")
	git(t, src, "branch", "-q", "-m", "main")
	onMain = git(t, src, "rev-parse", "HEAD")
	git(t, dir, "clone", "-q", "--bare", src, bare)
	git(t, bare, "fetch", "-q", src, "refs/pins/old:refs/pins/old")
	var calls [][]string
	runner = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		calls = append(calls, append([]string{}, args...))
		u := filepath.ToSlash(bare)
		gone := filepath.ToSlash(filepath.Join(dir, "gone.git"))
		env = append(env, "GIT_CONFIG_COUNT=4",
			"GIT_CONFIG_KEY_0=url."+u+".insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/team/process",
			"GIT_CONFIG_KEY_1=url."+u+".insteadOf", "GIT_CONFIG_VALUE_1=https://gitlab.example.org/team/process",
			"GIT_CONFIG_KEY_2=protocol.version", "GIT_CONFIG_VALUE_2=2",
			// A repository the forge refuses, without leaving the machine.
			"GIT_CONFIG_KEY_3=url."+gone+".insteadOf", "GIT_CONFIG_VALUE_3=https://github.com/team/elsewhere")
		return execGit(ctx, dir, env, args...)
	}
	return home, onMain, offBranch, runner, &calls
}

func wantDigest(b string) string {
	sum := sha256.Sum256([]byte(b))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Digest fetches the process repository into the home and prints the
// manifest's digest; the clone's helpers are reset, then ours; a rerun
// reuses the clone; a pinned commit on no branch is fetched by name; the
// token is in no argument, file or output.
func TestDigest(t *testing.T) {
	home, onMain, offBranch, runner, argv := processForge(t)
	var out bytes.Buffer
	rep, err := Digest(context.Background(), DigestOptions{Home: home, Repository: "https://github.com/team/process",
		Commit: onMain, Manifest: "processes/delivery.yaml", Self: "/opt/aicrew/aicrew-agent", Out: &out, Git: runner})
	if err != nil {
		t.Fatalf("digest: %v\n%s", err, out.String())
	}
	if rep.Digest != wantDigest(manifestBytes) || !rep.Cloned ||
		rep.Clone != filepath.Join(home, "repos", "github-com", "team", "process") {
		t.Fatalf("report %+v, want the digest %s", rep, wantDigest(manifestBytes))
	}
	raw, err := exec.Command("git", "-C", rep.Clone, "config", "--local", "--get-all", "credential.helper").Output()
	helpers := strings.TrimSuffix(string(raw), "\n")
	if lines := strings.Split(helpers, "\n"); err != nil || len(lines) != 2 || lines[0] != "" ||
		!strings.Contains(lines[1], "git-credential --home") {
		t.Fatalf("the helpers are not reset, then ours: %q", helpers)
	}

	rep2, err := Digest(context.Background(), DigestOptions{Home: home, Repository: "https://github.com/team/process",
		Commit: offBranch, Manifest: "processes/delivery.yaml", Self: "/opt/aicrew/aicrew-agent", Git: runner})
	if err != nil || rep2.Cloned || rep2.Digest != wantDigest("old\n") {
		t.Fatalf("a pinned commit on no branch, in the same clone: %+v %v", rep2, err)
	}

	for _, a := range *argv {
		if strings.Contains(strings.Join(a, " "), cloneToken) {
			t.Fatalf("a git argument carries the token: %v", a)
		}
	}
	if strings.Contains(out.String(), cloneToken) {
		t.Fatal("the output carries the token")
	}
	filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(p, filepath.Join(home, "creds")) {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(cloneToken)) {
			t.Errorf("%s holds the token", p)
		}
		return nil
	})
}

// A host the home holds no credential for is fetched without one, so a
// public process repository needs none.
func TestDigestWithoutACredential(t *testing.T) {
	home, onMain, _, runner, _ := processForge(t)
	rep, err := Digest(context.Background(), DigestOptions{Home: home, Repository: "https://gitlab.example.org/team/process",
		Commit: onMain, Manifest: "processes/delivery.yaml", Self: "x", Git: runner})
	if err != nil || rep.Digest != wantDigest(manifestBytes) ||
		rep.Clone != filepath.Join(home, "repos", "gitlab-example-org", "team", "process") {
		t.Fatalf("%+v %v", rep, err)
	}
}

// Digest refuses an ssh pin, a bad commit or manifest and a home that is
// none before touching repos/, and stops with no digest on a refused fetch,
// a commit the repository lacks and a manifest it lacks.
func TestDigestRefusals(t *testing.T) {
	home, onMain, _, runner, _ := processForge(t)
	ok := DigestOptions{Home: home, Repository: "https://github.com/team/process", Commit: onMain,
		Manifest: "processes/delivery.yaml", Self: "x", Git: runner}
	for name, c := range map[string]struct {
		mod  func(*DigestOptions)
		want string
	}{
		"ssh pin":         {func(o *DigestOptions) { o.Repository = "git@github.com:team/process.git" }, "decline the offer"},
		"ssh URL":         {func(o *DigestOptions) { o.Repository = "ssh://git@github.com/team/process.git" }, "decline the offer"},
		"http":            {func(o *DigestOptions) { o.Repository = "http://github.com/team/process" }, "https"},
		"short commit":    {func(o *DigestOptions) { o.Commit = "abc123" }, "full commit"},
		"absolute":        {func(o *DigestOptions) { o.Manifest = "/etc/passwd" }, "relative path"},
		"escapes":         {func(o *DigestOptions) { o.Manifest = "../x.yaml" }, "relative path"},
		"unclean":         {func(o *DigestOptions) { o.Manifest = "processes/./delivery.yaml" }, "relative path"},
		"not a home":      {func(o *DigestOptions) { o.Home = t.TempDir() }, "agent.json"},
		"dot-dot segment": {func(o *DigestOptions) { o.Repository = "https://github.com/team/.hidden" }, "segment"},
	} {
		o := ok
		c.mod(&o)
		if rep, err := Digest(context.Background(), o); err == nil || rep.Digest != "" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %+v %v", name, rep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "repos")); err == nil {
		t.Fatal("a refused digest touched repos/")
	}
	for name, c := range map[string]struct {
		mod  func(*DigestOptions)
		want string
	}{
		"refused fetch":    {func(o *DigestOptions) { o.Repository = "https://github.com/team/elsewhere" }, "clone https://github.com/team/elsewhere"},
		"missing commit":   {func(o *DigestOptions) { o.Commit = strings.Repeat("a", 40) }, "is not in"},
		"missing manifest": {func(o *DigestOptions) { o.Manifest = "processes/other.yaml" }, "the manifest processes/other.yaml is not in"},
	} {
		o := ok
		c.mod(&o)
		if rep, err := Digest(context.Background(), o); err == nil || rep.Digest != "" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %+v %v", name, rep, err)
		}
	}
}

// ROLES.md's digest paragraph and accept step, /crew-accept and /crew-offer
// name the command, so a worker never makes its own clone or handles a
// credential to verify an offer.
func TestTheGuidanceNamesTheDigestCommand(t *testing.T) {
	md := rolesMD()
	for _, want := range []string{
		"aicrew-agent digest --repository PROCESS_REPO --commit PROCESS_COMMIT --manifest PROCESS_MANIFEST",
		"Compute the digest yourself with `aicrew-agent digest`",
		"never fetch the\n  repository another way",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("ROLES.md does not say %q", want)
		}
	}
	if strings.Contains(md, "git -C repos/PROCESS show") {
		t.Error("ROLES.md still asks for a clone the worker makes itself")
	}
	for _, c := range crewCommands {
		if (c.Name == "crew-accept" || c.Name == "crew-offer") && !strings.Contains(commandMD(c), "`aicrew-agent digest") {
			t.Errorf("/%s does not name aicrew-agent digest", c.Name)
		}
	}
}
