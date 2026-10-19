// Package automation runs a workflow script (docs/DESIGN-TURNS.md, section
// 10): Starlark, hermetic by construction, with the host API of 10.3 and
// nothing more. A script defines main(event); main returns None when the
// event is handled, or turn(kind, content, conflict=None) when judgement is needed. Every
// action goes through the host: pr(), attempt(), run(cmd, args), step(kind,
// text) and comment(body). The script has no file access, no network, no
// clock and no environment; while loops and recursion are off; values are
// frozen between calls; and a run is bounded by an execution-step limit and
// a wall-clock cancel. The memory limit is the child process's (increment
// 2b), since the interpreter has none.
package automation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// HostAPIVersion is the version of the host API a script is written for
// (10.3); a script's header names it (increment 2c).
const HostAPIVersion = 1

// The limits of 10.3.
const (
	// MaxSteps bounds a run's execution steps.
	MaxSteps = 1_000_000
	// MaxWall bounds a run's wall-clock time, run() calls included.
	MaxWall = 2 * time.Minute
	// MaxRunOutput bounds what run() returns of a command's output.
	MaxRunOutput = 64 << 10
	// MaxText bounds a comment's body, a review's body in pr(), a turn's
	// content and a step's text.
	MaxText = 16 << 10
	// MaxExcerpt bounds a failed check's log excerpt in pr().
	MaxExcerpt = 8 << 10
	// MaxLabels bounds the labels one label() call adds or removes, and
	// MaxLabel each label's length.
	MaxLabels = 8
	MaxLabel  = 64
	// MaxURL bounds the result reference submit() names.
	MaxURL = 1 << 10
	// maxSource bounds a script.
	maxSource = 256 << 10
)

// PR is the pull request as the host's facts hold it (4.1).
type PR struct {
	Number    int64
	URL       string
	Head      string
	Base      string
	Branch    string
	Behind    bool
	Draft     bool
	Mergeable bool
	Labels    []string
	Reviews   []Review
	Checks    []Check
}

// Review is a review of the pull request: the local or the external one,
// with its head, verdict, findings and body.
type Review struct {
	Reviewer string // local or external
	Head     string
	Verdict  string
	Findings []string
	Body     string
}

// Check is a check at the head, with a failed job's log excerpt, and
// whether its failure is the runner's rather than the change's (the forge's
// own word, forge.RunnerFailure), so a script decides on a re-run without
// a list of each forge's words.
type Check struct {
	Name          string
	Conclusion    string
	Excerpt       string
	RunnerFailure bool
}

// Attempt is the attempt's state.
type Attempt struct {
	ID         string
	Phase      string
	TaskID     string
	TaskSize   string
	BaseCommit string
	SpentUSD   float64
	BudgetUSD  float64
}

// RunResult is a command's exit status and its output: what it wrote to
// its standard output and standard error together, as the host read it.
// The script sees the output as text (a Starlark string), bounded by
// Bound: invalid UTF-8 replaced, at most MaxRunOutput bytes, its start
// kept.
type RunResult struct {
	Exit   int
	Output string
}

// RerunResult is what rerun() did: the failed jobs it re-ran, by name, or
// why it re-ran none (a second call at the head, a forge with no re-run
// call, no failed job).
type RerunResult struct {
	Reran  bool
	Jobs   []string
	Reason string
}

// UpdateBaseResult is what update_base() did: the branch brought up to
// date with its base by a merge and pushed (Updated, from OldHead to
// NewHead), or why not: the branch is up to date, the merge conflicts
// (Conflict), the merge changes the patch identity (IdentityChanged), or
// the push was refused because the branch moved; Reason says which.
type UpdateBaseResult struct {
	Updated         bool      `json:"updated"`
	OldHead         string    `json:"old_head,omitempty"`
	NewHead         string    `json:"new_head,omitempty"`
	IdentityChanged bool      `json:"identity_changed,omitempty"`
	Conflict        *Conflict `json:"conflict,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

// Conflict is a conflict in the update of a branch from its base: the
// base, the conflicting files (at most MaxConflictFiles) and their hunks
// (at most MaxText). A turn carries it to the worker's action needed.
type Conflict struct {
	Base  string   `json:"base"`
	Files []string `json:"files"`
	Hunks string   `json:"hunks,omitempty"`
}

// MaxConflictFiles bounds the files a conflict names.
const MaxConflictFiles = 50

// Host is what a script reaches the world through. The launcher serves it
// over the step socket (increment 2b); tests use a fake.
type Host interface {
	PR(ctx context.Context) (PR, error)
	Attempt(ctx context.Context) (Attempt, error)
	// Run runs cmd with args, no shell, in the attempt's worktree. The
	// runner has checked it against the allow list.
	Run(ctx context.Context, cmd string, args []string) (RunResult, error)
	Step(ctx context.Context, kind, text string) error
	Comment(ctx context.Context, body string) error
	// The pull request's transitions of 4.2 (increment 2d-c), each with
	// the member's forge credential, which the script never holds.
	// Label removes, then adds, labels on the pull request. Ready drops
	// its draft state and prefix. Submit asks for the attempt's result to
	// be submitted, the holder's work step with url as the result
	// reference, once the script ends without failing. Rerun re-runs the
	// failed jobs at the head once; a second call at the head, and a forge
	// without a re-run call, re-run nothing and say so.
	Label(ctx context.Context, add, remove []string) error
	Ready(ctx context.Context) error
	Submit(ctx context.Context, url string) error
	Rerun(ctx context.Context) (RerunResult, error)
	// UpdateBase brings the pull request's branch up to date with its
	// base (increment 2e, docs/DESIGN-TURNS.md 10.4): a merge of the base
	// pushed without force when the patch identity is unchanged, with the
	// delta note as a comment; a conflict, a changed identity, a refused
	// push and an up-to-date branch are results, not errors. The git and
	// the credential are the launcher's; the script holds neither.
	UpdateBase(ctx context.Context) (UpdateBaseResult, error)
}

// Event is what a run covers: the kinds and the IDs of its events. At an
// extension point the kind is the point's name, and the ID names the pull
// request and its head. BaseOnly says the head is a base-only update of
// the one before it: its patch identity against the merge base is
// unchanged (docs/DESIGN-TURNS.md, 4.1 and 10.4), so the reviews stand.
type Event struct {
	Kinds    []string
	IDs      []string
	BaseOnly bool
	// Change is what changed at the pull request, for a run on
	// forge.changed (12.2); nil at a point, where the script sees None.
	Change *ForgeChange
}

// ForgeChange is what the launcher's observation of a pull request changed
// against the one before it: the facts a script subscribed to
// forge.changed acts on without reading the forge again.
type ForgeChange struct {
	// HeadMoved says the head commit is another than at the observation
	// before.
	HeadMoved bool
	// State and Checks are the new state and the new checks conclusion
	// when they changed, "" when they did not.
	State  string
	Checks string
	// LabelsAdded and LabelsRemoved are the label names that appeared and
	// went.
	LabelsAdded   []string
	LabelsRemoved []string
	// Comments are the comments new since the content was last read,
	// oldest first, at most MaxChangeComments, each body bounded by
	// MaxChangeComment; none when no content came with the observation.
	Comments []ChangedComment
	// ReviewFailures counts the observations at this head, this one
	// included, that carried the external reviewer's failure: its
	// could-not-run notice or the hands-review-failed label added. 0 when
	// this observation carried none.
	ReviewFailures int
}

// ChangedComment is one new comment: its author, kind (comment, review or
// inline), body and notice (forge.NoticeReviewFailed for the external
// reviewer's failure notice, "" otherwise).
type ChangedComment struct {
	Author string
	Kind   string
	Body   string
	Notice string
}

// The bounds of a change's comments.
const (
	MaxChangeComments = 10
	MaxChangeComment  = 4 << 10
)

// Turn is a request for a model turn: its kind and its content, and the
// base update's conflict when the turn carries one (turn(kind, content,
// conflict=...)).
type Turn struct {
	Kind     string    `json:"kind"`
	Content  string    `json:"content"`
	Conflict *Conflict `json:"conflict,omitempty"`
}

// Limits bound a run; zero values take MaxSteps and MaxWall.
type Limits struct {
	Steps uint64
	Wall  time.Duration
}

// Script is one script to run: its file name (for messages), its source,
// the host, and the allow list run() is checked against.
type Script struct {
	Name   string
	Source []byte
	Host   Host
	Allow  AllowList
	Limits Limits
}

// The errors a caller tells apart.
var (
	// ErrScript is a script that failed: fail(), a runtime error, a
	// limit, a refused command or a host error.
	ErrScript = errors.New("the script failed")
	// ErrInvalid is a script that does not load: a syntax error, an
	// undefined name, no main(event).
	ErrInvalid = errors.New("the script is not valid")
)

// fileOptions are the dialect: no while, no recursion, no reassigned
// globals, no top-level control flow, sets allowed.
var fileOptions = &syntax.FileOptions{Set: true}

// Run loads the script and calls its main(event). It returns the turn the
// script asks for, or nil when the event is handled.
func Run(ctx context.Context, s Script, ev Event) (*Turn, error) {
	if len(s.Source) > maxSource {
		return nil, fmt.Errorf("%w: the script is %d bytes, more than %d", ErrInvalid, len(s.Source), maxSource)
	}
	if !utf8.Valid(s.Source) {
		return nil, fmt.Errorf("%w: the script is not UTF-8", ErrInvalid)
	}
	lim := s.Limits
	if lim.Steps == 0 {
		lim.Steps = MaxSteps
	}
	if lim.Wall == 0 {
		lim.Wall = MaxWall
	}
	ctx, cancel := context.WithTimeout(ctx, lim.Wall)
	defer cancel()
	thread := &starlark.Thread{
		Name:  s.Name,
		Print: func(*starlark.Thread, string) {}, // print reaches nothing
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, errors.New("load is not available to a workflow script")
		},
	}
	thread.SetMaxExecutionSteps(lim.Steps)
	stop := context.AfterFunc(ctx, func() { thread.Cancel("the run's wall-clock limit") })
	defer stop()

	r := &runner{ctx: ctx, host: s.Host, allow: s.Allow}
	globals, err := starlark.ExecFileOptions(fileOptions, thread, s.Name, s.Source, r.predeclared())
	if err != nil {
		// A syntax error, an undefined name or a construct the dialect
		// leaves out (while, a global reassigned) is the script's text;
		// anything else happened while it ran.
		var serr syntax.Error
		var rerr resolve.ErrorList
		if errors.As(err, &serr) || errors.As(err, &rerr) {
			return nil, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		return nil, fmt.Errorf("%w: %s", ErrScript, err)
	}
	mainFn, ok := globals["main"].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("%w: it defines no main(event)", ErrInvalid)
	}
	event := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"kinds":     stringList(ev.Kinds),
		"ids":       stringList(ev.IDs),
		"base_only": starlark.Bool(ev.BaseOnly),
		"change":    changeValue(ev.Change),
	})
	event.Freeze()
	out, err := starlark.Call(thread, mainFn, starlark.Tuple{event}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrScript, err)
	}
	switch v := out.(type) {
	case starlark.NoneType:
		return nil, nil
	case *turnValue:
		t := v.t
		return &t, nil
	}
	return nil, fmt.Errorf("%w: main returned %s; it returns None or turn(kind, content, conflict=None)", ErrScript, out.Type())
}

// runner holds a run's host and allow list for the builtins.
type runner struct {
	ctx   context.Context
	host  Host
	allow AllowList
}

// predeclared is the host API of 10.3 with the transitions of 4.2
// (increment 2d-c): exactly these names beside Starlark's universe, which
// holds no file, network, clock or environment. A script that names one
// of them on a release without it fails at load with the name.
func (r *runner) predeclared() starlark.StringDict {
	return starlark.StringDict{
		"pr":      starlark.NewBuiltin("pr", r.pr),
		"attempt": starlark.NewBuiltin("attempt", r.attempt),
		"run":     starlark.NewBuiltin("run", r.run),
		"step":    starlark.NewBuiltin("step", r.step),
		"comment": starlark.NewBuiltin("comment", r.comment),
		"label":   starlark.NewBuiltin("label", r.label),
		"ready":   starlark.NewBuiltin("ready", r.ready),
		"submit":  starlark.NewBuiltin("submit", r.submit),
		"rerun":   starlark.NewBuiltin("rerun", r.rerun),
		"turn":    starlark.NewBuiltin("turn", turnBuiltin),

		"update_base": starlark.NewBuiltin("update_base", r.updateBase),
	}
}

func (r *runner) pr(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return nil, err
	}
	p, err := r.host.PR(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("pr: %w", err)
	}
	reviews := make([]starlark.Value, 0, len(p.Reviews))
	for _, rv := range p.Reviews {
		body, _ := Bound(rv.Body, MaxText)
		reviews = append(reviews, frozenStruct(starlark.StringDict{
			"reviewer": starlark.String(rv.Reviewer),
			"head":     starlark.String(rv.Head),
			"verdict":  starlark.String(rv.Verdict),
			"findings": stringList(rv.Findings),
			"body":     starlark.String(body),
		}))
	}
	checks := make([]starlark.Value, 0, len(p.Checks))
	for _, c := range p.Checks {
		excerpt, _ := Bound(c.Excerpt, MaxExcerpt)
		checks = append(checks, frozenStruct(starlark.StringDict{
			"name":           starlark.String(c.Name),
			"conclusion":     starlark.String(c.Conclusion),
			"excerpt":        starlark.String(excerpt),
			"runner_failure": starlark.Bool(c.RunnerFailure),
		}))
	}
	return frozenStruct(starlark.StringDict{
		"number":    starlark.MakeInt64(p.Number),
		"url":       starlark.String(p.URL),
		"head":      starlark.String(p.Head),
		"base":      starlark.String(p.Base),
		"branch":    starlark.String(p.Branch),
		"behind":    starlark.Bool(p.Behind),
		"draft":     starlark.Bool(p.Draft),
		"mergeable": starlark.Bool(p.Mergeable),
		"labels":    stringList(p.Labels),
		"reviews":   frozenList(reviews),
		"checks":    frozenList(checks),
	}), nil
}

func (r *runner) attempt(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return nil, err
	}
	a, err := r.host.Attempt(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("attempt: %w", err)
	}
	return frozenStruct(starlark.StringDict{
		"id":          starlark.String(a.ID),
		"phase":       starlark.String(a.Phase),
		"task":        frozenStruct(starlark.StringDict{"id": starlark.String(a.TaskID), "size": starlark.String(a.TaskSize)}),
		"base_commit": starlark.String(a.BaseCommit),
		"spent_usd":   starlark.Float(a.SpentUSD),
		"budget_usd":  starlark.Float(a.BudgetUSD),
	}), nil
}

func (r *runner) run(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var cmd string
	var list *starlark.List
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 2, &cmd, &list); err != nil {
		return nil, err
	}
	argv := make([]string, 0, list.Len())
	for i := range list.Len() {
		s, ok := starlark.AsString(list.Index(i))
		if !ok {
			return nil, fmt.Errorf("run: argument %d is %s, not a string", i, list.Index(i).Type())
		}
		argv = append(argv, s)
	}
	if err := r.allow.Admit(cmd, argv); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	res, err := r.host.Run(r.ctx, cmd, argv)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", cmd, err)
	}
	out, _ := Bound(res.Output, MaxRunOutput)
	return frozenStruct(starlark.StringDict{
		"exit":   starlark.MakeInt(res.Exit),
		"output": starlark.String(out),
	}), nil
}

func (r *runner) step(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var kind, text string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 2, &kind, &text); err != nil {
		return nil, err
	}
	if !stepKind(kind) {
		return nil, fmt.Errorf("step: %q is not a step kind: lowercase letters, digits and '_'", kind)
	}
	if len(text) > MaxText {
		return nil, fmt.Errorf("step: the text is %d bytes, more than %d", len(text), MaxText)
	}
	if err := r.host.Step(r.ctx, kind, text); err != nil {
		return nil, fmt.Errorf("step: %w", err)
	}
	return starlark.None, nil
}

func (r *runner) comment(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var body string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &body); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" || len(body) > MaxText {
		return nil, fmt.Errorf("comment: the body is empty or more than %d bytes", MaxText)
	}
	if err := r.host.Comment(r.ctx, body); err != nil {
		return nil, fmt.Errorf("comment: %w", err)
	}
	return starlark.None, nil
}

// label(add=[...], remove=[...]): labels removed, then added, on the pull
// request. Each list holds at most MaxLabels names of at most MaxLabel
// bytes, on one line; at least one name in all.
func (r *runner) label(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var addList, removeList *starlark.List
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "add?", &addList, "remove?", &removeList); err != nil {
		return nil, err
	}
	add, err := labelNames("add", addList)
	if err != nil {
		return nil, err
	}
	remove, err := labelNames("remove", removeList)
	if err != nil {
		return nil, err
	}
	if len(add)+len(remove) == 0 {
		return nil, errors.New("label: name a label to add or to remove")
	}
	if err := r.host.Label(r.ctx, add, remove); err != nil {
		return nil, fmt.Errorf("label: %w", err)
	}
	return starlark.None, nil
}

// labelNames is the names in list, checked.
func labelNames(what string, list *starlark.List) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	if list.Len() > MaxLabels {
		return nil, fmt.Errorf("label: %s names %d labels, more than %d", what, list.Len(), MaxLabels)
	}
	out := make([]string, 0, list.Len())
	for i := range list.Len() {
		s, ok := starlark.AsString(list.Index(i))
		if !ok {
			return nil, fmt.Errorf("label: %s[%d] is %s, not a string", what, i, list.Index(i).Type())
		}
		if !LabelName(s) {
			return nil, fmt.Errorf("label: %q is not a label name: 1 to %d bytes on one line", s, MaxLabel)
		}
		out = append(out, s)
	}
	return out, nil
}

// LabelName reports whether s can name a label: 1 to MaxLabel bytes of
// valid UTF-8, none of them a control character.
func LabelName(s string) bool {
	if s == "" || len(s) > MaxLabel || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ready(): the pull request leaves its draft state, its draft prefix
// dropped.
func (r *runner) ready(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return nil, err
	}
	if err := r.host.Ready(r.ctx); err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	return starlark.None, nil
}

// submit(url): the attempt's result is submitted with url as its
// reference, the pull request's URL as pr().url gives it, once main
// returns without failing.
func (r *runner) submit(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var u string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &u); err != nil {
		return nil, err
	}
	if !ResultURL(u) {
		return nil, fmt.Errorf("submit: %q is not a result reference: an http(s) URL of at most %d bytes", u, MaxURL)
	}
	if err := r.host.Submit(r.ctx, u); err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	return starlark.None, nil
}

// ResultURL reports whether u can be a result reference: an http(s) URL on
// one line, at most MaxURL bytes.
func ResultURL(u string) bool {
	if len(u) > MaxURL || !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) {
		return false
	}
	return !strings.ContainsFunc(u, func(r rune) bool { return r <= 0x20 || r == 0x7f })
}

// rerun(): the failed jobs at the head are re-run, once per head. The
// result says whether they were (reran), which (jobs), or why not
// (reason).
func (r *runner) rerun(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return nil, err
	}
	res, err := r.host.Rerun(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("rerun: %w", err)
	}
	return frozenStruct(starlark.StringDict{
		"reran":  starlark.Bool(res.Reran),
		"jobs":   stringList(res.Jobs),
		"reason": starlark.String(res.Reason),
	}), nil
}

// update_base(): the branch is brought up to date with its base by the
// launcher (Host.UpdateBase). The result says whether it was (updated,
// old_head, new_head), or why not: conflict (None, or a struct with base,
// files and hunks that turn(..., conflict=) takes), identity_changed, and
// reason in words.
func (r *runner) updateBase(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return nil, err
	}
	res, err := r.host.UpdateBase(r.ctx)
	if err != nil {
		return nil, fmt.Errorf("update_base: %w", err)
	}
	return frozenStruct(starlark.StringDict{
		"updated":          starlark.Bool(res.Updated),
		"old_head":         starlark.String(res.OldHead),
		"new_head":         starlark.String(res.NewHead),
		"identity_changed": starlark.Bool(res.IdentityChanged),
		"conflict":         conflictValue(res.Conflict),
		"reason":           starlark.String(res.Reason),
	}), nil
}

// conflictValue is a conflict as a script sees it: None, or a struct.
func conflictValue(c *Conflict) starlark.Value {
	if c == nil {
		return starlark.None
	}
	return frozenStruct(starlark.StringDict{
		"base":  starlark.String(c.Base),
		"files": stringList(c.Files),
		"hunks": starlark.String(c.Hunks),
	})
}

// turnBuiltin is turn(kind, content, conflict=None): a value, with no side
// effect. conflict is the struct update_base() returned, carried to the
// worker with the turn.
func turnBuiltin(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var kind, content string
	var conflict starlark.Value = starlark.None
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "kind", &kind, "content", &content, "conflict?", &conflict); err != nil {
		return nil, err
	}
	if !stepKind(kind) {
		return nil, fmt.Errorf("turn: %q is not a turn kind: lowercase letters, digits and '_'", kind)
	}
	if len(content) > MaxText {
		return nil, fmt.Errorf("turn: the content is %d bytes, more than %d", len(content), MaxText)
	}
	c, err := conflictOf(conflict)
	if err != nil {
		return nil, fmt.Errorf("turn: %w", err)
	}
	return &turnValue{t: Turn{Kind: kind, Content: content, Conflict: c}}, nil
}

// conflictOf reads a turn's conflict argument: None, or the struct
// update_base() returned, within its bounds.
func conflictOf(v starlark.Value) (*Conflict, error) {
	if v == starlark.None {
		return nil, nil
	}
	st, ok := v.(*starlarkstruct.Struct)
	if !ok {
		return nil, fmt.Errorf("conflict is None or the conflict update_base() returned, not %s", v.Type())
	}
	var c Conflict
	for name, into := range map[string]*string{"base": &c.Base, "hunks": &c.Hunks} {
		a, err := st.Attr(name)
		if err != nil {
			return nil, fmt.Errorf("conflict has no %s: %w", name, err)
		}
		s, ok := starlark.AsString(a)
		if !ok {
			return nil, fmt.Errorf("conflict's %s is not a string", name)
		}
		*into = s
	}
	files, err := st.Attr("files")
	if err != nil {
		return nil, fmt.Errorf("conflict has no files: %w", err)
	}
	it, ok := files.(starlark.Iterable)
	if !ok {
		return nil, errors.New("conflict's files is not a list")
	}
	for f := range starlark.Elements(it) {
		s, ok := starlark.AsString(f)
		if !ok {
			return nil, errors.New("conflict's files holds a value that is not a string")
		}
		c.Files = append(c.Files, s)
	}
	if c.Base == "" || len(c.Files) == 0 || len(c.Files) > MaxConflictFiles || len(c.Hunks) > MaxText {
		return nil, fmt.Errorf("conflict names a base, 1 to %d files and at most %d bytes of hunks", MaxConflictFiles, MaxText)
	}
	return &c, nil
}

// turnValue is turn(...)'s value: immutable, so frozen from the start.
type turnValue struct{ t Turn }

func (v *turnValue) String() string        { return fmt.Sprintf("turn(%q)", v.t.Kind) }
func (v *turnValue) Type() string          { return "turn" }
func (v *turnValue) Freeze()               {}
func (v *turnValue) Truth() starlark.Bool  { return starlark.True }
func (v *turnValue) Hash() (uint32, error) { return 0, errors.New("unhashable: turn") }

func stepKind(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func stringList(ss []string) *starlark.List {
	vs := make([]starlark.Value, 0, len(ss))
	for _, s := range ss {
		vs = append(vs, starlark.String(s))
	}
	return frozenList(vs)
}

// changeValue is event.change: None at a point, else the change as a
// struct whose comments are structs too.
func changeValue(c *ForgeChange) starlark.Value {
	if c == nil {
		return starlark.None
	}
	var comments []starlark.Value
	for _, cm := range c.Comments {
		comments = append(comments, frozenStruct(starlark.StringDict{
			"author": starlark.String(cm.Author),
			"kind":   starlark.String(cm.Kind),
			"body":   starlark.String(cm.Body),
			"notice": starlark.String(cm.Notice),
		}))
	}
	return frozenStruct(starlark.StringDict{
		"head_moved":      starlark.Bool(c.HeadMoved),
		"state":           starlark.String(c.State),
		"checks":          starlark.String(c.Checks),
		"labels_added":    stringList(c.LabelsAdded),
		"labels_removed":  stringList(c.LabelsRemoved),
		"comments":        frozenList(comments),
		"review_failures": starlark.MakeInt(c.ReviewFailures),
	})
}

func frozenList(vs []starlark.Value) *starlark.List {
	l := starlark.NewList(vs)
	l.Freeze()
	return l
}

func frozenStruct(d starlark.StringDict) *starlarkstruct.Struct {
	s := starlarkstruct.FromStringDict(starlarkstruct.Default, d)
	s.Freeze()
	return s
}

// Bound is s as text of at most n bytes, and whether it was cut. A host
// result is text: each run of bytes that is not UTF-8 becomes one U+FFFD
// first, so an invalid byte anywhere never costs the valid text around it,
// and the text is then cut on a rune boundary, keeping its start.
func Bound(s string, n int) (string, bool) {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	// The cut may split the last rune: drop its bytes. A U+FFFD of the
	// text decodes whole, three bytes long, and stays.
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s, true
}
