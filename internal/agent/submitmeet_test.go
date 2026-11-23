package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/store"
)

// submitThrough is the member's own submit of attempt id through the
// launcher's channel, as `aicrew-agent step work --intent submit` sends it.
func (s *stepEnv) submitThrough(t *testing.T, answers *[]byte, id, link string) StepAnswer {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"intent": "submit", "detail": link})
	return s.call(t, answers, StepCall{Op: "work", AttemptID: id, TaskID: "task-1", Body: body})
}

// submittedOnce fails unless aicrewd holds exactly one submitted result of
// attempt id, with link.
func (s *stepEnv) submittedOnce(t *testing.T, id, link string) {
	t.Helper()
	rs, err := s.store.AttemptResults(context.Background(), id)
	if err != nil || len(rs) != 1 || !strings.Contains(string(mustJSON(t, rs[0])), link) {
		t.Fatalf("the attempt's results: %+v, %v", rs, err)
	}
}

// gatesGreenRun is a run of the release's gates-green at attempt id, for
// submitResult, as the launcher starts one.
func gatesGreenRun(id string) *liveRun {
	return &liveRun{id: "run-gates", attempt: id, script: "gates-green.star", point: automation.PointGatesGreen}
}

const (
	submitLink = "https://git.example.test/crew/project-t/pulls/4"
	otherLink  = "https://git.example.test/crew/project-t/pulls/5"
)

// The member's submit after its launcher's settled submit of the same
// attempt (01a206f8-24d0). The refusal is the store's own phase check
// through the real handler; the answer is clean only for the link this
// launcher's record shows, and the attempt is submitted once.
func TestMemberSubmitMeetsASettledSubmit(t *testing.T) {
	var answers []byte
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	s.serve(t)
	id := s.claimedThrough(t, &answers)
	// The launcher's gates-green submit goes through the same driver.
	if r, err := s.submitStep(t, id, submitLink); err != nil || r.Outcome != "committed" {
		t.Fatalf("the launcher's submit: %+v %v", r, err)
	}
	if got := s.d.submittedRef(id); got != submitLink {
		t.Fatalf("the record after the committed submit: %q", got)
	}

	ans := s.submitThrough(t, &answers, id, submitLink)
	if !ans.OK || ans.Status != StepDone || ans.Error != nil ||
		!strings.Contains(ans.NextAction, "is submitted already, with the result "+submitLink+": there is nothing to submit") ||
		!strings.Contains(ans.NextAction, "a stop included, comes as its own message") {
		t.Fatalf("the same link: %+v", ans)
	}
	var res struct {
		Submitted bool   `json:"submitted"`
		Ref       string `json:"ref"`
	}
	if err := json.Unmarshal(ans.Result, &res); err != nil || !res.Submitted || res.Ref != submitLink {
		t.Fatalf("the same link's result: %s", ans.Result)
	}

	// Another link: the refusal stands, with the fact.
	ans = s.submitThrough(t, &answers, id, otherLink)
	if ans.OK || ans.Status != StepRefused || ans.Error == nil || ans.Error.Code != "attempt_state" ||
		!strings.HasSuffix(ans.Error.Message, " Attempt "+id+" is submitted already; this launcher's record shows the result "+
			submitLink+".") {
		t.Fatalf("another link: %+v", ans)
	}
	plain := strings.TrimSuffix(ans.Error.Message, " Attempt "+id+" is submitted already; this launcher's record shows the result "+
		submitLink+".")

	// No record, a malformed one and one of another attempt: the refusal,
	// with "no record".
	rec := filepath.Join(submitsDir(s.cfg.Home), id+".json")
	noRecord := plain + " Attempt " + id + " is submitted already; this launcher has no record of a submit."
	for name, content := range map[string]string{
		"none":            "",
		"malformed":       "{not json",
		"unknown field":   `{"version":1,"attempt_id":"` + id + `","ref":"` + submitLink + `","token":"x"}`,
		"another version": `{"version":2,"attempt_id":"` + id + `","ref":"` + submitLink + `"}`,
		"another attempt": `{"version":1,"attempt_id":"att-other","ref":"` + submitLink + `"}`,
	} {
		os.Remove(rec)
		if content != "" {
			if err := os.WriteFile(rec, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ans = s.submitThrough(t, &answers, id, submitLink)
		if ans.OK || ans.Status != StepRefused || ans.Error.Code != "attempt_state" || ans.Error.Message != noRecord {
			t.Fatalf("%s: %+v", name, ans)
		}
	}
	s.submittedOnce(t, id, submitLink)
}

// The member's submit while its launcher's submit of the same attempt is in
// flight is told so, never that the attempt is submitted. When that submit
// then settles as not committed, nothing says the attempt is submitted, no
// record is written, and the member's next submit is its own, as before.
func TestMemberSubmitMeetsASubmitInFlight(t *testing.T) {
	var answers []byte
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	s.serve(t)
	id := s.claimedThrough(t, &answers)
	// The launcher's submit has begun on aicrewd and is not settled. The
	// task moves before its update is read, so it will settle as not
	// committed.
	s.setFault(t, "mcp", "bump")
	s.d.crash = func(p string) bool { return p == "begun" }
	if _, err := s.submitStep(t, id, submitLink); !errors.Is(err, errCrashed) {
		t.Fatalf("the crash: %v", err)
	}
	s.d.crash = nil
	key := s.d.submitInFlight(id)
	if key == "" {
		t.Fatal("no submit in flight")
	}

	ans := s.submitThrough(t, &answers, id, submitLink)
	var res struct {
		RequestKey string `json:"request_key"`
	}
	if !ans.OK || ans.Status != StepPending || ans.Error != nil || json.Unmarshal(ans.Result, &res) != nil ||
		res.RequestKey != key || !strings.Contains(ans.NextAction, "This launcher is submitting attempt "+id+" already") ||
		!strings.Contains(ans.NextAction, "until that message comes, it is not") ||
		strings.Contains(ans.NextAction, "is submitted already") {
		t.Fatalf("a submit in flight: %+v", ans)
	}

	// The launcher's submit settles as not committed.
	ans = s.call(t, &answers, StepCall{Op: "recover"})
	var rs []StepResult
	if err := json.Unmarshal(ans.Result, &rs); err != nil || len(rs) != 1 || !rs[0].Settled || rs[0].Outcome != "not_committed" {
		t.Fatalf("the launcher's submit: %+v", ans)
	}
	if s.d.submitInFlight(id) != "" || s.d.submittedRef(id) != "" {
		t.Fatal("a record after a submit that did not commit")
	}
	if rs, _ := s.store.AttemptResults(context.Background(), id); len(rs) != 0 {
		t.Fatalf("results after a submit that did not commit: %+v", rs)
	}
	// The member's next submit is its own step on aicrewd, with a request key
	// of its own, not an answer about another submit. (The fake task moved
	// for good with the bump, so how that step settles here is the fake's.)
	ans = s.submitThrough(t, &answers, id, submitLink)
	if r := stepResult(t, ans); r.RequestKey == "" || r.RequestKey == key || r.Operation != "update" ||
		strings.Contains(ans.NextAction, "This launcher is submitting") || strings.Contains(ans.NextAction, "submitted already") ||
		(ans.Error != nil && strings.Contains(ans.Error.Message, "submitted already")) {
		t.Fatalf("the member's own submit afterwards: %+v", ans)
	}
}

// gates-green's submit after the member's settled or in-flight submit of
// the same attempt ends "nothing to do": the run is not failed, so it wakes
// nobody, and the attempt is submitted once.
func TestGatesGreenMeetsTheMembersSubmit(t *testing.T) {
	ctx := context.Background()
	for _, inFlight := range []bool{false, true} {
		var answers []byte
		s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
		srv := s.serve(t)
		id := s.claimedThrough(t, &answers)
		want := "the attempt is submitted already; nothing to do"
		if inFlight {
			s.d.crash = func(p string) bool { return p == "begun" }
			if _, err := s.submitStep(t, id, submitLink); !errors.Is(err, errCrashed) {
				t.Fatalf("the crash: %v", err)
			}
			s.d.crash = nil
			want = "a submit of the attempt is in flight already (request key " + s.d.submitInFlight(id) + "); nothing to do"
		} else if ans := s.submitThrough(t, &answers, id, submitLink); ans.Status != StepDone {
			t.Fatalf("the member's submit: %+v", ans)
		}
		run := gatesGreenRun(id)
		if err := srv.submitResult(ctx, run, submitLink); err != nil {
			t.Fatalf("in flight %v: gates-green's submit: %v", inFlight, err)
		}
		if n := len(run.steps); n == 0 || run.steps[n-1].Kind != stepHostSubmit || run.steps[n-1].Text != want {
			t.Fatalf("in flight %v: the run's steps: %+v", inFlight, run.steps)
		}
		if inFlight {
			if ans := s.call(t, &answers, StepCall{Op: "recover"}); ans.Status != StepDone {
				t.Fatalf("recovering the member's submit: %+v", ans)
			}
		}
		s.submittedOnce(t, id, submitLink)
	}
}

// refuseSettle is the crew client with every settle refused with
// attempt_state, as aicrewd's settle route can answer (server/attempts.go,
// settle) when the attempt changed after the step's begin.
type refuseSettle struct{ StepAPI }

func (refuseSettle) SettleStep(context.Context, string, string, string, StepReport) (Settlement, error) {
	return Settlement{}, &Refusal{Code: "attempt_state", Message: "The attempt's state does not allow this step now."}
}

// A submit whose own begin went through and whose own settle is then
// refused with attempt_state met nothing: its own record, begun and not
// settled, is never taken for another submit. The member gets the refusal
// as it is, and gates-green's run fails as before, so it wakes the
// coordinators.
func TestSubmitRefusedAfterItsOwnBegin(t *testing.T) {
	ctx := context.Background()
	for _, side := range []string{"member", "gates-green"} {
		var answers []byte
		s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
		srv := s.serve(t)
		id := s.claimedThrough(t, &answers)
		s.d.Crew = refuseSettle{s.d.Crew}
		if side == "member" {
			ans := s.submitThrough(t, &answers, id, submitLink)
			if ans.Status != StepRefused || ans.Error == nil || ans.Error.Code != "attempt_state" ||
				ans.Error.Message != "The attempt's state does not allow this step now." {
				t.Fatalf("the member's answer: %+v %+v", ans, ans.Error)
			}
		} else {
			run := gatesGreenRun(id)
			err := srv.submitResult(ctx, run, submitLink)
			var ref *Refusal
			if !errors.As(err, &ref) || ref.Code != "attempt_state" {
				t.Fatalf("gates-green's submit: %v", err)
			}
			for _, st := range run.steps {
				if strings.Contains(st.Text, "nothing to do") {
					t.Fatalf("gates-green's run met its own submit: %+v", run.steps)
				}
			}
		}
		// The call's own record is the one a meeting would have taken.
		if s.d.submitInFlight(id) == "" {
			t.Fatalf("%s: no begun record of the call's own submit", side)
		}
	}
}

// A refusal for any other reason is the refusal as it is, on both sides:
// another code, an error that is no refusal, a status that cannot be read,
// another attempt, a state or phase other than running and submitted. The
// phase is the live status's: after a rework the record of the first
// submit makes nothing clean.
func TestSubmitMeetingFailsClosed(t *testing.T) {
	// drive marks the refusal of a step's own first begin; the same code
	// after the step's own begin is a bare refusal.
	refusal := &Refusal{Code: "attempt_state", Message: "The attempt's state does not allow this step now."}
	state := &firstBeginRefused{refusal}
	status := func(id, st, phase string) func() (memberStatus, error) {
		return func() (memberStatus, error) {
			return memberStatus{Attempt: &memberAttempt{ID: id, State: st, Phase: phase}}, nil
		}
	}
	recorded := func() string { return submitLink }
	cases := []struct {
		name   string
		err    error
		status func() (memberStatus, error)
	}{
		{"another code", &firstBeginRefused{&Refusal{Code: "attempt_forbidden"}}, status("att-1", "running", "submitted")},
		{"after its own begin", refusal, status("att-1", "running", "submitted")},
		{"no refusal", errors.New("aicrewd did not answer"), status("att-1", "running", "submitted")},
		{"status unreadable", state, func() (memberStatus, error) { return memberStatus{}, errors.New("unavailable") }},
		{"no attempt", state, func() (memberStatus, error) { return memberStatus{}, nil }},
		{"another attempt", state, status("att-2", "running", "submitted")},
		{"blocked", state, status("att-1", "blocked", "submitted")},
		{"reworked", state, status("att-1", "running", "working")},
		{"accepted", state, status("att-1", "running", "accepted")},
	}
	for _, c := range cases {
		if m, ok := meetSubmit(c.err, "", c.status, recorded, "att-1"); ok {
			t.Errorf("%s: met %+v", c.name, m)
		}
	}
	// A submit in flight decides only an attempt_state refusal.
	if _, ok := meetSubmit(&firstBeginRefused{&Refusal{Code: "attempt_forbidden"}}, "req-1", status("att-1", "running",
		"working"), recorded, "att-1"); ok {
		t.Error("another code with a submit in flight met it")
	}
	// A refusal after the call's own begin never meets its own record.
	if _, ok := meetSubmit(refusal, "req-1", status("att-1", "running", "submitted"), recorded, "att-1"); ok {
		t.Error("a refusal after the call's own begin met a submit in flight")
	}
	// The status is read once, and not at all for a submit in flight.
	reads := 0
	count := func() (memberStatus, error) { reads++; return status("att-1", "running", "submitted")() }
	if m, ok := meetSubmit(state, "", count, recorded, "att-1"); !ok || m.ref != submitLink || reads != 1 {
		t.Errorf("submitted: %+v %v, %d reads", m, ok, reads)
	}
	if m, ok := meetSubmit(state, "req-1", count, recorded, "att-1"); !ok || m.inFlight != "req-1" || reads != 1 {
		t.Errorf("in flight: %+v %v, %d reads", m, ok, reads)
	}
}

// After a rework the record of the first submit still names its link, but
// the live phase decides: a refused submit of that link is the refusal as
// it is, never "nothing to submit" for a result that is not submitted.
func TestSubmitRecordAfterARework(t *testing.T) {
	var answers []byte
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	srv := s.serve(t)
	id := s.claimedThrough(t, &answers)
	if r, err := s.submitStep(t, id, submitLink); err != nil || r.Outcome != "committed" {
		t.Fatalf("the first submit: %+v %v", r, err)
	}
	if _, err := s.store.ReviewResult(ctx, s.coord, "rework-"+id, id, store.ResultReview{SessionID: s.coordSess.ID,
		Generation: s.coordSess.Generation, ResultSeq: s.resultSeq(t, id), Decision: store.ReviewRework}); err != nil {
		t.Fatal(err)
	}
	if st, err := srv.Status(ctx); err != nil || st.Attempt == nil || st.Attempt.Phase == "submitted" {
		t.Fatalf("the status after the rework: %+v %v", st.Attempt, err)
	}
	if s.d.submittedRef(id) != submitLink {
		t.Fatal("the first submit's record is gone")
	}
	ans := s.submitThrough(t, &answers, id, submitLink)
	if ans.Error != nil && strings.Contains(ans.Error.Message, "submitted already") ||
		strings.Contains(ans.NextAction, "submitted already") {
		t.Fatalf("the submit after a rework read the old record: %+v", ans)
	}
	// The same refusal met through meetSubmit with the live status.
	if m, ok := meetSubmit(&Refusal{Code: "attempt_state"}, "", func() (memberStatus, error) { return srv.Status(ctx) },
		func() string { return s.d.submittedRef(id) }, id); ok {
		t.Fatalf("met after a rework: %+v", m)
	}
}

// submitStep runs the member's work submit of attempt id with link
// through the driver, as the work helper does for any intent.
func (s *stepEnv) submitStep(t *testing.T, id, link string) (StepResult, error) {
	t.Helper()
	return s.run(t, attemptsPath+"/"+id+"/work", map[string]string{"intent": "submit", "detail": link})
}

// Only a work submit of the attempt that has begun on aicrewd is in
// flight: a record not yet begun holds no pending step there, and another
// attempt's, or another intent's, is not this submit.
func TestSubmitInFlightReadsBegunSubmits(t *testing.T) {
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	body := func(intent string) []byte {
		b, _ := json.Marshal(map[string]string{"intent": intent, "detail": submitLink})
		return b
	}
	path := func(id string) string { return attemptsPath + "/" + id + "/work" }
	for i, c := range []struct {
		name  string
		p     pendingStep
		empty bool
	}{
		{"not begun", pendingStep{Request: StepRequest{Path: path("att-1"), Body: body("submit")}, Phase: PhaseBegin}, true},
		{"another attempt", pendingStep{Request: StepRequest{Path: path("att-2"), Body: body("submit")}, Phase: PhaseBegun,
			Step: &Step{RequestKey: "req-2"}}, true},
		{"another intent", pendingStep{Request: StepRequest{Path: path("att-1"), Body: body("block")}, Phase: PhaseBegun,
			Step: &Step{RequestKey: "req-3"}}, true},
		{"begun", pendingStep{Request: StepRequest{Path: path("att-1"), Body: body("submit")}, Phase: PhaseBegun,
			Step: &Step{RequestKey: "req-4"}}, false},
	} {
		p := c.p
		p.BeginKey = fmt.Sprintf("step-%032x", i)
		if err := s.d.save(&p); err != nil {
			t.Fatal(err)
		}
		got := s.d.submitInFlight("att-1")
		if (got == "") != c.empty || (!c.empty && got != "req-4") {
			t.Errorf("%s: in flight %q", c.name, got)
		}
		if err := s.d.drop(&p); err != nil {
			t.Fatal(err)
		}
	}
}

// The record holds the attempt and the link only, and is never written for
// a link that holds the home's forge credential, which the member's submit
// does not refuse by itself.
func TestSubmitRecordHoldsNoToken(t *testing.T) {
	var answers []byte
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	provisionTestForge(t, s.cfg.Home, "git.example.test", "member-forge-token")
	s.serve(t)
	id := s.claimedThrough(t, &answers)
	link := submitLink + "?access_token=member-forge-token"
	if r, err := s.submitStep(t, id, link); err != nil || r.Outcome != "committed" {
		t.Fatalf("the submit: %+v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(submitsDir(s.cfg.Home), id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a record of a link with the token: %v", err)
	}

	s = setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	s.serve(t)
	id = s.claimedThrough(t, &answers)
	if r, err := s.submitStep(t, id, submitLink); err != nil || r.Outcome != "committed" {
		t.Fatalf("the submit: %+v %v", r, err)
	}
	raw, err := os.ReadFile(filepath.Join(submitsDir(s.cfg.Home), id+".json"))
	if err != nil || strings.TrimSpace(string(raw)) != `{"version":1,"attempt_id":"`+id+`","ref":"`+submitLink+`"}` {
		t.Fatalf("the record: %s %v", raw, err)
	}
}
