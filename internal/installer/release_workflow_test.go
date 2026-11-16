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
// The step may carry a YAML anchor (`- &digests` on the line above its
// name).
func stepRun(t *testing.T, job, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^      - (?:&[A-Za-z0-9_-]+\n        )?name: ` + regexp.QuoteMeta(name) + `\n`).FindStringIndex(job)
	if m == nil {
		t.Fatalf("no step %q", name)
	}
	step := job[m[1]:]
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
	ref, build, macos, check, publish := jobs["ref"], jobs["build"], jobs["macos"], jobs["check"], jobs["publish"]
	if ref == "" || build == "" || macos == "" || check == "" || publish == "" {
		t.Fatalf("release.yml lacks one of the jobs ref, build, macos, check, publish: %v", jobs)
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
	// The job that checks the hand-over in every run, a dry run included,
	// holds no permission at all: it can write nothing and sign nothing.
	if got := permsOf(check); got != "{}" {
		t.Errorf("the check job's permissions are %q, not {}", got)
	}
	// macos inherits the workflow's contents: read.
	if got := permsOf(macos); got != "(inherited)" && got != "contents: read" {
		t.Errorf("the macos job's permissions are %q", got)
	}
	for _, p := range []string{"contents: write", "id-token: write", "attestations: write"} {
		if !strings.Contains(permsOf(publish), p) {
			t.Errorf("the publish job lacks %q", p)
		}
		for name, job := range map[string]string{"ref": ref, "build": build, "macos": macos, "check": check} {
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
		for name, job := range map[string]string{"publish": publish, "check": check} {
			if strings.Contains(job, s) {
				t.Errorf("the %s job runs %q: it must check out and run nothing from the repository", name, s)
			}
		}
	}
	// The artifact is handed over in every run, so a dry run uploads it too.
	upload := regexp.MustCompile(`(?m)^      - name: Keep the assets[^\n]*\n((?:        .*\n)*)`).FindStringSubmatch(build)
	if upload == nil || !strings.Contains(upload[1], "uses: actions/upload-artifact@") {
		t.Fatal("the build job has no upload step named \"Keep the assets ...\"")
	}
	if strings.Contains(upload[1], "        if:") {
		t.Errorf("the upload step has a condition; a dry run must hand the files over too:\n%s", upload[1])
	}
	_, body, _ := strings.Cut(wf, "\njobs:\n")
	if first := regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):\n`).FindStringSubmatch(body); first == nil || first[1] != "ref" {
		t.Errorf("the first job is %v, not ref", first)
	}
	if !strings.Contains(build, "    needs: ref\n") {
		t.Error("the build job does not wait for the ref job")
	}
	// needs alone is not enough: an `if: always() && ...` would publish
	// after a failed job it needs. The condition stays exactly this.
	if !strings.Contains(publish, "    needs: [ref, build, macos, check]\n    if: needs.build.outputs.dry_run != 'true'\n") {
		t.Error("the publish job does not wait for the guard, the build, macOS and the check, or its if is not exactly the dry-run condition")
	}
	// The check job runs in every run: no condition of its own.
	if !strings.Contains(check, "    needs: build\n") || regexp.MustCompile(`(?m)^    if:`).MatchString(check) {
		t.Error("the check job does not wait for the build alone, or it has a condition: it must run in a dry run too")
	}
	// The download and the three checks are defined once, in the check
	// job, each under an anchor; publish runs exactly those steps by alias,
	// in the same order, before anything is attested or published. A
	// copied step list in publish (which could drift) fails here.
	anchors := []string{
		"download\n        uses: actions/download-artifact@",
		"digests\n        name: SHA256SUMS and the notes are the ones build produced\n",
		"sums\n        name: Every file matches SHA256SUMS\n",
		"exact\n        name: dist holds exactly the files SHA256SUMS lists\n",
	}
	at := -1
	for _, a := range anchors {
		i := strings.Index(check, "      - &"+a)
		if i <= at {
			t.Fatalf("check: the anchored step %q is missing or out of order", strings.SplitN(a, "\n", 2)[0])
		}
		at = i
	}
	steps := regexp.MustCompile(`(?m)^      - (.*)$`).FindAllStringSubmatch(publish, -1)
	want := []string{"*download", "*digests", "*sums", "*exact", "name: Attest build provenance", "name: Publish"}
	if len(steps) != len(want) {
		t.Fatalf("publish has the steps %v, want %v", steps, want)
	}
	for i, w := range want {
		if steps[i][1] != w {
			t.Errorf("publish step %d is %q, want %q", i+1, steps[i][1], w)
		}
	}
	if strings.Contains(publish, "sha256sum") {
		t.Error("publish carries its own copy of a check; it must run the check job's steps by alias")
	}
	order := []string{"actions/attest-build-provenance@", "gh release create"}
	at = -1
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

// The publish step, run as the workflow has it with a stand-in git and gh:
// a failed or empty read of the forge's tags stops it before gh, so a
// release is never created with "latest" decided from nothing.
func TestReleasePublishStepRefusesAFailedTagRead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the step runs on the ubuntu runner")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	publish := stepRun(t, releaseJobs(t)["publish"], "Publish")
	cases := []struct {
		name, git, tag string
		ok             bool
		want           string // in the output, or in gh's arguments when ok
	}{
		{"the read fails", "exit 128", "v0.11.0", false, "could not read the vX.Y.Z tags from the forge"},
		{"no release tag listed", "echo 'abc\trefs/tags/latest'", "v0.11.0", false, "could not read the vX.Y.Z tags"},
		{"nothing listed", "true", "v0.11.0", false, "could not read the vX.Y.Z tags"},
		{"the newest tag", "printf 'a\trefs/tags/v0.9.0\\nb\trefs/tags/v0.11.0\\nc\trefs/tags/v0.10.0\\n'", "v0.11.0", true, "--latest=true"},
		{"an older tag", "printf 'a\trefs/tags/v0.9.0\\nb\trefs/tags/v0.11.0\\n'", "v0.9.0", true, "--latest=false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			ghArgs := filepath.Join(dir, "gh-args")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"git": "#!/usr/bin/env bash\n" + c.git + "\n",
				"gh":  "#!/usr/bin/env bash\necho \"$*\" > \"" + ghArgs + "\"\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bash, "-e", "-c", publish)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"TAG="+c.tag, "GH_REPO=example/repo", "GH_TOKEN=")
			out, err := cmd.CombinedOutput()
			args, readErr := os.ReadFile(ghArgs)
			if !c.ok {
				if err == nil || !strings.Contains(string(out), c.want) {
					t.Fatalf("want a refusal with %q, got %v\n%s", c.want, err, out)
				}
				if readErr == nil {
					t.Fatalf("gh was reached: %s", args)
				}
				return
			}
			if err != nil || readErr != nil || !strings.Contains(string(args), c.want) {
				t.Fatalf("want gh called with %q, got %v %v\n%s\ngh: %s", c.want, err, readErr, out, args)
			}
		})
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
	// One pull request for the actions' updates, not one per action.
	if !regexp.MustCompile(`(?m)^    groups:\n      actions:\n        patterns: \['\*'\]\n`).MatchString(dep) {
		t.Error(".github/dependabot.yml does not group the actions' updates")
	}
}

// The simulation workflows run on a pull request only when their label
// asks for it. Their job has no job-level if: GitHub does not evaluate the
// name of a job an if skips, so its check run would carry the name's
// expression as text. The job always starts, and REQUESTED, the very
// condition the name uses, gates every step but the one that says it was
// not requested; the check is then "<job> (not requested)", never a
// skipped "<job>" that passes for a real run.
func TestSimulationWorkflowsNameEveryCheck(t *testing.T) {
	space := regexp.MustCompile(`\s+`)
	for file, job := range map[string]string{"pilot-sim.yml": "pilot-sim", "e2e-real-aimem.yml": "real-aimem"} {
		t.Run(job, func(t *testing.T) {
			body := workflowJobs(t, read(t, filepath.Join(".github", "workflows", file)))[job]
			if body == "" {
				t.Fatalf("%s has no job %s", file, job)
			}
			if regexp.MustCompile(`(?m)^    if:`).MatchString(body) {
				t.Error("the job has a job-level if; a skipped job's check run is named by the expression's text")
			}
			name := regexp.MustCompile(`(?s)    name: >-\n      \$\{\{ \((.*?)\)\n      && '` + regexp.QuoteMeta(job) + `' \|\| '` + regexp.QuoteMeta(job) + ` \(not requested\)' \}\}\n`).FindStringSubmatch(body)
			req := regexp.MustCompile(`(?s)      REQUESTED: >-\n        \$\{\{ (.*?) \}\}\n`).FindStringSubmatch(body)
			if name == nil || req == nil {
				t.Fatalf("no name expression or no REQUESTED in the job:\n%s", body)
			}
			if a, b := space.ReplaceAllString(name[1], " "), space.ReplaceAllString(req[1], " "); a != b {
				t.Errorf("REQUESTED is not the name's condition:\n name: %s\n REQUESTED: %s", a, b)
			}
			steps := strings.Split(body, "\n      - ")[1:]
			if len(steps) < 2 {
				t.Fatalf("the job has %d steps", len(steps))
			}
			if !strings.HasPrefix(steps[0], "name: not requested\n        if: env.REQUESTED != 'true'\n") {
				t.Errorf("the first step is not the one that says the run was not requested:\n%s", steps[0])
			}
			for _, s := range steps[1:] {
				if !regexp.MustCompile(`(?m)^        if: (always\(\) && )?env\.REQUESTED == 'true'$`).MatchString(s) {
					t.Errorf("a step runs when the simulation was not requested:\n%s", strings.SplitN(s, "\n", 2)[0])
				}
			}
		})
	}
}

// The three checks, run as release.yml has them against a handed-over
// artifact: a good one passes all three, and each tampering stops at the
// check that names it. They are read from the check job, where they are
// defined; the publish job runs the same steps by alias (pinned above), so
// this covers the dry run and the publishing run alike.
func TestPublishChecksRefuseAnArtifactBuildDidNotProduce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the checks run on the ubuntu runner (GNU coreutils and find)")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	check := releaseJobs(t)["check"]
	digests := stepRun(t, check, "SHA256SUMS and the notes are the ones build produced")
	sums := stepRun(t, check, "Every file matches SHA256SUMS")
	exact := stepRun(t, check, "dist holds exactly the files SHA256SUMS lists")
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
