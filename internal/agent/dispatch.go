package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
)

// The extension points (docs/DESIGN-TURNS.md, 4.2, 9.1 and 13.1; increment
// 2d-a). The launcher dispatches the six points of 9.1 for each pull
// request it watches, from what it observes of it (forgewatch.go) and from
// the work step that submits the attempt, and runs the effective
// automations declared at the point (automationsources.go): a script in
// its own process (automation.go), or a declared command in the attempt's
// worktree under the turn's allow list. Each point fires once for its edge:
// a new head, CI concluded at the head, CI failed at the head, both reviews
// READY and CI green at the head, the submit, the merge. The launcher keeps
// what it fired in PointsFile, so a restart fires nothing twice; a run its
// exit interrupted is not in it and runs again at the next observation.
// Every run is reported to aicrewd as a fact on the attempt, and aicrewd
// wakes the coordinators on a failed run and the worker on a turn
// (store/automations.go).

// PointsFile is the home file, relative to the home, that keeps each
// watched pull request's heads, reviews and the points fired for it. It
// holds no secret.
const PointsFile = "state/points.json"

// automationsPath is aicrewd's route for a run report.
const automationsPath = "/v1/crew/automations"

// commandWall bounds a declared command's run.
const commandWall = automation.MaxWall

// The kinds of the steps the launcher records for a declared command.
const stepCommand = "command"

// pointsDoc is PointsFile.
type pointsDoc struct {
	Pulls map[string]*pullPoints `json:"pulls"`
}

// pullPoints is what the launcher knows of one watched pull request for
// the points: its attempt, its base, the last two heads with their patch
// identities, the newest review of each reviewer, and the edge each point
// last fired for.
type pullPoints struct {
	Attempt string `json:"attempt"`
	Number  int64  `json:"number"`
	URL     string `json:"url"`
	// Base is the branch the pull request targets, as the forge reports
	// it; empty until a poll read it.
	Base string `json:"base,omitempty"`
	// Head is the last head observed, Previous the one before it.
	Head     string `json:"head,omitempty"`
	Previous string `json:"previous,omitempty"`
	// Identity is the patch identity of each of the two heads against its
	// merge base (4.1), "" when it could not be computed.
	Identity map[string]string `json:"identity,omitempty"`
	// Reviews is the newest review of each reviewer (local, external), as
	// the last content read saw them.
	Reviews map[string]pointReview `json:"reviews,omitempty"`
	// State is the pull request's state as last observed. A merged pull
	// request leaves the watch list, and stays here until after-merge ran.
	State string `json:"state,omitempty"`
	// Fired is, per point, the edges it fired for, the newest last: the
	// head, the head with the CI conclusion, or "merged". An edge fires
	// once however the observations flap between it and another; the
	// edges are kept for as long as the pull request is (writePoints).
	Fired map[string][]string `json:"fired,omitempty"`
	// PrePush is the declared check that failed at Head's after-push point,
	// kept until a head where the checks pass and sent with every
	// observation at the head (forgewatch.go), so the worker's need holds
	// across polls and restarts; nil when none failed, or the point has not
	// run at the head.
	PrePush *pointPrePush `json:"pre_push,omitempty"`
	// Rerun is the head whose failed jobs a script's rerun() re-ran
	// (automation.go): once per head, so a second call at the head re-runs
	// nothing.
	Rerun string    `json:"rerun,omitempty"`
	Seen  time.Time `json:"seen"`
}

// pointPrePush is a failed check with the head it failed at.
type pointPrePush struct {
	Head    string         `json:"head"`
	Failure PrePushFailure `json:"failure"`
}

// prePushAt is the failed check kept for the pull request key at head, as
// an observation carries it, or nil.
func (s *StepServer) prePushAt(key, head string) *PrePushFailure {
	s.points.mu.Lock()
	defer s.points.mu.Unlock()
	p := readPoints(s.home).Pulls[key]
	if p == nil || p.PrePush == nil || head == "" || p.PrePush.Head != head {
		return nil
	}
	f := p.PrePush.Failure
	return &f
}

// pendingMerge reports whether p was merged and after-merge has not run.
func (p *pullPoints) pendingMerge() bool {
	return p.State == forge.PullMerged && !slices.Contains(p.Fired[automation.PointAfterMerge], "merged")
}

// observation is p as an observation of the pull request key names, for a
// point fired from the kept state rather than from a forge read.
func (p *pullPoints) observation(key string) PullObservation {
	host, rest, _ := strings.Cut(key, "/")
	repository, _, _ := strings.Cut(rest, "#")
	state := p.State
	if state == "" {
		state = forge.PullOpen
	}
	return PullObservation{Host: host, Repository: repository, Number: p.Number, URL: p.URL, State: state, HeadCommit: p.Head}
}

// pointReview is a review as the points read it.
type pointReview struct {
	Head    string `json:"head"`
	Verdict string `json:"verdict,omitempty"`
	Blocker bool   `json:"blocker"`
}

// ready reports whether the review is READY at head.
func (r pointReview) ready(head string) bool {
	return r.Verdict == verdictReady && !r.Blocker && r.Head != "" && head != "" &&
		(strings.HasPrefix(head, r.Head) || strings.HasPrefix(r.Head, head))
}

// pullKey names a watched pull request as the points keep it.
func pullKey(host, repository string, n int64) string {
	return fmt.Sprintf("%s/%s#%d", host, repository, n)
}

func pointsPathIn(home string) string { return filepath.Join(home, filepath.FromSlash(PointsFile)) }

// pointState is the launcher's points state: the document, and the edges
// in flight, so a poll during a run does not fire the point again.
type pointState struct {
	mu       sync.Mutex
	inflight map[string]bool
	// run serializes the dispatches: the points of one launcher run in
	// order, one at a time, beside the steps.
	run sync.Mutex
	wg  sync.WaitGroup
}

func readPoints(home string) pointsDoc {
	var d pointsDoc
	if raw, err := os.ReadFile(pointsPathIn(home)); err == nil {
		_ = json.Unmarshal(raw, &d)
	}
	if d.Pulls == nil {
		d.Pulls = map[string]*pullPoints{}
	}
	return d
}

// writePoints writes d, without the pull requests the launcher no longer
// watches, except keep and the merged ones whose after-merge has not run.
func writePoints(home string, d pointsDoc, keep string) error {
	watched := map[string]bool{keep: true}
	for _, w := range readWatched(home) {
		watched[pullKey(w.Host, w.Repository, w.Number)] = true
	}
	for k, p := range d.Pulls {
		if !watched[k] && !p.pendingMerge() {
			delete(d.Pulls, k)
		}
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	if err := os.MkdirAll(filepath.Dir(pointsPathIn(home)), 0o700); err != nil {
		return err
	}
	return writeAtomic(pointsPathIn(home), append(b, '\n'))
}

// pullFacts is what an observation knows beyond the status: the base
// branch, when a poll read it, and the reviews, when the content was read.
type pullFacts struct {
	Base    string
	Reviews map[string]pointReview // nil when no content was read
}

// reviewsOf is the newest review of each reviewer in c, for the points.
func reviewsOf(c forge.PullContent) map[string]pointReview {
	out := map[string]pointReview{}
	for i := len(c.Comments) - 1; i >= 0; i-- {
		cm := c.Comments[i]
		if cm.Review == nil {
			continue
		}
		if _, seen := out[cm.Review.Reviewer]; seen {
			continue
		}
		v := cm.Review.Verdict
		out[cm.Review.Reviewer] = pointReview{Head: cm.Review.Head, Verdict: v,
			Blocker: v != verdictReady && (v != "" || strings.Contains(cm.Body, "[BLOCKER]"))}
	}
	return out
}

// edges is the points that fire for o, in the points' order, after p took
// o's head, base and reviews. A point fires once for its edge: the
// after-push point for a head, after-ci for a head and a conclusion,
// ci-failed for a head that failed, gates-green for a head whose local and
// external reviews are READY while CI passed, after-merge for the merge.
func (p *pullPoints) edges(o PullObservation, f pullFacts, inflight map[string]bool, key string) []string {
	if o.HeadCommit != "" && o.HeadCommit != p.Head {
		p.Previous, p.Head = p.Head, o.HeadCommit
	}
	if f.Base != "" {
		p.Base = f.Base
	}
	if f.Reviews != nil {
		p.Reviews = f.Reviews
	}
	if o.State != "" {
		p.State = o.State
	}
	p.Number, p.URL, p.Seen = o.Number, o.URL, time.Now().UTC()
	head := p.Head
	var out []string
	fire := func(point, edge string) {
		if edge != "" && !slices.Contains(p.Fired[point], edge) && !inflight[key+" "+point] {
			out = append(out, point)
		}
	}
	if o.State == forge.PullOpen && head != "" {
		fire(automation.PointAfterPush, head)
		if o.Checks == forge.ChecksSuccess || o.Checks == forge.ChecksFailure {
			fire(automation.PointAfterCI, head+" "+o.Checks)
		}
		if o.Checks == forge.ChecksFailure {
			fire(automation.PointCIFailed, head+" "+o.Checks)
		}
		if o.Checks == forge.ChecksSuccess && p.Reviews[forge.ReviewerLocal].ready(head) && p.Reviews[forge.ReviewerExternal].ready(head) {
			fire(automation.PointGatesGreen, head)
		}
	}
	if o.State == forge.PullMerged {
		fire(automation.PointAfterMerge, "merged")
	}
	return out
}

// edgeOf is the edge a point fired for at o.
func edgeOf(point string, o PullObservation, head string) string {
	switch point {
	case automation.PointAfterCI, automation.PointCIFailed:
		return head + " " + o.Checks
	case automation.PointAfterMerge:
		return "merged"
	}
	return head
}

// noticePull takes an observation aicrewd accepted for attempt, records the
// pull request's facts, and dispatches, in the background, the points whose
// edge it is. It runs beside the steps and returns at once.
func (s *StepServer) noticePull(o PullObservation, attempt string, f pullFacts) {
	key := pullKey(o.Host, o.Repository, o.Number)
	s.points.mu.Lock()
	doc := readPoints(s.home)
	p := doc.Pulls[key]
	if p == nil || p.Attempt != attempt {
		p = &pullPoints{Attempt: attempt}
		doc.Pulls[key] = p
	}
	if s.points.inflight == nil {
		s.points.inflight = map[string]bool{}
	}
	points := p.edges(o, f, s.points.inflight, key)
	for _, pt := range points {
		s.points.inflight[key+" "+pt] = true
	}
	err := writePoints(s.home, doc, key)
	s.points.mu.Unlock()
	if err != nil {
		s.log.Warn("the points' state was not written", "error", err.Error())
	}
	if len(points) == 0 {
		return
	}
	snapshot := *p
	s.points.wg.Add(1)
	go func() {
		defer s.points.wg.Done()
		s.points.run.Lock()
		defer s.points.run.Unlock()
		for _, pt := range points {
			s.firePoint(s.ctx, key, &snapshot, pt, o)
		}
	}()
}

// waitPoints waits for the dispatches in flight; the tests use it.
func (s *StepServer) waitPoints() { s.points.wg.Wait() }

// replayPoints dispatches the points the kept state still owes: a merged
// pull request whose after-merge run the launcher's exit cut short. The
// merge took the pull request off the watch list, so no poll sees it
// again; the poll calls this first.
func (s *StepServer) replayPoints() {
	s.points.mu.Lock()
	doc := readPoints(s.home)
	s.points.mu.Unlock()
	for key, p := range doc.Pulls {
		if p.pendingMerge() {
			s.noticePull(p.observation(key), p.Attempt, pullFacts{})
		}
	}
}

// firePoint runs the automations at point for the pull request, records the
// edge as fired, and clears it from the edges in flight. A run the
// launcher's exit cut short records no edge: the next launcher runs the
// point again at its next observation (12.4, at least once). At
// after-push, the first declared check that failed is kept for the head
// and sent to aicrewd at once with the observation that fired the point,
// so the worker is woken with it (2d-b).
func (s *StepServer) firePoint(ctx context.Context, key string, p *pullPoints, point string, o PullObservation) {
	baseOnly := false
	if point == automation.PointAfterPush {
		baseOnly = s.baseOnly(ctx, p)
	}
	failed := s.runPoint(ctx, p, point, o, baseOnly)
	edge := edgeOf(point, o, p.Head)
	s.points.mu.Lock()
	delete(s.points.inflight, key+" "+point)
	if ctx.Err() != nil {
		s.points.mu.Unlock()
		return
	}
	doc := readPoints(s.home)
	cur := doc.Pulls[key]
	if cur == nil {
		cur = p
		doc.Pulls[key] = cur
	}
	if cur.Fired == nil {
		cur.Fired = map[string][]string{}
	}
	if !slices.Contains(cur.Fired[point], edge) {
		cur.Fired[point] = append(cur.Fired[point], edge)
	}
	var announce *PullObservation
	if point == automation.PointAfterPush {
		cur.Identity = p.Identity
		cur.PrePush = nil
		if len(failed) > 0 {
			cur.PrePush = &pointPrePush{Head: p.Head, Failure: prePushOf(failed)}
			if o.HeadCommit == p.Head {
				again := o
				again.Baseline, again.PrePush = false, &cur.PrePush.Failure
				announce = &again
			}
		}
	}
	if err := writePoints(s.home, doc, key); err != nil {
		s.log.Warn("the points' state was not written", "error", err.Error())
	}
	s.points.mu.Unlock()
	if announce != nil {
		body := fitObservation(announce)
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if _, err := s.local.LocalStep(rctx, newKey("pull"), s.driver.Session.stepToken(), pullsPath, body); err != nil {
			s.log.Warn("automations: report the failed check", "pull", p.Number, "error", err.Error())
		}
	}
}

// checkFailure is a declared check at after-push that ran and exited
// nonzero: the check's verdict on the head, for the worker.
type checkFailure struct {
	name    string
	failure PrePushFailure
}

// prePushOf is the failure the observation carries: the first check that
// failed, naming the others, in the shape aicrewd takes.
func prePushOf(failed []checkFailure) PrePushFailure {
	out := failed[0].failure
	for _, f := range failed[1:] {
		out.Others = append(out.Others, f.name)
	}
	normalizePrePush(&out)
	return out
}

// The bounds of a failed check as aicrewd's observation takes it
// (store/forgeevents.go, validate): the output as a log excerpt is bounded
// (forge.MaxExcerpt), the command in words within a check field, and the
// other checks' names within the failed checks the route takes. The
// manifest admits more (32 words of 256 bytes, 64 entries), so the
// failure is cut to these before it is sent.
const (
	maxPrePushOutput  = 8 << 10
	maxPrePushCommand = 1 << 10
	maxPrePushOthers  = 40
)

// normalizePrePush cuts p to the bounds above and to the text contract:
// no NUL and valid UTF-8 in the output, a cut marked; the command on one
// line, a cut marked at its end; at most maxPrePushOthers names.
func normalizePrePush(p *PrePushFailure) {
	out, cut := automation.Bound(strings.ReplaceAll(p.Output, "\x00", ""), maxPrePushOutput)
	p.Output, p.Cut = out, p.Cut || cut
	p.Command = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(p.Command)
	if mark := " [cut]"; len(p.Command) > maxPrePushCommand {
		p.Command = bound(p.Command, maxPrePushCommand-len(mark)) + mark
	}
	if len(p.Others) > maxPrePushOthers {
		p.Others = p.Others[:maxPrePushOthers]
	}
}

// baseOnly computes the head's patch identity against its merge base in
// the attempt's worktree, keeps it with the previous head's, and reports
// whether the two are equal: a base-only update (4.1, 10.4).
func (s *StepServer) baseOnly(ctx context.Context, p *pullPoints) bool {
	if p.Identity == nil {
		p.Identity = map[string]string{}
	}
	base := p.Base
	if base == "" {
		if st, err := s.Status(ctx); err == nil && st.Attempt != nil && st.Attempt.Repository != nil {
			base = st.Attempt.Repository.DefaultBranch
		}
	}
	worktree := filepath.Join(s.home, "worktrees", p.Attempt)
	id := patchIdentity(ctx, worktree, base, p.Head)
	for h := range p.Identity {
		if h != p.Head && h != p.Previous {
			delete(p.Identity, h)
		}
	}
	p.Identity[p.Head] = id
	prev, ok := p.Identity[p.Previous]
	return id != "" && ok && prev == id
}

// patchIdentity is the stable patch id of head against its merge base with
// the base branch, as the worktree's origin has them after a fetch of
// both; "" when it cannot be computed.
func patchIdentity(ctx context.Context, worktree, base, head string) string {
	if base == "" || head == "" || !baseShape.MatchString(head) || !branchShape.MatchString(base) || strings.Contains(base, "..") {
		return ""
	}
	if fi, err := os.Stat(worktree); err != nil || !fi.IsDir() {
		return ""
	}
	git := automationGit
	if git == nil {
		git = execGit
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ref := "refs/remotes/origin/" + base
	git(ctx, worktree, gitEnv, "fetch", "--quiet", "origin", "+refs/heads/"+base+":"+ref)
	if _, err := git(ctx, worktree, gitEnv, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil {
		git(ctx, worktree, gitEnv, "fetch", "--quiet", "origin", head)
		if _, err := git(ctx, worktree, gitEnv, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil {
			return ""
		}
	}
	mb, err := git(ctx, worktree, gitEnv, "merge-base", ref, head)
	if mb = strings.TrimSpace(mb); err != nil || !baseShape.MatchString(mb) {
		return ""
	}
	diff, err := git(ctx, worktree, gitEnv, "diff", mb, head)
	if err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	cmd.Dir = worktree
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Stdin = strings.NewReader(diff)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if !baseShape.MatchString(id) {
		return ""
	}
	return id
}

// runPoint runs the effective automations declared at point for the pull
// request of p's attempt, and reports each run to aicrewd. It returns the
// declared checks that failed at after-push, in the set's order.
func (s *StepServer) runPoint(ctx context.Context, p *pullPoints, point string, o PullObservation, baseOnly bool) (failed []checkFailure) {
	st, err := s.Status(ctx)
	if err != nil {
		s.log.Warn("automations: read the session status", "point", point, "error", err.Error())
		return nil
	}
	if st.Attempt == nil || st.Attempt.ID != p.Attempt {
		s.log.Info("automations: the pull request's attempt is no longer the member's open attempt", "point", point, "attempt", p.Attempt)
		return nil
	}
	var src AutomationSources
	if a := st.Attempt; a.Process != nil && a.Repository != nil {
		src = AutomationSources{Process: a.Process, Repository: a.Repository.URL, BaseBranch: a.Repository.DefaultBranch}
		if p.Base != "" {
			src.BaseBranch = p.Base
		}
	}
	set, err := s.Automations(ctx, src)
	if err != nil {
		s.log.Warn("automations: the effective set could not be read", "point", point, "error", RedactForgeTokens(s.home, err.Error()))
		return nil
	}
	head := p.Head
	event := automation.Event{Kinds: []string{point}, IDs: []string{pullKey(o.Host, o.Repository, o.Number) + "@" + head}, BaseOnly: baseOnly}
	for _, e := range set.Entries {
		if e.Point != point || e.Source == automation.SourceDisabled {
			continue
		}
		rep := automationReport{AttemptID: p.Attempt, Point: point, Name: e.Name, Source: e.Source, Commit: e.Commit,
			Script: e.Script, Command: e.Command, Pull: o.Number, URL: o.URL, Head: head, BaseOnly: baseOnly}
		switch {
		case e.Problem != "":
			rep.RunID, rep.Outcome, rep.Error = newUUID(), runFailed, e.Problem
			s.recordProblem(rep)
		case e.Source == automation.SourceCommand:
			s.runCommand(ctx, &rep, e.Command)
			if rep.check != nil {
				failed = append(failed, *rep.check)
			}
		default:
			res, err := s.RunAutomation(ctx, AutomationRun{AttemptID: p.Attempt, Pull: o.Number, Name: e.Script, Source: e.Code, Event: event, Point: point})
			if err != nil {
				rep.RunID, rep.Outcome, rep.Error = newUUID(), runFailed, redactText(s.home, err.Error())
				s.recordProblem(rep)
				break
			}
			rep.RunID, rep.Steps = res.RunID, res.Steps
			switch {
			case res.Failed != "":
				rep.Outcome, rep.Error = runFailed, res.Failed
			case res.Turn != nil:
				rep.Outcome, rep.Turn, rep.Content = runTurn, res.Turn.Kind, res.Turn.Content
			default:
				rep.Outcome = runHandled
			}
		}
		if ctx.Err() != nil {
			// The launcher is stopping: the run it cut is not a failure
			// of the automation, and the point runs again.
			return nil
		}
		s.reportRun(ctx, rep)
	}
	return failed
}

// How a run ended, as aicrewd names it (store/automations.go).
const (
	runHandled = "handled"
	runTurn    = "turn"
	runFailed  = "failed"
)

// automationReport is a run as the launcher reports it to aicrewd, in the
// shape of its route's body.
type automationReport struct {
	AttemptID string           `json:"attempt_id"`
	RunID     string           `json:"run_id"`
	Point     string           `json:"point"`
	Name      string           `json:"name"`
	Source    string           `json:"source"`
	Commit    string           `json:"commit,omitempty"`
	Script    string           `json:"script,omitempty"`
	Command   []string         `json:"command,omitempty"`
	Pull      int64            `json:"pull"`
	URL       string           `json:"url"`
	Head      string           `json:"head"`
	BaseOnly  bool             `json:"base_only,omitempty"`
	Outcome   string           `json:"outcome"`
	Turn      string           `json:"turn,omitempty"`
	Content   string           `json:"content,omitempty"`
	Error     string           `json:"error,omitempty"`
	Exit      *int             `json:"exit,omitempty"`
	Steps     []automationStep `json:"steps,omitempty"`
	// check is a declared check at after-push that exited nonzero: the
	// worker's, through the observation, not part of the report.
	check *checkFailure
}

// automationStep is a step a run recorded, as reported.
type automationStep struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// The bounds of what a report carries of a run's steps, within aicrewd's.
const (
	maxReportSteps    = 20
	maxReportStepText = 1 << 10
	maxReportContent  = 4 << 10
	maxReportError    = 2 << 10
)

// recordProblem records an entry that could not run as a failed run in
// the home's log.
func (s *StepServer) recordProblem(rep automationReport) {
	run := &liveRun{id: rep.RunID, attempt: rep.AttemptID, script: rep.Name}
	s.recordStep(run, stepAutomationFailed, rep.Error)
	if err := appendAutomationRecord(s.home, automationRecord{At: time.Now().UTC(), Run: rep.RunID, Attempt: rep.AttemptID,
		Script: rep.Name, Point: rep.Point, Outcome: runFailed}); err != nil {
		s.log.Warn("a run was not recorded", "error", err.Error())
	}
}

// runCommand runs a declared command for rep: no shell, in the attempt's
// worktree, under the turn's allow list, with the client's environment and
// nothing of the home's. Its output, bounded and redacted, is the run's
// one step; a refused command, one that cannot start or one that passes
// its wall-clock limit is a failed run, and so is a nonzero exit, except
// at after-push, where it is the check's verdict on the head: the run is
// handled, and the failure goes to the worker (firePoint).
func (s *StepServer) runCommand(ctx context.Context, rep *automationReport, cmdline []string) {
	rep.RunID = newUUID()
	run := &liveRun{id: rep.RunID, attempt: rep.AttemptID, script: rep.Name, point: rep.Point}
	fail := func(msg string) {
		rep.Outcome, rep.Error = runFailed, redactText(s.home, msg)
		s.recordStep(run, stepAutomationFailed, rep.Error)
		s.recordEnd(run, rep.Outcome, cmdline, rep.Exit)
	}
	if len(cmdline) == 0 {
		fail("the entry names no command")
		return
	}
	worktree := filepath.Join(s.home, "worktrees", rep.AttemptID)
	if fi, err := os.Stat(worktree); err != nil || !fi.IsDir() {
		fail(fmt.Sprintf("the attempt %s has no worktree in this home", rep.AttemptID))
		return
	}
	names, _, err := teamToolchains(s.home)
	if err != nil {
		fail("read the team's toolchains: " + err.Error())
		return
	}
	if err := TurnAllowList(s.home, names).Admit(cmdline[0], cmdline[1:]); err != nil {
		fail(err.Error())
		return
	}
	if isCredentialCommand(cmdline) {
		fail("a declared command is never the credential helper: " + strings.Join(cmdline, " "))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, commandWall)
	defer cancel()
	cmd := exec.CommandContext(cctx, cmdline[0], cmdline[1:]...)
	cmd.Dir = worktree
	cmd.Env = s.commandEnv()
	var out capped
	out.n = maxCommandOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	text, _ := automation.Bound(hostRedactor(s.home)(out.b.String()), automation.MaxRunOutput)
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) || err == nil:
		code := cmd.ProcessState.ExitCode()
		rep.Exit = &code
		rep.Steps = []automationStep{{Kind: stepCommand, Text: bound(text, maxReportStepText)}}
		if err := appendAutomationRecord(s.home, automationRecord{At: time.Now().UTC(), Run: run.id, Attempt: run.attempt,
			Script: run.script, Point: run.point, Kind: stepCommand, Text: redactText(s.home, text), Command: cmdline, Exit: &code}); err != nil {
			s.log.Warn("a command's step was not recorded", "error", err.Error())
		}
		switch {
		case errors.Is(cctx.Err(), context.DeadlineExceeded):
			fail(fmt.Sprintf("the command passed its wall-clock limit of %s, and was stopped", commandWall))
		case code != 0 && rep.Point == automation.PointAfterPush:
			out, cut := automation.Bound(text, maxPrePushOutput)
			rep.Outcome = runHandled
			rep.check = &checkFailure{name: rep.Name, failure: PrePushFailure{Command: strings.Join(cmdline, " "), Exit: code, Output: out, Cut: cut}}
			s.recordEnd(run, rep.Outcome, cmdline, rep.Exit)
		case code != 0:
			fail(fmt.Sprintf("exit status %d", code))
		default:
			rep.Outcome = runHandled
			s.recordEnd(run, rep.Outcome, cmdline, rep.Exit)
		}
	default:
		fail("the command did not start: " + err.Error())
	}
}

// isCredentialCommand reports whether cmdline is the credential helper
// itself, `aicrew-agent git-credential ...`, which the allow list's
// `Bash(aicrew-agent *)` would admit; a declared command never runs it.
func isCredentialCommand(cmdline []string) bool {
	if len(cmdline) < 2 {
		return false
	}
	prog := strings.TrimSuffix(strings.ToLower(filepath.Base(cmdline[0])), ".exe")
	return prog == "aicrew-agent" && cmdline[1] == "git-credential"
}

// recordEnd records how a command's run ended in the home's log.
func (s *StepServer) recordEnd(run *liveRun, outcome string, cmdline []string, exit *int) {
	if err := appendAutomationRecord(s.home, automationRecord{At: time.Now().UTC(), Run: run.id, Attempt: run.attempt,
		Script: run.script, Point: run.point, Command: cmdline, Outcome: outcome, Exit: exit}); err != nil {
		s.log.Warn("a run was not recorded", "error", err.Error())
	}
}

// bound is s cut to n bytes of valid UTF-8.
func bound(s string, n int) string {
	out, _ := automation.Bound(s, n)
	return out
}

// reportRun reports a run to aicrewd as the launcher's session. A report
// aicrewd does not take is logged; the home's log holds the run.
func (s *StepServer) reportRun(ctx context.Context, rep automationReport) {
	body := fitReport(&rep)
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := s.local.LocalStep(rctx, newKey("automation"), s.driver.Session.stepToken(), automationsPath, body); err != nil {
		s.log.Warn("automations: report a run", "run", rep.RunID, "name", rep.Name, "point", rep.Point, "error", err.Error())
	}
}

// maxReportEncoded bounds a report as encoded, under the route's body
// limit (server/attempts.go maxReportBody): JSON escaping can multiply a
// text's bytes.
const maxReportEncoded = 40 << 10

// fitReport cuts rep to what aicrewd takes, and encodes it: the texts
// without NUL (which aicrewd refuses), each bounded, then the steps' texts,
// the content and the error halved until the encoding fits, then the
// steps themselves, and at last the command's words, each cut and the cut
// marked. A failed run keeps an error.
func fitReport(rep *automationReport) []byte {
	clean := func(s string, n int) string { return bound(strings.ReplaceAll(s, "\x00", ""), n) }
	if len(rep.Steps) > maxReportSteps {
		rep.Steps = rep.Steps[:maxReportSteps]
	}
	for i := range rep.Steps {
		rep.Steps[i].Text = clean(rep.Steps[i].Text, maxReportStepText)
	}
	rep.Content, rep.Error = clean(rep.Content, maxReportContent), clean(rep.Error, maxReportError)
	if rep.Outcome == runFailed && strings.TrimSpace(rep.Error) == "" {
		rep.Error = "the run failed; its error text could not be reported"
	}
	body, _ := json.Marshal(rep)
	for n := maxReportStepText / 2; len(body) > maxReportEncoded && n >= 64; n /= 2 {
		for i := range rep.Steps {
			rep.Steps[i].Text = bound(rep.Steps[i].Text, n)
		}
		rep.Content, rep.Error = bound(rep.Content, 4*n), bound(rep.Error, 2*n)
		body, _ = json.Marshal(rep)
	}
	for len(body) > maxReportEncoded && len(rep.Steps) > 0 {
		rep.Steps = rep.Steps[:len(rep.Steps)/2]
		body, _ = json.Marshal(rep)
	}
	for n := 64; len(body) > maxReportEncoded && len(rep.Command) > 0 && n >= 4; n /= 4 {
		cut := false
		for i, a := range rep.Command {
			if len(a) > n {
				rep.Command[i], cut = bound(a, n), true
			}
		}
		if last := len(rep.Command) - 1; cut && rep.Command[last] != "[cut]" {
			// The mark takes a word's place when the list is full:
			// aicrewd takes MaxCommandArgs words, as the manifest does.
			if len(rep.Command) >= automation.MaxCommandArgs {
				rep.Command[last] = "[cut]"
			} else {
				rep.Command = append(rep.Command, "[cut]")
			}
		}
		body, _ = json.Marshal(rep)
	}
	return body
}

// beforeSubmit fires the before-submit point for the attempt's watched pull
// request, in the caller's step, so the point's automations run before the
// submit is sent. An attempt with no watched pull request has nothing to
// run at.
func (s *StepServer) beforeSubmit(ctx context.Context, attemptID string) {
	s.points.run.Lock()
	defer s.points.run.Unlock()
	s.fireBeforeSubmit(ctx, attemptID)
}

// fireBeforeSubmit is beforeSubmit for a caller that holds the points' run
// lock already: a script's submit() (automation.go), served while its own
// run holds it.
func (s *StepServer) fireBeforeSubmit(ctx context.Context, attemptID string) {
	s.points.mu.Lock()
	doc := readPoints(s.home)
	var key string
	var p *pullPoints
	for k, cand := range doc.Pulls {
		if cand.Attempt == attemptID && (p == nil || cand.Seen.After(p.Seen)) {
			key, p = k, cand
		}
	}
	fires := p != nil && p.Head != "" && !slices.Contains(p.Fired[automation.PointBeforeSubmit], p.Head) &&
		!s.points.inflight[key+" "+automation.PointBeforeSubmit]
	if fires {
		if s.points.inflight == nil {
			s.points.inflight = map[string]bool{}
		}
		s.points.inflight[key+" "+automation.PointBeforeSubmit] = true
	}
	s.points.mu.Unlock()
	if !fires {
		return
	}
	snapshot := *p
	o := p.observation(key)
	o.State = forge.PullOpen
	s.firePoint(ctx, key, &snapshot, automation.PointBeforeSubmit, o)
}

// workIntent is the intent of a work step's body, "" when it has none.
func workIntent(body []byte) string {
	var in struct {
		Intent string `json:"intent"`
	}
	_ = json.Unmarshal(body, &in)
	return in.Intent
}

// attemptBase is the base branch of the attempt's watched pull request,
// "" when none was read.
func (s *StepServer) attemptBase(attemptID string) string {
	s.points.mu.Lock()
	defer s.points.mu.Unlock()
	base := ""
	var seen time.Time
	for _, p := range readPoints(s.home).Pulls {
		if p.Attempt == attemptID && p.Base != "" && p.Seen.After(seen) {
			base, seen = p.Base, p.Seen
		}
	}
	return base
}
