package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// workflowJobs splits a workflow into its jobs: the text under each
// two-space key below `jobs:`.
func workflowJobs(t *testing.T, wf string) map[string]string {
	t.Helper()
	_, body, ok := strings.Cut(wf, "\njobs:\n")
	if !ok {
		t.Fatal("the workflow has no jobs")
	}
	jobs := map[string]string{}
	key := regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):\n`)
	idx := key.FindAllStringSubmatchIndex(body, -1)
	for i, m := range idx {
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		jobs[body[m[2]:m[3]]] = body[m[0]:end]
	}
	return jobs
}

// stepRun is the `run: |` script of the step named name in job, dedented.
func stepRun(t *testing.T, job, name string) string {
	t.Helper()
	i := strings.Index(job, "      - name: "+name+"\n")
	if i < 0 {
		t.Fatalf("no step %q", name)
	}
	step := job[i+len("      - name: "+name+"\n"):]
	if j := strings.Index(step, "\n      - "); j >= 0 {
		step = step[:j+1]
	}
	_, script, ok := strings.Cut(step, "        run: |\n")
	if !ok {
		t.Fatalf("step %q has no run block", name)
	}
	var out strings.Builder
	for _, l := range strings.SplitAfter(script, "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "          ") {
			break // a comment or key after the block
		}
		out.WriteString(strings.TrimPrefix(l, "          "))
	}
	return out.String()
}

func releaseJobs(t *testing.T) map[string]string {
	t.Helper()
	return workflowJobs(t, read(t, filepath.Join(".github", "workflows", "release.yml")))
}

// The release's tests and build run in jobs that can write nothing and
// cannot sign; the job that holds those permissions checks out nothing,
// so no code from the repository runs beside the write token or the OIDC
// token.
func TestReleaseBuildsWithoutWritePermissions(t *testing.T) {
	wf := read(t, filepath.Join(".github", "workflows", "release.yml"))
	if top := regexp.MustCompile(`(?m)^permissions:\n  contents: read\n(?:\n|\S)`); !top.MatchString(wf) {
		t.Error("the workflow's own permissions are not contents: read alone")
	}
	jobs := releaseJobs(t)
	ref, build, macos, publish := jobs["ref"], jobs["build"], jobs["macos"], jobs["publish"]
	if ref == "" || build == "" || macos == "" || publish == "" {
		t.Fatalf("release.yml lacks one of the jobs ref, build, macos, publish: %v", jobs)
	}
	perms := regexp.MustCompile(`(?m)^    permissions:(.*)\n((?:      .*\n)*)`)
	permsOf := func(job string) string {
		m := perms.FindStringSubmatch(job)
		if m == nil {
			return "(inherited)"
		}
		return strings.TrimSpace(m[1] + m[2])
	}
	if got := permsOf(build); got != "contents: read" {
		t.Errorf("the build job's permissions are %q, not contents: read alone", got)
	}
	if got := permsOf(ref); got != "{}" {
		t.Errorf("the ref job's permissions are %q, not {}", got)
	}
	// macos inherits the workflow's contents: read.
	if got := permsOf(macos); got != "(inherited)" && got != "contents: read" {
		t.Errorf("the macos job's permissions are %q", got)
	}
	for _, p := range []string{"contents: write", "id-token: write", "attestations: write"} {
		if !strings.Contains(permsOf(publish), p) {
			t.Errorf("the publish job lacks %q", p)
		}
		for name, job := range map[string]string{"ref": ref, "build": build, "macos": macos} {
			if strings.Contains(job, strings.SplitN(p, ":", 2)[0]+": write") {
				t.Errorf("the %s job holds %q", name, p)
			}
		}
	}
	for _, s := range []string{`release.sh" resolve`, `release.sh" check`, "go test -count=1 ./...", `release.sh" build`, `release.sh" notes`, "- name: Hand over the digests"} {
		if !strings.Contains(build, s) {
			t.Errorf("the build job does not run %q", s)
		}
	}
	for _, s := range []string{"actions/checkout@", "setup-go", "release.sh", "scripts/", "go test", "go build", "go run"} {
		if strings.Contains(publish, s) {
			t.Errorf("the publish job runs %q: it must check out and run nothing from the repository", s)
		}
	}
	_, body, _ := strings.Cut(wf, "\njobs:\n")
	if first := regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):\n`).FindStringSubmatch(body); first == nil || first[1] != "ref" {
		t.Errorf("the first job is %v, not ref", first)
	}
	if !strings.Contains(build, "    needs: ref\n") {
		t.Error("the build job does not wait for the ref job")
	}
	if !strings.Contains(publish, "    needs: [ref, build, macos]\n") {
		t.Error("the publish job does not wait for the guard, the build and macOS")
	}
	// The checks run in this order, before anything is attested or published.
	order := []string{
		"actions/download-artifact@",
		"- name: SHA256SUMS and the notes are the ones build produced",
		"- name: Every file matches SHA256SUMS",
		"- name: dist holds exactly the files SHA256SUMS lists",
		"actions/attest-build-provenance@",
		"gh release create",
	}
	at := -1
	for _, s := range order {
		i := strings.Index(publish, s)
		if i <= at {
			t.Fatalf("publish: %q is missing or out of order", s)
		}
		at = i
	}
	// Every published file is attested: the attestation and the release
	// name the same files.
	if !strings.Contains(publish, "subject-path: release/dist/*\n") || !strings.Contains(publish, `gh release create "$TAG" release/dist/* `) {
		t.Error("the attestation and the release do not name the same files, release/dist/*")
	}
}

// The guard job's own script, run as the workflow has it: a manual run
// that publishes starts from its tag, and a dry run from main. A guard
// whose condition is dropped fails the publishing run from main below.
func TestReleaseRefGuard(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	ref := releaseJobs(t)["ref"]
	name := "A manual run starts from its tag, or a dry run from main"
	if !strings.Contains(ref, "      - name: "+name+"\n        if: github.event_name == 'workflow_dispatch'\n") {
		t.Fatal("the guard step does not run on every manual run")
	}
	guard := stepRun(t, ref, name)
	cases := []struct {
		ref, tag, dry string
		ok            bool
		want          string
	}{
		{"refs/heads/main", "v1.2.3", "false", false, "Start it from the tag: gh workflow run release.yml --ref v1.2.3 -f tag=v1.2.3 -f dry_run=false"},
		{"refs/heads/side", "v1.2.3", "false", false, "not refs/tags/v1.2.3"},
		{"refs/tags/v1.2.2", "v1.2.3", "false", false, "not refs/tags/v1.2.3"},
		{"refs/tags/v1.2.3", "v1.2.3", "false", true, ""},
		{"refs/tags/v1.2.3", "1.2.3", "false", true, ""},
		{"refs/heads/main", "v1.2.3", "true", true, ""},
		{"refs/heads/side", "v1.2.3", "true", false, "a dry run starts from main"},
		{"refs/tags/v1.2.3", "v1.2.3", "true", false, "a dry run starts from main"},
	}
	for _, c := range cases {
		cmd := exec.Command(bash, "-e", "-c", guard)
		cmd.Env = append(os.Environ(), "GITHUB_REF="+c.ref, "INPUT_TAG="+c.tag, "INPUT_DRY_RUN="+c.dry)
		out, err := cmd.CombinedOutput()
		if c.ok != (err == nil) || !strings.Contains(string(out), c.want) {
			t.Errorf("ref %s, tag %s, dry run %s: want ok=%v with %q, got %v\n%s", c.ref, c.tag, c.dry, c.ok, c.want, err, out)
		}
	}
}

// Every checkout drops the token from its git configuration, and every
// action is named by a full commit SHA with its version in a comment.
func TestWorkflowsPinActionsAndKeepNoCredential(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	uses := regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*(.*)$`)
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\d+\.\d+\.\d+$`)
	keeps := regexp.MustCompile(`(?m)^          persist-credentials: false(\s+#.*)?$`)
	checkouts := 0
	for _, f := range files {
		wf := read(t, filepath.Join(".github", "workflows", filepath.Base(f)))
		for _, m := range uses.FindAllStringSubmatch(wf, -1) {
			if !pinned.MatchString(strings.TrimSpace(m[1])) {
				t.Errorf("%s: %s is not pinned to a full commit SHA with its version in a comment", filepath.Base(f), m[1])
			}
		}
		for _, s := range strings.Split(wf, "\n      - ")[1:] {
			if strings.HasPrefix(s, "uses: actions/checkout@") {
				checkouts++
				if !keeps.MatchString(s) {
					t.Errorf("%s: a checkout keeps the token: %s", filepath.Base(f), strings.SplitN(s, "\n", 2)[0])
				}
			}
		}
	}
	if checkouts == 0 {
		t.Error("no checkout found: the test reads nothing")
	}
	dep := read(t, filepath.Join(".github", "dependabot.yml"))
	if !strings.Contains(dep, "package-ecosystem: github-actions") {
		t.Error(".github/dependabot.yml does not keep the actions' pins")
	}
}

// The publish job's three checks, run as release.yml has them against a
// handed-over artifact: a good one passes all three, and each tampering
// stops at the check that names it.
func TestPublishChecksRefuseAnArtifactBuildDidNotProduce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the checks run on the ubuntu runner (GNU coreutils and find)")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	publish := releaseJobs(t)["publish"]
	digests := stepRun(t, publish, "SHA256SUMS and the notes are the ones build produced")
	sums := stepRun(t, publish, "Every file matches SHA256SUMS")
	exact := stepRun(t, publish, "dist holds exactly the files SHA256SUMS lists")
	hand := stepRun(t, releaseJobs(t)["build"], "Hand over the digests")

	run := func(dir, script string, env ...string) (string, error) {
		cmd := exec.Command(bash, "-e", "-c", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// build: the artifact as the build job lays it out (release.sh's sums
	// line), and its outputs from the build job's own hand-over step.
	build := func(t *testing.T) (root string, env []string) {
		root = t.TempDir()
		dist := filepath.Join(root, "dist")
		if err := os.Mkdir(dist, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"aicrew-agent-linux-amd64": "binary\n", "LICENSE": "license\n", "boot.sh": "#!/bin/sh\n"} {
			if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "release_notes.md"), []byte("notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		outputs := filepath.Join(root, "outputs")
		out, err := run(root, `(cd dist && sha256sum $(ls | grep -vx SHA256SUMS) | sed 's/ \*/  /' > SHA256SUMS)`+"\n"+hand, "GITHUB_OUTPUT="+outputs)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		b, err := os.ReadFile(outputs)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Fields(string(b)) {
			k, v, _ := strings.Cut(l, "=")
			env = append(env, strings.ToUpper(k)+"="+v)
		}
		if len(env) != 2 {
			t.Fatalf("the hand-over wrote %q", b)
		}
		return root, env
	}
	type step struct{ dir, script string }
	checks := func(root string) []step {
		return []step{{root, digests}, {filepath.Join(root, "dist"), sums}, {filepath.Join(root, "dist"), exact}}
	}
	write := func(t *testing.T, p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		tamper func(t *testing.T, root string)
		stop   int    // the check that refuses it; 3 = none
		want   string // its message
	}{
		{"as built", func(*testing.T, string) {}, 3, ""},
		{"notes changed", func(t *testing.T, r string) { write(t, filepath.Join(r, "release_notes.md"), "other\n") },
			0, "not the ones the build job produced"},
		{"sums rewritten over a changed file", func(t *testing.T, r string) {
			write(t, filepath.Join(r, "dist", "boot.sh"), "#!/bin/sh\nevil\n")
			if out, err := run(filepath.Join(r, "dist"), "sha256sum aicrew-agent-linux-amd64 LICENSE boot.sh > SHA256SUMS"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}, 0, "not the ones the build job produced"},
		{"a file changed", func(t *testing.T, r string) { write(t, filepath.Join(r, "dist", "LICENSE"), "changed\n") },
			1, "does not match its line in SHA256SUMS"},
		{"a file the sums do not list", func(t *testing.T, r string) { write(t, filepath.Join(r, "dist", "extra"), "x\n") },
			2, "present but not listed:\nextra"},
		{"a listed file missing", func(t *testing.T, r string) {
			if err := os.Remove(filepath.Join(r, "dist", "boot.sh")); err != nil {
				t.Fatal(err)
			}
		}, 1, "does not match its line in SHA256SUMS"},
		{"a directory the sums do not list", func(t *testing.T, r string) {
			if err := os.Mkdir(filepath.Join(r, "dist", "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, 2, "present but not listed:\nsub"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, env := build(t)
			c.tamper(t, root)
			for i, s := range checks(root) {
				out, err := run(s.dir, s.script, env...)
				if i < c.stop {
					if err != nil {
						t.Fatalf("check %d refused it: %v\n%s", i, err, out)
					}
					continue
				}
				if err == nil || !strings.Contains(out, c.want) {
					t.Fatalf("check %d: want a refusal with %q, got %v\n%s", i, c.want, err, out)
				}
				return
			}
		})
	}
	// The exact-match check alone: a file the sums do not list passes the
	// other two, so this step is what keeps it out of the release.
	t.Run("only the exact-match check stops an unlisted file", func(t *testing.T) {
		root, env := build(t)
		write(t, filepath.Join(root, "dist", "extra"), "x\n")
		for _, s := range checks(root)[:2] {
			if out, err := run(s.dir, s.script, env...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
	})
}
