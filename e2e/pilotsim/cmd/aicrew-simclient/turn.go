package main

// The turn mode (docs/DESIGN-PILOT-SIMULATION.md, sections 2.2 to 2.4): the
// launcher runs `claude -p --output-format stream-json ... --session-id ID`
// in the home with `/crew-turn <brief>` on standard input. The scripted
// member reads its role and attempt from the brief and its messages from
// `aicrew-agent inbox --json`, acts only through the product's surfaces
// (aicrew-agent's steps, clone and forge commands, git in its worktree, the
// home's aimem MCP server), acks the messages it read, records the turn in
// docs/HANDOFF.md, and ends with one stream-json result line.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/forge"
)

// turnMode reports the launcher's turn: print mode with a session ID.
func turnMode(args []string) bool {
	return printMode(args) && slices.Contains(args, "--session-id")
}

// brief is what the scripted member reads of its brief (internal/agent,
// brief.go): the first line and the "Who and where" section.
type brief struct {
	Attempt, State, Phase string
	Role, Team, Agent     string
	Task, Project, Hub    string
	Branch, Base, Repo    string
}

var (
	briefFirst  = regexp.MustCompile(`^Attempt (\S+): ([^/\s]+)/([^.\s]+)\.$`)
	briefRole   = regexp.MustCompile(`(?m)^- Role: (\S+); team (\S*); agent (\S*)\.$`)
	briefTask   = regexp.MustCompile(`(?m)^- Attempt (\S+) on task (\S+) \(project (\S+), hub (\S+)\)\.$`)
	briefBranch = regexp.MustCompile("(?m)^- Branch (\\S+) from base commit (\\S+); worktree `")
	briefRepo   = regexp.MustCompile(`(?m)^- Repository (\S+) \(default branch \S+\)\.$`)
)

// parseBrief reads the brief after the /crew-turn command.
func parseBrief(prompt string) (brief, error) {
	text, ok := strings.CutPrefix(prompt, "/crew-turn ")
	if !ok {
		return brief{}, errors.New("the prompt is not /crew-turn")
	}
	first, _, _ := strings.Cut(text, "\n")
	var b brief
	switch m := briefFirst.FindStringSubmatch(first); {
	case m != nil:
		b.Attempt, b.State, b.Phase = m[1], m[2], m[3]
	case first != "No open attempt.":
		return brief{}, fmt.Errorf("the brief's first line is neither an attempt nor none: %q", first)
	}
	m := briefRole.FindStringSubmatch(text)
	if m == nil {
		return brief{}, errors.New("the brief names no role")
	}
	b.Role, b.Team, b.Agent = m[1], m[2], m[3]
	if b.Attempt == "" {
		return b, nil
	}
	if m := briefTask.FindStringSubmatch(text); m != nil && m[1] == b.Attempt {
		b.Task, b.Project, b.Hub = m[2], m[3], m[4]
	}
	if m := briefBranch.FindStringSubmatch(text); m != nil {
		b.Branch, b.Base = m[1], m[2]
	}
	if m := briefRepo.FindStringSubmatch(text); m != nil {
		b.Repo = m[1]
	}
	return b, nil
}

// message is what the scripts read of an inbox message (internal/store,
// InboxItem).
type message struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	AttemptID string         `json:"attempt_id"`
	Task      *agent.TaskRef `json:"task"`
	Project   *struct {
		HubID     string `json:"hub_id"`
		ProjectID string `json:"project_id"`
	} `json:"project"`
	Offer *struct {
		InstructionDigest string `json:"instruction_digest"`
	} `json:"offer"`
	Action *struct {
		Subjects []struct {
			Kind string         `json:"kind"`
			Task *agent.TaskRef `json:"task"`
		} `json:"subjects"`
	} `json:"action"`
	Text string `json:"text"`
}

// runFunc runs a command in dir with stdin and returns its standard output
// and exit code; err is a command that could not run.
type runFunc func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, int, error)

// mcpFunc calls one tool of the home's aimem MCP server and returns its text.
type mcpFunc func(ctx context.Context, tool string, args map[string]any) (string, error)

// member is one turn of the scripted member.
type member struct {
	home string
	run  runFunc
	mcp  mcpFunc
	now  func() time.Time
	did  []string // what the turn did, in order
}

func (m *member) note(format string, a ...any) { m.did = append(m.did, fmt.Sprintf(format, a...)) }

// agentJSON runs `aicrew-agent ARGS --json` in dir and decodes its answer;
// args that name --json themselves place it before their positional
// arguments. Exit 4 (recorded, not settled) is not an error: `step
// recover` settles it.
func (m *member) agentJSON(ctx context.Context, dir, stdin string, out any, args ...string) error {
	if !slices.Contains(args, "--json") {
		args = append(args, "--json")
	}
	raw, code, err := m.run(ctx, dir, stdin, "aicrew-agent", args...)
	if err != nil {
		return fmt.Errorf("aicrew-agent %s: %w", args[0], err)
	}
	if code != 0 && code != 4 {
		return fmt.Errorf("aicrew-agent %s exited %d: %s", strings.Join(args[:min(2, len(args))], " "), code,
			strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("aicrew-agent %s: the answer is not JSON: %w", args[0], err)
	}
	return nil
}

// step runs one attempt step.
func (m *member) step(ctx context.Context, op, attempt, task string, body any, extra ...string) (agent.StepAnswer, error) {
	args := []string{"step", op}
	if attempt != "" {
		args = append(args, "--attempt", attempt)
	}
	if task != "" {
		args = append(args, "--task", task)
	}
	if body != nil {
		raw, _ := json.Marshal(body)
		args = append(args, "--body", string(raw))
	}
	args = append(args, extra...)
	raw, code, err := m.run(ctx, m.home, "", "aicrew-agent", args...)
	if err != nil {
		return agent.StepAnswer{}, fmt.Errorf("step %s: %w", op, err)
	}
	var ans agent.StepAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return ans, fmt.Errorf("step %s: the answer is not JSON (exit %d): %.400s", op, code, strings.TrimSpace(string(raw)))
	}
	if code != 0 && code != 4 {
		why := ans.Status
		if ans.Error != nil {
			why = ans.Error.Code + ": " + ans.Error.Message
		}
		return ans, fmt.Errorf("step %s exited %d: %s", op, code, why)
	}
	return ans, nil
}

// inbox reads the member's unacknowledged messages.
func (m *member) inbox(ctx context.Context) ([]message, error) {
	var ans agent.StepAnswer
	if err := m.agentJSON(ctx, m.home, "", &ans, "inbox"); err != nil {
		return nil, err
	}
	var res struct {
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal(ans.Result, &res); err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	return res.Messages, nil
}

// ack acknowledges the messages the turn read: each is handled, or not the
// script's to act on.
func (m *member) ack(ctx context.Context, msgs []message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]string, len(msgs))
	for i, msg := range msgs {
		ids[i] = msg.ID
	}
	_, code, err := m.run(ctx, m.home, "", "aicrew-agent", "inbox", "--ack", strings.Join(ids, ","))
	if err != nil || code != 0 {
		return fmt.Errorf("inbox --ack exited %d: %v", code, err)
	}
	m.note("Acked %d message(s).", len(ids))
	return nil
}

// turn runs one turn: the role's script, the acks and the handoff.
func (m *member) turn(ctx context.Context, b brief) error {
	msgs, err := m.inbox(ctx)
	if err != nil {
		return err
	}
	switch b.Role {
	case "worker":
		err = m.worker(ctx, b, msgs)
	case "coordinator":
		err = m.coordinator(ctx, msgs)
	default:
		err = fmt.Errorf("no script for the role %q", b.Role)
	}
	if err == nil {
		err = m.ack(ctx, msgs)
	}
	if err == nil && len(m.did) == 0 {
		m.note("Nothing to do.")
	}
	// What a failed turn did is recorded too: the next turn goes on from it.
	if len(m.did) > 0 {
		if herr := m.recordHandoff(b); err == nil {
			err = herr
		}
	}
	return err
}

// ---- the worker ----------------------------------------------------------------

func (m *member) worker(ctx context.Context, b brief, msgs []message) error {
	if b.Attempt == "" || b.State == "offered" {
		for _, msg := range msgs {
			if msg.Offer == nil || msg.AttemptID == "" || msg.Task == nil {
				continue
			}
			if _, err := m.step(ctx, "accept", msg.AttemptID, msg.Task.TaskID,
				map[string]string{"instruction_digest": msg.Offer.InstructionDigest}); err != nil {
				return err
			}
			m.note("Accepted the offer of attempt %s on task %s.", msg.AttemptID, msg.Task.TaskID)
			return nil
		}
		return nil
	}
	if b.State != "running" || (b.Phase != "working" && b.Phase != "rework") {
		m.note("Attempt %s is %s/%s: nothing for the worker to do.", b.Attempt, b.State, b.Phase)
		return nil
	}
	wt := filepath.Join(m.home, "worktrees", b.Attempt)
	if _, err := os.Stat(wt); errors.Is(err, os.ErrNotExist) {
		var out any
		if err := m.agentJSON(ctx, m.home, "", &out, "clone", "--repository", b.Repo, "--attempt", b.Attempt,
			"--base", b.Base, "--branch", b.Branch); err != nil {
			return err
		}
		m.note("Cloned %s and made the worktree on %s from %s.", b.Repo, b.Branch, b.Base)
	}
	n := recordedPull(m.section())
	if n == 0 {
		return m.openPull(ctx, b, wt)
	}
	return m.followPull(ctx, b, wt, n)
}

// openPull commits the task's change, pushes it with the home's credential,
// opens the pull request and asks for the external review.
func (m *member) openPull(ctx context.Context, b brief, wt string) error {
	file := filepath.Join("sim", b.Task+".md")
	if err := os.MkdirAll(filepath.Join(wt, "sim"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(wt, file), []byte("Task "+b.Task+", attempt "+b.Attempt+".\n"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"add", filepath.ToSlash(file)},
		{"-c", "user.name=sim-worker", "-c", "user.email=sim-worker@example.test", "-c", "commit.gpgsign=false",
			"commit", "-q", "-m", "sim: task " + b.Task},
		{"push", "-q", "origin", b.Branch},
	} {
		if out, code, err := m.run(ctx, wt, "", "git", args...); err != nil || code != 0 {
			return fmt.Errorf("git %s exited %d: %v %s", args[len(args)-1], code, err, strings.TrimSpace(string(out)))
		}
	}
	var opened agent.PullReport
	body := "The pilot simulation's change for task " + b.Task + " (attempt " + b.Attempt + ").\n"
	if err := m.agentJSON(ctx, wt, body, &opened, "forge", "pr", "open", "--title", "sim: task "+b.Task,
		"--body-file", "-"); err != nil {
		return err
	}
	m.note("Pull request #%d: %s, opened at head %s.", opened.Pull.Number, opened.Pull.URL, opened.Pull.HeadCommit)
	return m.requestReview(ctx, wt, opened.Pull.Number)
}

// requestReview asks for the external review: the review-this label.
func (m *member) requestReview(ctx context.Context, wt string, n int64) error {
	var labelled agent.PullReport
	if err := m.agentJSON(ctx, wt, "", &labelled, "forge", "pr", "label", "add", "--pr", fmt.Sprint(n), "--json",
		"review-this"); err != nil {
		return err
	}
	m.note("Asked for the external review of #%d (review-this).", n)
	return nil
}

// followPull reads the pull request and submits it once the external review
// is READY at its head and its checks passed; until then it waits.
func (m *member) followPull(ctx context.Context, b brief, wt string, n int64) error {
	var shown agent.PullReport
	if err := m.agentJSON(ctx, wt, "", &shown, "forge", "pr", "show", "--pr", fmt.Sprint(n)); err != nil {
		return err
	}
	p := shown.Pull
	verdict, checks := pullGates(shown.Content)
	switch {
	case p.State != forge.PullOpen:
		m.note("Pull request #%d is %s.", n, p.State)
	case verdict == "" && !slices.ContainsFunc(p.Labels, func(l string) bool {
		return l == "review-this" || l == "hands-reviewing" || l == "hands-reviewed"
	}):
		return m.requestReview(ctx, wt, n)
	case slices.Contains(p.Labels, "review-this") || slices.Contains(p.Labels, "hands-reviewing"):
		m.note("Waiting on the external review of #%d at %s (labels %s).", n, p.HeadCommit, strings.Join(p.Labels, ", "))
	case verdict == "READY_FOR_HUMAN_MERGE" && checks == forge.ChecksSuccess:
		if _, err := m.step(ctx, "work", b.Attempt, b.Task, map[string]string{"intent": "submit", "detail": p.URL}); err != nil {
			return err
		}
		m.note("Submitted #%d (%s): the external review is READY at %s and the checks passed.", n, p.URL, p.HeadCommit)
	default:
		m.note("Pull request #%d at %s: external verdict %q, checks %q; waiting.", n, p.HeadCommit, verdict, checks)
	}
	return nil
}

// pullGates is the external review's verdict at the pull request's head
// ("" for none) and its checks' combined state ("" for none).
func pullGates(c *forge.PullContent) (verdict, checks string) {
	if c == nil {
		return "", ""
	}
	for _, cm := range c.Comments {
		if r := cm.Review; r != nil && r.Reviewer == forge.ReviewerExternal && r.AtHead {
			verdict = r.Verdict // the latest at the head wins
		}
	}
	for _, ch := range c.Checks {
		switch {
		case ch.State == forge.ChecksFailure:
			return verdict, forge.ChecksFailure
		case ch.State != forge.ChecksSuccess:
			checks = forge.ChecksPending
		case checks == "":
			checks = forge.ChecksSuccess
		}
	}
	return verdict, checks
}

var pullLine = regexp.MustCompile(`(?m)^- \S+ Pull request #(\d+): `)

// recordedPull is the pull request number the worker recorded in its
// section's Done, 0 for none.
func recordedPull(section string) int64 {
	var n int64
	if m := pullLine.FindStringSubmatch(section); m != nil {
		fmt.Sscan(m[1], &n)
	}
	return n
}

// ---- the coordinator -------------------------------------------------------------

var taskID = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// candidates are the tasks the messages name: a board change's, or those a
// READY list names in its text, in the order read.
func candidates(msgs []message) []agent.TaskRef {
	var out []agent.TaskRef
	add := func(t agent.TaskRef) {
		if t.TaskID != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	for _, msg := range msgs {
		if msg.Action != nil {
			for _, s := range msg.Action.Subjects {
				if s.Kind == "board" && s.Task != nil {
					add(*s.Task)
				}
			}
		}
		if msg.Kind == "lifecycle" && msg.Project != nil && msg.AttemptID == "" {
			for _, id := range taskID.FindAllString(msg.Text, -1) {
				add(agent.TaskRef{HubID: msg.Project.HubID, ProjectID: msg.Project.ProjectID, TaskID: id})
			}
		}
	}
	return out
}

func (m *member) coordinator(ctx context.Context, msgs []message) error {
	tasks := candidates(msgs)
	if len(tasks) == 0 {
		return nil
	}
	var ans agent.StepAnswer
	if err := m.agentJSON(ctx, m.home, "", &ans, "attempts"); err != nil {
		return err
	}
	var open agent.OpenAttempts
	if err := json.Unmarshal(ans.Result, &open); err != nil {
		return fmt.Errorf("attempts: %w", err)
	}
	for _, t := range tasks {
		if slices.ContainsFunc(open.Attempts, func(a agent.OpenAttempt) bool { return a.Task.TaskID == t.TaskID }) {
			m.note("Task %s has an open attempt.", t.TaskID)
			continue
		}
		if err := m.triageAndOffer(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// boardTask is what the coordinator reads of a task.
type boardTask struct {
	Revision   int64  `json:"revision"`
	State      string `json:"state"`
	NextAction string `json:"next_action"`
}

// offerNext is the next action the coordinator's triage sets; a READY task
// that has it is triaged already.
const offerNext = "Offer it to the team's worker."

func (m *member) readTask(ctx context.Context, id string) (boardTask, error) {
	text, err := m.mcp(ctx, "get_task", map[string]any{"id": id})
	if err != nil {
		return boardTask{}, err
	}
	var t boardTask
	if err := json.Unmarshal([]byte(text), &t); err != nil || t.Revision < 1 {
		return boardTask{}, fmt.Errorf("get_task %s: not a task: %.200s", id, text)
	}
	return t, nil
}

// triageAndOffer triages a BACKLOG or READY task to READY, with its next
// action and a comment, and offers it.
func (m *member) triageAndOffer(ctx context.Context, t agent.TaskRef) error {
	task, err := m.readTask(ctx, t.TaskID)
	if err != nil {
		return err
	}
	if task.State == "BACKLOG" || (task.State == "READY" && task.NextAction != offerNext) {
		key := fmt.Sprintf("sim-triage-%s-%d", t.TaskID, task.Revision)
		text, err := m.mcp(ctx, "triage_task", map[string]any{"id": t.TaskID, "expected_revision": task.Revision,
			"state": "READY", "next_action": offerNext, "idempotency_key": key})
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(text), &task); err != nil || task.State != "READY" {
			return fmt.Errorf("triage_task %s: the task is not READY: %.200s", t.TaskID, text)
		}
		if _, err := m.mcp(ctx, "add_task_comment", map[string]any{"id": t.TaskID,
			"body":            "Triaged READY by the scripted coordinator: the simulation's task has no dependency and fits one attempt.",
			"idempotency_key": key + "-comment"}); err != nil {
			return err
		}
		m.note("Triaged task %s: READY (revision %d).", t.TaskID, task.Revision)
	}
	if task.State != "READY" {
		m.note("Task %s is %s: not offered.", t.TaskID, task.State)
		return nil
	}
	return m.offer(ctx, t, task.Revision)
}

// offer offers the task to a worker that verified the project's repository
// at write access (`aicrew-agent members`), with the repository filled from
// the forge and the process pin and its digest from the hub.
func (m *member) offer(ctx context.Context, t agent.TaskRef, revision int64) error {
	text, err := m.mcp(ctx, "project_repository", map[string]any{"project": t.ProjectID})
	if err != nil {
		return err
	}
	var repo struct {
		Repository *struct {
			URL string `json:"url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(text), &repo); err != nil || repo.Repository == nil || repo.Repository.URL == "" {
		return fmt.Errorf("project_repository %s names no repository: %.200s", t.ProjectID, text)
	}
	worker, err := m.writer(ctx, repo.Repository.URL)
	if err != nil {
		return err
	}
	body := map[string]any{
		"worker_agent_id": worker, "task": t, "expected_revision": revision,
		"repository": map[string]string{"access": "write", "branch": "work/" + t.TaskID},
		"expires_at": m.now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}
	ans, err := m.step(ctx, "offer", "", "", body, "--repository", repo.Repository.URL, "--process")
	if err != nil {
		return err
	}
	var res struct {
		AttemptID string `json:"attempt_id"`
	}
	_ = json.Unmarshal(ans.Result, &res)
	m.note("Offered task %s to %s: attempt %s.", t.TaskID, worker, res.AttemptID)
	return nil
}

// writer is the agent ID of the first worker whose last capability report
// verified the repository at write access.
func (m *member) writer(ctx context.Context, repoURL string) (string, error) {
	var ans agent.StepAnswer
	if err := m.agentJSON(ctx, m.home, "", &ans, "members"); err != nil {
		return "", err
	}
	var team agent.TeamMembers
	if err := json.Unmarshal(ans.Result, &team); err != nil {
		return "", fmt.Errorf("aicrew-agent members: %w", err)
	}
	for _, mb := range team.Members {
		if mb.Role != "worker" {
			continue
		}
		for _, c := range mb.Capabilities {
			for _, r := range c.Repositories {
				if r.URL == repoURL && r.Access == "write" {
					return mb.AgentID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no worker verified %s at write access (aicrew-agent members)", repoURL)
}

// ---- the handoff -------------------------------------------------------------------

const (
	sectionHeading = "## CURRENT ATTEMPT"
	archiveHeading = "## Archive"
)

func (m *member) handoffPath() string {
	return filepath.Join(m.home, filepath.FromSlash(agent.HandoffFile))
}

// section is the CURRENT ATTEMPT section of the home's handoff, "" for none.
func (m *member) section() string {
	raw, err := os.ReadFile(m.handoffPath())
	if err != nil {
		return ""
	}
	_, sec, _ := splitSection(string(raw))
	return sec
}

// splitSection splits the doc around its CURRENT ATTEMPT section.
func splitSection(doc string) (before, section, after string) {
	i := strings.Index(doc, sectionHeading+"\n")
	if i < 0 {
		return doc, "", ""
	}
	rest := doc[i:]
	j := strings.Index(rest, "\n"+archiveHeading+"\n")
	if j < 0 {
		return doc[:i], rest, ""
	}
	return doc[:i], rest[:j+1], rest[j+1:]
}

// recordHandoff writes the turn into the handoff: a worker's lines go under
// the section's Done, and its Next says what the turn leaves; a coordinator,
// which has no section, appends them to the file.
func (m *member) recordHandoff(b brief) error {
	path := m.handoffPath()
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	at := m.now().UTC().Format(time.RFC3339)
	var lines strings.Builder
	for _, d := range m.did {
		fmt.Fprintf(&lines, "- %s %s\n", at, d)
	}
	doc := string(raw)
	before, sec, after := splitSection(doc)
	if b.Role == "worker" && sec != "" {
		sec = editSection(sec, lines.String(), "Turn of "+at+": "+m.did[len(m.did)-1])
		doc = before + sec + after
	} else {
		if doc != "" && !strings.HasSuffix(doc, "\n") {
			doc += "\n"
		}
		doc += lines.String()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(doc), 0o600)
}

// editSection appends done under "### Done" and makes next the body of
// "### Next"; a heading the section lacks is added at its end.
func editSection(sec, done, next string) string {
	sec = withSub(sec, "### Done", func(body string) string { return body + "\n" + done })
	return withSub(sec, "### Next", func(string) string { return next })
}

// withSub replaces the body of the subsection under heading with f of its
// current body, both without their surrounding blank lines; the result
// keeps one blank line after the heading and before the next one.
func withSub(sec, heading string, f func(string) string) string {
	i := strings.Index(sec, "\n"+heading+"\n")
	if i < 0 {
		return strings.TrimRight(sec, "\n") + "\n\n" + heading + "\n\n" + strings.Trim(f(""), "\n") + "\n"
	}
	start := i + 1 + len(heading)
	end := len(sec)
	if j := strings.Index(sec[start:], "\n### "); j >= 0 {
		end = start + j + 1
	}
	body := "\n\n" + strings.Trim(f(strings.Trim(sec[start:end], "\n")), "\n") + "\n"
	if end < len(sec) {
		body += "\n"
	}
	return sec[:start] + body + sec[end:]
}

// ---- the client's side -------------------------------------------------------------

// turnRun is one turn as the launcher runs it: the brief from stdin, the
// script, and the stream-json events with the result last. A script that
// fails ends the turn with an error result, which the launcher records as
// a failed turn.
func turnRun(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, m *member) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	var b brief
	if err == nil {
		b, err = parseBrief(strings.TrimSpace(string(raw)))
	}
	if err == nil {
		err = m.turn(ctx, b)
	}
	emit(stdout, map[string]any{"type": "system", "subtype": "init", "cwd": dir, "model": simModel,
		"mcp_servers": []mcpEntry{}, "skills": []string{}})
	if err != nil {
		emit(stdout, map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"result": strings.Join(m.did, " "), "errors": []string{err.Error()}, "total_cost_usd": 0})
		return 1
	}
	emit(stdout, map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"result": strings.Join(m.did, " "), "total_cost_usd": 0})
	return 0
}

// newMember is the real member: commands on PATH and the home's aimem MCP
// server as its .mcp.json starts it.
func newMember(dir string) *member {
	home := os.Getenv(agent.HomeEnv)
	if home == "" {
		home = dir
	}
	return &member{home: home, run: execRun, now: time.Now,
		mcp: func(ctx context.Context, tool string, args map[string]any) (string, error) {
			s, ok, err := aimemServer(home)
			if err != nil || !ok {
				return "", fmt.Errorf("the home's aimem MCP server: %v", err)
			}
			return callTool(ctx, s, tool, args)
		}}
}

func execRun(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if len(out) == 0 {
			out = stderr.Bytes()
		}
		return out, exit.ExitCode(), nil
	}
	return out, 0, err
}
