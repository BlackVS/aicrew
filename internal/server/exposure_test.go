package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	serverPath = "github.com/BlackVS/aicrew/internal/server"
	storePath  = "github.com/BlackVS/aicrew/internal/store"
)

// The routes agents and aimem reach call only the store operations that
// authenticate by proof, session token or peer credential: never one that
// trusts a caller it is given. The operator API (admin.go, the escalation
// routes of escalations.go, the control routes and team states read of
// control.go and the merges read of merges.go) alone calls the operator's store operations, only in
// functions handed the operator's caller, and gets that caller only inside
// the wrapper (adminRoute): it builds the operator's caller there after
// comparing the operator credential, and has the store authenticate an
// architect credential there and nowhere else.
//
// The guard resolves every identifier of the package through go/types, so an
// aliased or dot import of the store, a local variable or field holding the
// store, or a method value reaches the same check as s.store.Op(...).
var (
	// storeOps are the *store.Store operations every route may call.
	storeOps = map[string]bool{
		"AuthenticateIntrospection": true, "Introspect": true,
		"AuthenticateCoordination": true, "CoordinationFact": true,
		"IssueAgentChallenge": true, "EnterSession": true, "ResumeSessionWithProof": true,
		"RefreshHandle": true, "LeaveWithToken": true, "AuthenticateSessionToken": true,
		"BeginOfferWithToken": true, "BeginAcceptWithToken": true, "DeclineWithToken": true,
		"BeginWithdrawWithToken": true, "SettleWithToken": true, "BeginClaimWithToken": true,
		"RequestStopWithToken": true, "ConfirmStopWithToken": true, "BeginStopReleaseWithToken": true,
		"ReviewWithToken": true, "ConfirmDeliveryWithToken": true, "BeginFinalizeWithToken": true,
		"BeginWorkWithToken": true, "SupersedeWorkWithToken": true,
		// The member's inbox, as the token's session (pilot G1).
		"ReadInboxWithToken": true, "AckWithToken": true, "PendingInboxWithToken": true, "PendingWithToken": true,
		// The pause in force for the member whose session token the route
		// has just authenticated, for its session status (C1).
		"PauseFor": true,
		// The team's open attempts, for its coordinator (the store refuses any other role).
		"OpenAttemptsWithToken": true,
		// The member's open escalations, before it raises another.
		"OpenEscalationsWithToken": true,
		// Invitation redemption authenticates by the invitation code, which
		// the store checks inside every command (1a81-3).
		"BeginRedemption": true, "CompleteRedemption": true,
		// The team's grants, read from its hub live at offer and claim and
		// refreshed periodically: aicrewd's own reads, recorded as its
		// reconciler (354c-1a).
		"GetTeam": true, "ListTeams": true, "RecordTeamGrants": true,
		// The member's requirements and capability reports, by its session
		// token; and the worker's last report, a planning fact the offer's
		// capability check reads (354c-2). The team's projects, every grant,
		// which the member records in its home (3a4b).
		"RequirementsWithToken": true, "TeamProjectsWithToken": true, "ReportCapabilitiesWithToken": true, "TeamCapabilitiesWithToken": true,
		"AgentCapabilities": true,
		// The coordinator's escalation, by its session token (5598).
		"RaiseEscalationWithToken": true,
		// The hubs' board feeds, read and recorded as aicrewd's reconciler
		// (5570); the store refuses every other caller.
		"BoardCursor": true, "RecordBoardPage": true, "ResetBoardCursor": true,
		// The worker's observation of its running attempt's pull request, by
		// its session token (forge events), and its launcher's report of an
		// automation run at one of its extension points (increment 2d-a).
		"ObservePullWithToken": true, "RecordAutomationRunWithToken": true,
		// The worker's push and read of its attempt's handoff (increment 3c).
		"PutHandoffWithToken": true, "HandoffWithToken": true,
		// The member's report of its turns' costs and its read of their totals.
		"ReportTurnWithToken": true, "TurnTotalsWithToken": true,
		// The event queues' delivery, as aicrewd's reconciler (increment
		// 1b); the store refuses every other caller.
		"DeliverEvents": true,
		// The member's open attempt as its worker, in its session status,
		// for the turn loop's continuation (A3).
		"MemberAttemptFor": true,
	}
	// operatorOps are the *store.Store operations only the operator API's
	// functions handed the operator's caller may call.
	operatorOps = map[string]bool{
		"ListIntrospectionCredentials": true, "IssueIntrospectionCredential": true, "RevokeIntrospectionCredential": true,
		"ListTeams": true, "CreateTeam": true, "GetTeam": true, "ListMembers": true, "SetTeamHub": true,
		"RenameTeam": true, "SetTeamToolchain": true, "ListInvitations": true, "IssueInvitation": true, "GetInvitation": true,
		"RevokeInvitation": true, "RecordTeamRegistration": true,
		// The escalations, which the operator's caller or an architect's
		// reads and answers, and the architect credentials (5598).
		"ListEscalations": true, "GetEscalation": true, "AnswerEscalation": true,
		"ListArchitectCredentials": true, "IssueArchitectCredential": true, "RevokeArchitectCredential": true,
		// The team's state, pauses and nudges (C1), and the team states the
		// operator's caller or an architect's reads (d56a).
		"GetTeamState": true, "SetPause": true, "Nudge": true, "TeamStates": true,
		// The pull requests waiting for the human merge, which the
		// operator's caller or an architect's reads (C3).
		"PendingMerges": true,
	}
	// wrapperOps are the *store.Store operations only the wrapper calls.
	wrapperOps = map[string]bool{"AuthenticateArchitect": true}
	// storeHelpers are the store's other functions and methods the service
	// may use anywhere: values, checks and derivations that reach no stored
	// state and grant no caller, and the reconciler's caller, which only
	// aicrewd's own reads and records use and the store limits to them. A
	// function missing here is refused until a review adds it.
	storeHelpers = map[string]bool{
		"store.NewSecret": true, "store.Secret.Reveal": true, "store.GenerateInvitationCode": true,
		"store.IntrospectionCredential.Active": true, "store.ArchitectCredential.Active": true,
		"store.TeamGrant.Valid": true, "store.CapabilityGap": true, "store.DeriveState": true,
		"store.LooksStalled": true,
		// The configured verifier, which redeems aimem proof receipts.
		"store.Verifier.Redeem":  true,
		"store.ReconcilerCaller": true,
	}
)

// operatorFiles hold the operator API.
var operatorFiles = map[string]bool{"admin.go": true, "escalations.go": true, "control.go": true, "merges.go": true}

func TestExposureGuard(t *testing.T) {
	g := loadServer(t)
	report := g.check()
	for _, v := range report.violations {
		t.Error(v)
	}
	if report.operatorCallers != 1 {
		t.Fatalf("the operator wrapper builds the operator's caller %d times, want 1", report.operatorCallers)
	}
	if report.storeCalls < len(storeOps) {
		t.Fatalf("found only %d uses of the store's operations; the guard is not looking at the service", report.storeCalls)
	}
}

// The fixtures get past a guard that matches selector shapes; the type-based
// guard rejects each of them.
func TestExposureGuardRejectsBypasses(t *testing.T) {
	g := loadServer(t, filepath.Join("testdata", "exposure", "aliased.go"), filepath.Join("testdata", "exposure", "dotimport.go"))
	report := g.check()
	got := map[string]bool{}
	for _, v := range report.violations {
		if strings.Contains(v.pos.Filename, "testdata") {
			got[v.fn+" "+v.object] = true
		} else {
			t.Errorf("a violation outside the fixtures: %s", v)
		}
	}
	want := []string{
		"bypassAliasedCaller store.OperatorCaller",
		"bypassAliasedAgent store.AgentCaller",
		"bypassLocalStore (*store.Store).RenameTeam",
		"bypassLocalStore (*store.Store).IssueInvitation",
		"bypassMethodValue (*store.Store).AuthenticateArchitect",
		"bypassUnknownFunction store.ValidProcessIdentity",
		"bypassDotImport store.OperatorCaller",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("the guard let %q through", w)
		}
		delete(got, w)
	}
	for v := range got {
		t.Errorf("unexpected violation in the fixtures: %s", v)
	}
	// The fixtures construct no caller inside the wrapper.
	if report.operatorCallers != 1 {
		t.Fatalf("the operator wrapper builds the operator's caller %d times, want 1", report.operatorCallers)
	}
}

type guarded struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
}

type violation struct {
	pos    token.Position
	fn     string // the enclosing function, or "" at package level
	object string // the store function, as "store.F" or "(*store.T).M"
	why    string
}

func (v violation) String() string {
	return fmt.Sprintf("%s: %s %s in %s", v.pos, v.object, v.why, v.fnName())
}

func (v violation) fnName() string {
	if v.fn == "" {
		return "a package-level declaration"
	}
	return v.fn
}

type guardReport struct {
	violations      []violation
	operatorCallers int
	storeCalls      int
}

// loadServer type-checks the package's non-test files, and the extra files,
// against the export data the go tool builds for its imports.
func loadServer(t *testing.T, extra ...string) guarded {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the guard needs the go tool: %v", err)
	}
	cmd := exec.Command(gobin, "list", "-export", "-deps", "-json=ImportPath,Export,GoFiles,Error", ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	type listed struct {
		ImportPath string
		Export     string
		GoFiles    []string
		Error      *struct{ Err string }
	}
	exports := map[string]string{}
	var self *listed
	for dec := json.NewDecoder(strings.NewReader(string(out))); ; {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("go list output: %v", err)
		}
		if p.Error != nil {
			t.Fatalf("go list %s: %s", p.ImportPath, p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
		if p.ImportPath == serverPath {
			self = &p
		}
	}
	if self == nil {
		t.Fatalf("go list did not list %s", serverPath)
	}
	// A file the build leaves out on this platform would escape the guard.
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, name := range all {
		if !strings.HasSuffix(name, "_test.go") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	built := append([]string(nil), self.GoFiles...)
	sort.Strings(built)
	if strings.Join(names, " ") != strings.Join(built, " ") {
		t.Fatalf("the build uses %v of the package's files %v; the guard must see every file", built, names)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range append(names, extra...) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	lookup := func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	if _, err := conf.Check(serverPath, fset, files, info); err != nil {
		t.Fatalf("type-checking the service: %v", err)
	}
	return guarded{fset: fset, files: files, info: info}
}

// check applies the rule above to every identifier the package uses.
func (g guarded) check() guardReport {
	var r guardReport
	for _, f := range g.files {
		file := filepath.Base(g.fset.Position(f.Pos()).Filename)
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			name := ""
			if fn != nil {
				name = fn.Name.Name
			}
			inWrapper := file == "admin.go" && name == "adminRoute"
			// Only the wrapper and the operator API's functions it hands
			// the operator's caller to may reach the operator's operations.
			operatorFn := operatorFiles[file] && fn != nil && (inWrapper || g.takesCaller(fn))
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				obj, ok := g.info.Uses[id].(*types.Func)
				if !ok || obj.Pkg() == nil || obj.Pkg().Path() != storePath {
					return true
				}
				v := violation{pos: g.fset.Position(id.Pos()), fn: name, object: storeName(obj)}
				recv := receiverName(obj)
				switch {
				case recv == "Store":
					r.storeCalls++
					op := obj.Name()
					if wrapperOps[op] {
						if !inWrapper {
							v.why = "outside the operator wrapper"
						}
					} else if !storeOps[op] && !(operatorFn && operatorOps[op]) {
						v.why = "is not the service's to call"
					}
				case recv == "" && obj.Name() == "OperatorCaller" && inWrapper:
					r.operatorCallers++
				case recv == "" && (obj.Name() == "OperatorCaller" || obj.Name() == "AgentCaller"):
					v.why = "constructs a trusted caller"
				case !storeHelpers[v.object]:
					v.why = "is not a helper the guard knows"
				}
				if v.why != "" {
					r.violations = append(r.violations, v)
				}
				return true
			})
		}
	}
	return r
}

// takesCaller reports whether fn has a store.Caller parameter.
func (g guarded) takesCaller(fn *ast.FuncDecl) bool {
	obj, ok := g.info.Defs[fn.Name].(*types.Func)
	if !ok {
		return false
	}
	for param := range obj.Type().(*types.Signature).Params().Variables() {
		if named, ok := param.Type().(*types.Named); ok &&
			named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == storePath && named.Obj().Name() == "Caller" {
			return true
		}
	}
	return false
}

// receiverName is the name of fn's receiver type, or "" for a function.
func receiverName(fn *types.Func) string {
	recv := fn.Type().(*types.Signature).Recv()
	if recv == nil {
		return ""
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name()
	}
	return t.String()
}

// storeName names fn as "store.F", "store.T.M" or "(*store.T).M".
func storeName(fn *types.Func) string {
	recv := fn.Type().(*types.Signature).Recv()
	if recv == nil {
		return "store." + fn.Name()
	}
	if _, ok := recv.Type().(*types.Pointer); ok {
		return "(*store." + receiverName(fn) + ")." + fn.Name()
	}
	return "store." + receiverName(fn) + "." + fn.Name()
}
