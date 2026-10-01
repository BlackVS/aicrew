package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var proofShape = regexp.MustCompile(`^acp1_[A-Za-z0-9_-]{43}$`)

// step is one mutation the fake aimem was asked to commit, with the fact its
// proof answered at that moment: when aimem would verify it.
type step struct {
	op    ReservationOp
	key   string
	proof string
	fact  Fact
}

type proofLog struct {
	mu    sync.Mutex
	steps []step
}

func (l *proofLog) last(t *testing.T) step {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.steps) == 0 {
		t.Fatal("no step reached aimem")
	}
	return l.steps[len(l.steps)-1]
}

// watchProofs asks the store for each proof's fact as the fake aimem is
// about to commit its step. The test agents are linked on the tasks' hub,
// as the members of a real team are.
func watchProofs(t *testing.T, e execTeam) *proofLog {
	t.Helper()
	if _, err := e.s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-a'`); err != nil {
		t.Fatal(err)
	}
	l := &proofLog{}
	e.port.onMutate = func(op ReservationOp, req ReservationRequest) {
		st := step{op: op, key: req.RequestKey, proof: req.CoordinationProof}
		if req.CoordinationProof != "" {
			f, err := e.s.CoordinationFact(context.Background(), req.CoordinationProof, "hub-a")
			if err != nil {
				t.Errorf("fact for %s: %v", op, err)
			}
			st.fact = f
		}
		l.mu.Lock()
		l.steps = append(l.steps, st)
		l.mu.Unlock()
	}
	return l
}

func memberOf(m crewMember, role Role) FactMember {
	return FactMember{UserID: "user-" + m.agent.ID, AgentID: m.agent.ID, TeamID: m.sess.TeamID, Role: role,
		SessionID: m.sess.ID, Generation: m.sess.Generation}
}

func inactive(t *testing.T, s *Store, proof, why string) {
	t.Helper()
	f, err := s.CoordinationFact(context.Background(), proof, "hub-a")
	if err != nil {
		t.Fatalf("%s: %v", why, err)
	}
	if f.Active || f != (Fact{}) {
		t.Fatalf("%s: the proof still answers %+v", why, f)
	}
}

// checkStep asserts the fact a step's proof answered while its intent was
// pending, and that the proof answers inactive once the step settled.
func checkStep(t *testing.T, s *Store, st step, want Fact) {
	t.Helper()
	if !proofShape.MatchString(st.proof) {
		t.Fatalf("%s carried %q, not a coordination proof", st.op, st.proof)
	}
	want.Active, want.Operation, want.RequestKeyDigest = true, st.op, requestKeyDigest(st.key)
	got := st.fact
	want.ExpiresAt = got.ExpiresAt
	if got.ExpiresAt.IsZero() || got.ExpiresAt.After(s.now().Add(ProofLifetime)) {
		t.Fatalf("%s: expires at %v", st.op, got.ExpiresAt)
	}
	if (got.Process == nil) != (want.Process == nil) || (got.Process != nil && *got.Process != *want.Process) ||
		(got.IntendedWorker == nil) != (want.IntendedWorker == nil) ||
		(got.IntendedWorker != nil && *got.IntendedWorker != *want.IntendedWorker) {
		t.Fatalf("%s: fact %+v, want %+v", st.op, got, want)
	}
	got.Process, got.IntendedWorker, want.Process, want.IntendedWorker = nil, nil, nil, nil
	if got != want {
		t.Fatalf("%s: fact\n%+v\nwant\n%+v", st.op, got, want)
	}
	inactive(t, s, st.proof, string(st.op)+" after it settled")
}

// Every proof-bearing step, driven through its real store operation, sends
// a proof whose fact names the acting member and the references aimem
// checks; an update sends none. Each proof ends with its step.
func TestCoordinationFactsFromRealTransitions(t *testing.T) {
	s, path := openTemp(t)
	e := newClaimTeam(t, s)
	log := watchProofs(t, e.execTeam)
	lead, builder := memberOf(e.lead, RoleCoordinator), memberOf(e.builder, RoleWorker)
	pin := testPin
	var proofs []string
	check := func(want Fact) {
		t.Helper()
		st := log.last(t)
		checkStep(t, s, st, want)
		proofs = append(proofs, st.proof)
	}

	a := e.offer(t, "offer", "task-1")
	check(Fact{Kind: FactOffer, Task: a.Task, Member: lead, OfferRef: a.offerRef(), Process: &pin,
		IntendedWorker: &FactWorker{UserID: "user-" + e.builder.agent.ID, AgentID: e.builder.agent.ID}})
	a = e.accept(t, "accept", a)
	check(Fact{Kind: FactAcceptedAttempt, Task: a.Task, Member: builder, OfferRef: a.offerRef(),
		AttemptRef: a.attemptRef(), Process: &pin})

	a = e.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
	if st := log.last(t); st.op != ReservationUpdate || st.proof != "" {
		t.Fatalf("a holder's update carried a proof: %+v", st)
	}
	if _, err := e.review(t, "review", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.finalize(t, "finalize", a, e.builder, 1, devDelivery("1")); err != nil {
		t.Fatal(err)
	}
	check(Fact{Kind: FactAcceptedForFinalization, Task: a.Task, Member: builder, AttemptRef: a.attemptRef(),
		EvidenceDigest: evidenceDigest(append(refsOf(devDelivery("1").Evidence),
			"aicrew attempt "+a.ID+" by member "+e.builder.agent.ID))})

	// An independent claim, finalized by the reviewing coordinator.
	c, err := e.claim(t, "claim", e.solo, "task-2")
	if err != nil {
		t.Fatal(err)
	}
	check(Fact{Kind: FactIndependentClaim, Task: c.Task, Member: memberOf(e.solo, RoleIndependent),
		AttemptRef: c.attemptRef(), Process: &pin})
	if _, err := e.soloWork(t, "solo-submit", c, IntentSubmit, "https://example.invalid/pull/2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.review(t, "solo-review", c, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.finalize(t, "lead-finalize", c, e.lead, 1, devDelivery("2")); err != nil {
		t.Fatal(err)
	}
	check(Fact{Kind: FactAcceptedForFinalization, Task: c.Task, Member: lead, AttemptRef: c.attemptRef(),
		EvidenceDigest: evidenceDigest(append(refsOf(devDelivery("2").Evidence),
			"aicrew attempt "+c.ID+" by member "+e.solo.agent.ID))})

	// An offer the coordinator withdraws was never accepted.
	w := e.offer(t, "offer-3", "task-3")
	proofs = append(proofs, log.last(t).proof)
	if _, err := e.release(t, "withdraw", w); err != nil {
		t.Fatal(err)
	}
	check(Fact{Kind: FactNeverAccepted, Task: w.Task, Member: lead, OfferRef: w.offerRef()})

	// A stopped attempt, released by its holder.
	r := e.accept(t, "accept-4", e.offer(t, "offer-4", "task-4"))
	if _, err := e.requestStop(t, "stop", r, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.confirmStop(t, "confirm", r); err != nil {
		t.Fatal(err)
	}
	if _, err := e.releaseStopped(t, "release", r, ReleaseReady, ""); err != nil {
		t.Fatal(err)
	}
	check(Fact{Kind: FactStopped, Task: r.Task, Member: builder, AttemptRef: r.attemptRef()})

	var live int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM coordination_proofs WHERE ended_at = ''`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("%d proofs outlived their steps (%v)", live, err)
	}
	// Only digests are stored: no proof appears anywhere in the database.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		for _, p := range proofs {
			if bytes.Contains(raw, []byte(p)) || bytes.Contains(raw, []byte(strings.TrimPrefix(p, proofPrefix))) {
				t.Fatalf("a proof is stored in %s", f)
			}
		}
	}
}

// pendingOffer leaves an offer's claim pending after a lost reply, and
// returns its proof, which still answers: the member may retry the same key.
func pendingOffer(t *testing.T) (execTeam, Attempt, string) {
	t.Helper()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	log := watchProofs(t, e)
	e.port.faults[ReservationClaim] = faultBeforeCommit
	if _, err := e.s.OfferTask(context.Background(), e.lead.caller, e.port, "offer", e.offerReq("task-1")); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("offer with a lost reply: %v", err)
	}
	a := mustState(t, s, mustOnlyAttempt(t, s), AttemptReconciling)
	st := log.last(t)
	f, err := s.CoordinationFact(context.Background(), st.proof, "hub-a")
	if err != nil || !f.Active || f.Kind != FactOffer {
		t.Fatalf("the pending offer's fact = %+v, %v", f, err)
	}
	return e, a, st.proof
}

func mustOnlyAttempt(t *testing.T, s *Store) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM attempts`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A proof answers inactive whenever its fact has stopped being true, with
// nothing that says why.
func TestCoordinationFactInactive(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(t *testing.T, e execTeam, a Attempt) string{
		"unknown proof": func(t *testing.T, e execTeam, a Attempt) string {
			return "acp1_" + strings.Repeat("A", 43)
		},
		"expired": func(t *testing.T, e execTeam, a Attempt) string {
			later := e.s.now().Add(ProofLifetime + time.Second)
			e.s.now = func() time.Time { return later }
			return ""
		},
		"settled": func(t *testing.T, e execTeam, a Attempt) string {
			if _, err := e.s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID); err != nil {
				t.Fatal(err)
			}
			mustState(t, e.s, a.ID, AttemptClosed)
			return ""
		},
		"coordinator resumed": func(t *testing.T, e execTeam, a Attempt) string {
			if _, err := e.s.ResumeSession(ctx, e.lead.caller, "resume", e.lead.sess.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"coordinator session ended": func(t *testing.T, e execTeam, a Attempt) string {
			if _, err := e.s.StopSession(ctx, operator(t), "stop", e.lead.sess.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"link changed": func(t *testing.T, e execTeam, a Attempt) string {
			if _, err := e.s.db.Exec(`UPDATE agents SET linked_token_id = 'another-token' WHERE id = ?`, e.lead.agent.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"membership removed": func(t *testing.T, e execTeam, a Attempt) string {
			if _, err := e.s.db.Exec(`UPDATE memberships SET removed = 1 WHERE team_id = ? AND agent_id = ?`, a.TeamID, e.lead.agent.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"role changed": func(t *testing.T, e execTeam, a Attempt) string {
			if _, err := e.s.db.Exec(`UPDATE memberships SET role = 'worker' WHERE team_id = ? AND agent_id = ?`, a.TeamID, e.lead.agent.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"intended worker unlinked": func(t *testing.T, e execTeam, a Attempt) string {
			if _, err := e.s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-b' WHERE id = ?`, e.builder.agent.ID); err != nil {
				t.Fatal(err)
			}
			return ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, a, proof := pendingOffer(t)
			if other := change(t, e, a); other != "" {
				proof = other
			}
			inactive(t, e.s, proof, name)
		})
	}
	t.Run("another hub", func(t *testing.T) {
		e, _, proof := pendingOffer(t)
		if f, err := e.s.CoordinationFact(ctx, proof, "hub-b"); err != nil || f.Active {
			t.Fatalf("asked by another hub: %+v, %v", f, err)
		}
	})
}

// A finalize answers only while the acceptance it relies on stands, and a
// transfer only for the offer's worker from the recorded coordinator
// generation.
func TestCoordinationFactKindRules(t *testing.T) {
	ctx := context.Background()
	t.Run("acceptance withdrawn", func(t *testing.T) {
		e, a := running(t)
		log := watchProofs(t, e)
		a = e.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
		if _, err := e.review(t, "review", a, e.lead, 1, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		e.port.faults[ReservationFinalize] = faultBeforeCommit
		if _, err := e.finalize(t, "finalize", a, e.builder, 1, devDelivery("1")); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("finalize with a lost reply: %v", err)
		}
		proof := log.last(t).proof
		if f, err := e.s.CoordinationFact(ctx, proof, "hub-a"); err != nil || !f.Active {
			t.Fatalf("the pending finalize's fact = %+v, %v", f, err)
		}
		if _, err := e.s.db.Exec(`UPDATE attempts SET accepted_result = 0 WHERE id = ?`, a.ID); err != nil {
			t.Fatal(err)
		}
		inactive(t, e.s, proof, "finalize without its acceptance")
	})
	t.Run("coordinator generation moved", func(t *testing.T) {
		e, a, proof := pendingOffer(t)
		if _, err := e.s.db.Exec(`UPDATE teams SET coordinator_generation = coordinator_generation + 1 WHERE id = ?`, a.TeamID); err != nil {
			t.Fatal(err)
		}
		inactive(t, e.s, proof, "offer after the coordinator generation moved")
	})
	t.Run("transfer by another worker", func(t *testing.T) {
		s, _ := openTemp(t)
		e := newExecTeam(t, s)
		log := watchProofs(t, e)
		a := e.offer(t, "offer", "task-1")
		e.port.faults[ReservationTransfer] = faultBeforeCommit
		if _, err := e.s.AcceptOffer(ctx, e.builder.caller, e.port, "accept", a.ID, e.acceptReq()); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("accept with a lost reply: %v", err)
		}
		proof := log.last(t).proof
		if f, err := s.CoordinationFact(ctx, proof, "hub-a"); err != nil || !f.Active || f.Kind != FactAcceptedAttempt {
			t.Fatalf("the pending transfer's fact = %+v, %v", f, err)
		}
		if _, err := s.db.Exec(`UPDATE attempts SET worker_agent_id = ? WHERE id = ?`, e.lead.agent.ID, a.ID); err != nil {
			t.Fatal(err)
		}
		inactive(t, s, proof, "transfer acted by someone other than the attempt's worker")
	})
}

// A refused step ends its proof like a committed one, and the attempt's next
// intent gets a new proof of its own; the refused one stays dead.
func TestRefusedStepEndsItsProof(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	log := watchProofs(t, e)
	a := e.offer(t, "offer", "task-1")
	e.port.refuse[ReservationTransfer] = fixtureRefusal(t, "stale_worker")
	var refusal *ReservationRefusal
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "accept-1", a.ID, e.acceptReq()); !errors.As(err, &refusal) {
		t.Fatalf("refused transfer: %v", err)
	}
	refused := log.last(t)
	if !refused.fact.Active || refused.fact.Kind != FactAcceptedAttempt {
		t.Fatalf("the refused transfer's fact before aimem answered: %+v", refused.fact)
	}
	inactive(t, s, refused.proof, "a refused step's proof")
	var ended string
	if err := s.db.QueryRow(`SELECT ended_at FROM coordination_proofs WHERE digest = ?`, secretDigest(refused.proof)).Scan(&ended); err != nil || ended == "" {
		t.Fatalf("the refused step's proof was not ended: %q, %v", ended, err)
	}

	a = e.accept(t, "accept-2", mustState(t, s, a.ID, AttemptOffered))
	second := log.last(t)
	if second.proof == refused.proof || second.key == refused.key {
		t.Fatal("the second intent reused the refused step's proof or key")
	}
	checkStep(t, s, second, Fact{Kind: FactAcceptedAttempt, Task: a.Task, Member: memberOf(e.builder, RoleWorker),
		OfferRef: a.offerRef(), AttemptRef: a.attemptRef(), Process: &testPin})
	inactive(t, s, refused.proof, "the refused proof after a later intent")
}

// Each condition holds on its own: every case below breaks exactly one of
// them and leaves the others true, so no other rule answers for it.
func TestCoordinationFactEachRuleAlone(t *testing.T) {
	ctx := context.Background()
	exec := func(t *testing.T, e execTeam, stmt string, args ...any) {
		t.Helper()
		if _, err := e.s.db.Exec(stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("intent superseded", func(t *testing.T) {
		e, a, proof := pendingOffer(t)
		exec(t, e, `UPDATE attempts SET pending_key = 'another-intent' WHERE id = ?`, a.ID)
		inactive(t, e.s, proof, "a proof for an intent that is no longer the pending one")
	})
	t.Run("proof ended", func(t *testing.T) {
		e, a, proof := pendingOffer(t)
		exec(t, e, `UPDATE coordination_proofs SET ended_at = ? WHERE attempt_id = ?`, formatTime(e.s.now()), a.ID)
		inactive(t, e.s, proof, "an ended proof")
	})
	t.Run("task on another hub", func(t *testing.T) {
		e, _, proof := pendingOffer(t)
		exec(t, e, `UPDATE agents SET linked_hub_id = 'hub-b'`)
		if f, err := e.s.CoordinationFact(ctx, proof, "hub-b"); err != nil || f.Active {
			t.Fatalf("asked by a hub the task is not on: %+v, %v", f, err)
		}
	})
	t.Run("session no longer active", func(t *testing.T) {
		e, _, proof := pendingOffer(t)
		exec(t, e, `UPDATE sessions SET state = 'stopped' WHERE id = ?`, e.lead.sess.ID)
		inactive(t, e.s, proof, "a stopped session at the same generation")
	})
	t.Run("worker resumed", func(t *testing.T) {
		s, _ := openTemp(t)
		e := newExecTeam(t, s)
		log := watchProofs(t, e)
		a := e.offer(t, "offer", "task-1")
		e.port.faults[ReservationTransfer] = faultBeforeCommit
		if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "accept", a.ID, e.acceptReq()); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("accept with a lost reply: %v", err)
		}
		proof := log.last(t).proof
		exec(t, e, `UPDATE sessions SET generation = generation + 1 WHERE id = ?`, e.builder.sess.ID)
		inactive(t, s, proof, "a transfer from a fenced worker generation")
	})
}

// Schema v14 adds the proof table to a populated v13 store and keeps every
// reference; an offer then issues a proof as usual.
func TestMigrationV14AddsCoordinationProofs(t *testing.T) {
	ctx := context.Background()
	s, path := populatedStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, path)
	dropDeliveryColumns(t, raw)
	for _, stmt := range []string{`DROP TABLE coordination_proofs`, `ALTER TABLE introspection_credentials DROP COLUMN operations`,
		`ALTER TABLE attempts DROP COLUMN process_verified_receipt`, `UPDATE schema_version SET version = 13`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open the v13 store: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := foreignKeyCheck(ctx, s2.db); err != nil {
		t.Fatal(err)
	}
	tm := mustTeam(t, s2, "t-after", "after-migration", projectA)
	e := execTeam{s: s2, tm: tm, lead: joinCrew(t, s2, tm.ID, "lead-2", RoleCoordinator),
		builder: joinCrew(t, s2, tm.ID, "builder-2", RoleWorker), port: newFakeReservations(t)}
	e.offer(t, "offer-after", "task-after")
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM coordination_proofs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("proofs after an offer = %d, %v", n, err)
	}
}

// A pin is recorded only in the hub selection's forms, which aimem compares
// byte for byte; a malformed one is refused before any proof exists.
func TestProcessPinForms(t *testing.T) {
	ok := testPin.Identity
	long := ok
	long.Repository = "https://" + strings.Repeat("r", 512-len("https://"))
	for name, p := range map[string]ProcessIdentity{"the test pin": ok, "a 512-byte repository": long,
		"ssh": {Repository: "ssh://git.example/p.git", Commit: ok.Commit, Manifest: "m.json"},
		"scp": {Repository: "git@git.example:team/p.git", Commit: ok.Commit, Manifest: "a/b/m.json"}} {
		if !ValidProcessIdentity(p) {
			t.Errorf("%s: refused %+v", name, p)
		}
	}
	bad := map[string]func(p *ProcessIdentity){
		"repository over 512 bytes": func(p *ProcessIdentity) { p.Repository = long.Repository + "r" },
		"plain http":                func(p *ProcessIdentity) { p.Repository = "http://git.example/p.git" },
		"no scheme":                 func(p *ProcessIdentity) { p.Repository = "github.com/example/process" },
		"leading dash":              func(p *ProcessIdentity) { p.Repository = "-uhttps://git.example/p.git" },
		"whitespace in repository":  func(p *ProcessIdentity) { p.Repository = "https://git.example/p .git" },
		"quote in repository":       func(p *ProcessIdentity) { p.Repository = `https://git.example/p".git` },
		"abbreviated commit":        func(p *ProcessIdentity) { p.Commit = "3f2a9c1" },
		"uppercase commit":          func(p *ProcessIdentity) { p.Commit = strings.ToUpper(ok.Commit) },
		"absolute manifest":         func(p *ProcessIdentity) { p.Manifest = "/process/manifest.json" },
		"manifest leaves":           func(p *ProcessIdentity) { p.Manifest = "../manifest.json" },
		"manifest is ..":            func(p *ProcessIdentity) { p.Manifest = ".." },
		"manifest is .":             func(p *ProcessIdentity) { p.Manifest = "." },
		"manifest not clean":        func(p *ProcessIdentity) { p.Manifest = "a//manifest.json" },
		"manifest backslash":        func(p *ProcessIdentity) { p.Manifest = `a\manifest.json` },
		"manifest over 256 bytes":   func(p *ProcessIdentity) { p.Manifest = strings.Repeat("m", 257) },
		"empty manifest":            func(p *ProcessIdentity) { p.Manifest = "" },
	}
	for name, change := range bad {
		p := ok
		change(&p)
		if ValidProcessIdentity(p) {
			t.Errorf("%s: accepted %+v", name, p)
		}
	}

	ctx := context.Background()
	s, _ := openTemp(t)
	e := newClaimTeam(t, s)
	req := e.offerReq("task-1")
	req.Process.Identity.Commit = "3f2a9c1"
	if _, err := s.OfferTask(ctx, e.lead.caller, e.port, "offer", req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an offer with a malformed pin: %v", err)
	}
	claim := e.claimReq(e.solo, "task-2")
	claim.Process.Identity.Manifest = "../manifest.json"
	if _, err := s.ClaimTask(ctx, e.solo.caller, e.port, "claim", claim); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a claim with a malformed pin: %v", err)
	}
	if n := count(t, s, "coordination_proofs"); n != 0 {
		t.Fatalf("%d proofs issued for refused pins", n)
	}
	req = e.offerReq("task-3")
	req.Process.Identity = long
	if _, err := s.OfferTask(ctx, e.lead.caller, e.port, "offer-long", req); err != nil {
		t.Fatalf("an offer with a 512-byte repository: %v", err)
	}
}

// A pin recorded in a malformed form, as by an older release or a damaged
// row, never reaches aimem: the fact answers inactive instead.
func TestMalformedRecordedPinAnswersInactive(t *testing.T) {
	e, a, proof := pendingOffer(t)
	if _, err := e.s.db.Exec(`UPDATE attempts SET process_commit = upper(process_commit) WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	inactive(t, e.s, proof, "a fact whose recorded pin is malformed")
}
