// Package automation runs a workflow script (docs/DESIGN-TURNS.md, section
// 10): Starlark, hermetic by construction, with the host API of 10.3 and
// nothing more. A script defines main(event); main returns None when the
// event is handled, or turn(kind, content) when judgement is needed. Every
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
	// maxSource bounds a script.
	maxSource = 256 << 10
)

// PR is the pull request as the host's facts hold it (4.1).
type PR struct {
	Number    int64
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

// Check is a check at the head, with a failed job's log excerpt.
type Check struct {
	Name       string
	Conclusion string
	Excerpt    string
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

// RunResult is a command's exit status and its output.
type RunResult struct {
	Exit   int
	Output string
}

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
}

// Event is what a run covers: the kinds and the IDs of its events.
type Event struct {
	Kinds []string
	IDs   []string
}

// Turn is a request for a model turn: its kind and its content.
type Turn struct {
	Kind    string
	Content string
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
		"kinds": stringList(ev.Kinds),
		"ids":   stringList(ev.IDs),
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
	return nil, fmt.Errorf("%w: main returned %s; it returns None or turn(kind, content)", ErrScript, out.Type())
}

// runner holds a run's host and allow list for the builtins.
type runner struct {
	ctx   context.Context
	host  Host
	allow AllowList
}

// predeclared is the host API of 10.3: exactly these names beside
// Starlark's universe, which holds no file, network, clock or environment.
func (r *runner) predeclared() starlark.StringDict {
	return starlark.StringDict{
		"pr":      starlark.NewBuiltin("pr", r.pr),
		"attempt": starlark.NewBuiltin("attempt", r.attempt),
		"run":     starlark.NewBuiltin("run", r.run),
		"step":    starlark.NewBuiltin("step", r.step),
		"comment": starlark.NewBuiltin("comment", r.comment),
		"turn":    starlark.NewBuiltin("turn", turnBuiltin),
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
		body, _ := bound(rv.Body, MaxText)
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
		excerpt, _ := bound(c.Excerpt, MaxExcerpt)
		checks = append(checks, frozenStruct(starlark.StringDict{
			"name":       starlark.String(c.Name),
			"conclusion": starlark.String(c.Conclusion),
			"excerpt":    starlark.String(excerpt),
		}))
	}
	return frozenStruct(starlark.StringDict{
		"number":    starlark.MakeInt64(p.Number),
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
	out, _ := bound(res.Output, MaxRunOutput)
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

// turnBuiltin is turn(kind, content): a value, with no side effect.
func turnBuiltin(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var kind, content string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 2, &kind, &content); err != nil {
		return nil, err
	}
	if !stepKind(kind) {
		return nil, fmt.Errorf("turn: %q is not a turn kind: lowercase letters, digits and '_'", kind)
	}
	if len(content) > MaxText {
		return nil, fmt.Errorf("turn: the content is %d bytes, more than %d", len(content), MaxText)
	}
	return &turnValue{t: Turn{Kind: kind, Content: content}}, nil
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

// bound is s cut to at most n bytes on a rune boundary, and whether it was
// cut.
func bound(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s, true
}
