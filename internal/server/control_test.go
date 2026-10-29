package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

func (e *coordEnv) teamState(t *testing.T) map[string]opapi.MemberState {
	t.Helper()
	var st opapi.TeamState
	if got := e.adminCall(t, http.MethodGet, opapi.TeamStatePath, e.opToken, url.Values{"id": {e.team.ID}}, nil, &st); got.status != http.StatusOK ||
		st.TeamID != e.team.ID || st.TeamName != e.team.Name {
		t.Fatalf("team state: %d %s", got.status, got.raw)
	}
	out := map[string]opapi.MemberState{}
	for _, m := range st.Members {
		out[m.AgentID] = m
	}
	return out
}

type pendingReply struct {
	Pending []struct {
		ID string `json:"id"`
	} `json:"pending"`
	Paused *opapi.Pause `json:"paused"`
}

func (e *coordEnv) pending(t *testing.T, m member) pendingReply {
	t.Helper()
	got := e.get(t, m.token, InboxPendingPath)
	var p pendingReply
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.raw), &p) != nil {
		t.Fatalf("pending: %d %s", got.status, got.raw)
	}
	return p
}

// The operator pauses the team over the API: every member's pending read
// lists nothing and names the pause, and its session status does too; a
// nudge then waits in the inbox. On the resume the pending read lists it,
// and the team state shows each member's derived state.
func TestControlRoutes(t *testing.T) {
	e := setupCoordination(t)
	// The worker's wake reads the pending list: it is waiting. The others
	// were heard from only when they entered.
	e.pending(t, e.worker)
	states := e.teamState(t)
	if s := states[e.worker.agent.ID]; s.State != opapi.StateWaiting || s.Session == nil || s.Session.ID != e.worker.sess.ID ||
		s.Session.LastWaitedAt.IsZero() || s.Role != "worker" || s.Label != e.worker.agent.Label {
		t.Fatalf("the waiting worker: %+v", s)
	}
	if s := states[e.lead.agent.ID]; s.State != opapi.StateWorking || !s.Session.LastWaitedAt.IsZero() {
		t.Fatalf("the coordinator, heard from at entry: %+v", s)
	}

	var res opapi.PauseResult
	if got := e.adminCall(t, http.MethodPost, opapi.TeamPausePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID, Reason: "upgrading the agents"}, &res); got.status != http.StatusOK ||
		!res.Changed || res.Paused == nil || res.Paused.Reason != "upgrading the agents" {
		t.Fatalf("team pause: %d %s", got.status, got.raw)
	}
	var n opapi.Nudge
	if got := e.adminCall(t, http.MethodPost, opapi.MemberNudgePath, e.opToken, nil,
		opapi.NudgeRequest{TeamID: e.team.ID, AgentID: e.worker.agent.ID, Note: "aicrew-agent is upgraded"}, &n); got.status != http.StatusOK ||
		n.MessageID == "" || n.AgentID != e.worker.agent.ID {
		t.Fatalf("nudge: %d %s", got.status, got.raw)
	}
	for _, m := range []member{e.lead, e.worker, e.indep} {
		if p := e.pending(t, m); len(p.Pending) != 0 || p.Paused == nil || p.Paused.Reason != "upgrading the agents" {
			t.Fatalf("%s's pending read while paused: %+v", m.agent.Label, p)
		}
	}
	got := e.get(t, e.worker.token, SessionPath)
	if got.status != http.StatusOK || !strings.Contains(got.raw, `"paused":{"team_id":"`+e.team.ID) {
		t.Fatalf("the session status names the pause: %s", got.raw)
	}
	states = e.teamState(t)
	for id, s := range states {
		if s.State != opapi.StatePaused {
			t.Fatalf("%s while the team is paused: %+v", id, s)
		}
	}
	if s := states[e.worker.agent.ID]; s.Unacknowledged != 1 {
		t.Fatalf("the worker's unacknowledged count: %+v", s)
	}

	res = opapi.PauseResult{}
	if got := e.adminCall(t, http.MethodPost, opapi.TeamResumePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID}, &res); got.status != http.StatusOK || !res.Changed || res.Paused != nil {
		t.Fatalf("team resume: %d %s", got.status, got.raw)
	}
	if p := e.pending(t, e.worker); len(p.Pending) != 1 || p.Pending[0].ID != n.MessageID || p.Paused != nil {
		t.Fatalf("the worker's pending read after the resume: %+v", p)
	}
	if p := e.pending(t, e.lead); len(p.Pending) != 0 {
		t.Fatalf("the nudge reached the coordinator: %+v", p)
	}
	got = e.get(t, e.worker.token, InboxPath+"?limit=10")
	if got.status != http.StatusOK || !strings.Contains(got.raw, `"operator":{"note":"aicrew-agent is upgraded","by":"operator:`) ||
		!strings.Contains(got.raw, "A note from the operator: aicrew-agent is upgraded") {
		t.Fatalf("the nudge as delivered: %s", got.raw)
	}

	// A member's pause holds that member alone.
	res = opapi.PauseResult{}
	if got := e.adminCall(t, http.MethodPost, opapi.MemberPausePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID, AgentID: e.lead.agent.ID}, &res); got.status != http.StatusOK ||
		!res.Changed || res.Paused.AgentID != e.lead.agent.ID {
		t.Fatalf("member pause: %d %s", got.status, got.raw)
	}
	if p := e.pending(t, e.lead); p.Paused == nil || p.Paused.AgentID != e.lead.agent.ID {
		t.Fatalf("the paused coordinator: %+v", p)
	}
	if p := e.pending(t, e.worker); p.Paused != nil {
		t.Fatalf("the worker under the coordinator's pause: %+v", p)
	}
	states = e.teamState(t)
	if s := states[e.lead.agent.ID]; s.State != opapi.StatePaused || s.Paused == nil {
		t.Fatalf("the paused coordinator's state: %+v", s)
	}
	if s := states[e.worker.agent.ID]; s.State != opapi.StateWaiting {
		t.Fatalf("the worker's state: %+v", s)
	}
	if got := e.adminCall(t, http.MethodPost, opapi.MemberResumePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID, AgentID: e.lead.agent.ID}, &res); got.status != http.StatusOK || !res.Changed {
		t.Fatalf("member resume: %d %s", got.status, got.raw)
	}
}

// The control routes refuse a team route naming a member, a member route
// naming none, unknown targets and unknown fields.
func TestControlRouteRefusals(t *testing.T) {
	e := setupCoordination(t)
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.TeamPausePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID, AgentID: e.worker.agent.ID}, nil), http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.MemberPausePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID}, nil), http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.MemberPausePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID, AgentID: "nobody"}, nil), http.StatusNotFound, opapi.CodeNotFound)
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.TeamResumePath, e.opToken, nil,
		opapi.PauseRequest{TeamID: e.team.ID, Reason: "why"}, nil), http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.MemberNudgePath, e.opToken, nil,
		map[string]string{"team_id": e.team.ID, "agent_id": e.worker.agent.ID, "note": "x", "extra": "y"}, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.MemberNudgePath, e.opToken, nil,
		opapi.NudgeRequest{TeamID: e.team.ID, AgentID: e.worker.agent.ID}, nil), http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.TeamStatePath, e.opToken, nil, nil, nil), http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.TeamStatePath, e.opToken, url.Values{"id": {"nope"}}, nil, nil),
		http.StatusNotFound, opapi.CodeNotFound)
	if p := e.pending(t, e.worker); len(p.Pending) != 0 || p.Paused != nil {
		t.Fatalf("a refused call changed something: %+v", p)
	}
}

// The liveness the service keeps follows each session's calls, and forgets
// the oldest once full.
func TestLiveness(t *testing.T) {
	var l liveness
	now := time.Now()
	l.saw("", true, now)
	if len(l.seen) != 0 {
		t.Fatal("an empty session ID was recorded")
	}
	l.saw("s1", true, now)
	l.saw("s1", false, now.Add(time.Minute))
	if v := l.of("s1"); !v.waited.Equal(now) || !v.seen.Equal(now.Add(time.Minute)) {
		t.Fatalf("s1: %+v", v)
	}
	old := now.Add(-2 * store.WorkingWindow)
	l.seen = map[string]sessionSeen{}
	for i := 0; i < maxLiveness; i++ {
		l.seen["old-"+time.Duration(i).String()] = sessionSeen{seen: old}
	}
	l.saw("fresh", false, now)
	if len(l.seen) != 1 || l.of("fresh").seen.IsZero() {
		t.Fatalf("a full liveness kept %d sessions", len(l.seen))
	}
}

// GET /v1/admin/team-states reads every team's state, or one team's, for the
// operator and an architect credential, with the open attempt's last change
// a worker's blocked report and its pull request's wait for the external
// review;
// the operator's own team state route stays the operator's, and an unknown
// parameter or team is refused.
func TestTeamStatesRoute(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	// The attempt's pull request asks for the external review: the state
	// shows the wait, not yet overdue.
	obs := map[string]any{"host": "git.example.test", "repository": "crew/project-example", "number": 7,
		"url": "https://git.example.test/crew/project-example/pulls/7", "state": "open", "head": "work/task-1",
		"head_commit": "0123456789abcdef0123456789abcdef01234567", "labels": []string{"review-this"}, "checks": "pending",
		"comments": 0}
	if got := e.call(t, e.worker.token, PullsPath, "", obs); got.status != http.StatusOK {
		t.Fatalf("the pull request: %d %s", got.status, got.raw)
	}
	// The worker reports its attempt blocked: the state shows the reason.
	ctx := context.Background()
	b, err := e.store.AuthenticateSessionToken(ctx, e.worker.token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateWork(ctx, e.worker.caller, e.aimem, "block-1", id, store.WorkUpdate{SessionID: b.SessionID,
		Generation: b.Generation, Intent: store.IntentBlock, Detail: "Waiting on the schema decision."}); err != nil {
		t.Fatalf("block: %v", err)
	}
	var cred opapi.ArchitectCredential
	if got := e.adminCall(t, http.MethodPost, opapi.ArchitectCredentialsPath, e.opToken, nil,
		opapi.ArchitectCredentialRequest{Label: "planning"}, &cred); got.status != http.StatusCreated {
		t.Fatalf("issue: %d %s", got.status, got.raw)
	}
	for _, bearer := range []string{e.opToken, cred.Bearer} {
		for _, q := range []url.Values{nil, {"team": {e.team.ID}}} {
			var list []opapi.TeamState
			got := e.adminCall(t, http.MethodGet, opapi.TeamStatesPath, bearer, q, nil, &list)
			if got.status != http.StatusOK || len(list) != 1 || list[0].TeamID != e.team.ID || len(list[0].Members) == 0 {
				t.Fatalf("team states (%v): %d %s", q, got.status, got.raw)
			}
			found := false
			for _, m := range list[0].Members {
				if m.AgentID == e.worker.agent.ID {
					found = m.OpenAttempt != nil && m.OpenAttempt.ID == id && !m.OpenAttempt.UpdatedAt.IsZero() && !m.Stalled &&
						m.OpenAttempt.Block != nil && m.OpenAttempt.Block.Reason == "Waiting on the schema decision." &&
						!m.OpenAttempt.Block.Since.IsZero() && m.OpenAttempt.HubBlock == nil &&
						m.OpenAttempt.ReviewWait != nil && m.OpenAttempt.ReviewWait.Pull == 7 && !m.OpenAttempt.ReviewWait.Overdue &&
						m.OpenAttempt.ReviewWait.Head == "0123456789abcdef0123456789abcdef01234567" && !m.OpenAttempt.ReviewWait.Since.IsZero()
				}
			}
			if !found {
				t.Fatalf("the worker's open attempt: %s", got.raw)
			}
		}
	}
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.TeamStatePath, cred.Bearer, url.Values{"id": {e.team.ID}}, nil, nil),
		http.StatusUnauthorized, opapi.CodeUnauthorized)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.TeamStatesPath, e.opToken, url.Values{"team": {"no-such-team"}}, nil, nil),
		http.StatusNotFound, opapi.CodeNotFound)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.TeamStatesPath, e.opToken, url.Values{"id": {e.team.ID}}, nil, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
}
