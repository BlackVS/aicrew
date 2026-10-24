package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

// The operator's control of a team (docs/DESIGN-CONTROL-PLANE.md, section
// 4.2 and decision D12; increment C1): the team's state, pause and resume of
// the team or a member, and a nudge. The store holds the pauses and writes
// the nudge's message; this file adds what the service sees of each
// member's session, from which a member's state follows until the runner
// reports it (B2).

func (s *Server) registerControl() {
	s.handle(http.MethodGet, opapi.TeamStatePath, s.operator("team.state", s.teamState))
	s.handle(http.MethodGet, opapi.TeamStatesPath, s.adminRoute("team.states", true, s.teamStates))
	s.handle(http.MethodPost, opapi.TeamPausePath, s.operator("team.pause", s.pauseRoute(false, true)))
	s.handle(http.MethodPost, opapi.TeamResumePath, s.operator("team.resume", s.pauseRoute(false, false)))
	s.handle(http.MethodPost, opapi.MemberPausePath, s.operator("member.pause", s.pauseRoute(true, true)))
	s.handle(http.MethodPost, opapi.MemberResumePath, s.operator("member.resume", s.pauseRoute(true, false)))
	s.handle(http.MethodPost, opapi.MemberNudgePath, s.operator("member.nudge", s.nudge))
}

// liveness is what the service saw of members' sessions since it started:
// when each was last heard from on a member route, and when its wake last
// read the pending list. It is kept in memory: a restart forgets it, and a
// session is then heard from again at its next call.
type liveness struct {
	mu   sync.Mutex
	seen map[string]sessionSeen
}

type sessionSeen struct{ seen, waited time.Time }

// maxLiveness bounds the sessions remembered; past it, those not heard from
// within the working window are forgotten.
const maxLiveness = 4096

// saw records a session heard from at now, and its wake's pending read when
// waited is set.
func (l *liveness) saw(sessionID string, waited bool, now time.Time) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]sessionSeen{}
	}
	if len(l.seen) >= maxLiveness {
		for id, v := range l.seen {
			if now.Sub(v.seen) > store.WorkingWindow {
				delete(l.seen, id)
			}
		}
	}
	v := l.seen[sessionID]
	v.seen = now
	if waited {
		v.waited = now
	}
	l.seen[sessionID] = v
}

func (l *liveness) of(sessionID string) sessionSeen {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[sessionID]
}

func (s *Server) teamState(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	id := r.URL.Query().Get("id")
	if id == "" {
		adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "the query names the team: ?id=TEAM")
		return opapi.CodeInvalid, ""
	}
	st, err := s.store.GetTeamState(r.Context(), op, id)
	if err != nil {
		return adminFail(w, err), ""
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, s.teamStateOf(st, now))
	return "ok", st.TeamID
}

// teamStates is GET /v1/admin/team-states[?team=ID], for the operator and
// an architect credential.
func (s *Server) teamStates(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	q := r.URL.Query()
	for name := range q {
		if name != "team" {
			adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "the only parameter is team")
			return opapi.CodeInvalid, ""
		}
	}
	list, err := s.store.TeamStates(r.Context(), op, q.Get("team"))
	if err != nil {
		return adminFail(w, err), ""
	}
	now := time.Now()
	out := make([]opapi.TeamState, 0, len(list))
	for _, st := range list {
		out = append(out, s.teamStateOf(st, now))
	}
	writeJSON(w, http.StatusOK, out)
	return "ok", ""
}

// teamStateOf converts the store's team state and derives each member's
// state from its pause and what the service saw of its session.
func (s *Server) teamStateOf(st store.TeamState, now time.Time) opapi.TeamState {
	out := opapi.TeamState{TeamID: st.TeamID, TeamName: st.TeamName, Paused: pauseOf(st.Paused),
		Members: []opapi.MemberState{}, ReadAt: now}
	for _, m := range st.Members {
		v := opapi.MemberState{AgentID: m.AgentID, Label: m.Label, Role: string(m.Role),
			Profile: opapi.MemberProfile{Model: m.Profile.Model, Client: m.Profile.Client, ClientVersion: m.Profile.ClientVersion},
			Paused:  pauseOf(m.Paused), Unacknowledged: m.Unacknowledged}
		var seen sessionSeen
		if sess := m.Session; sess != nil {
			seen = s.live.of(sess.ID)
			// The store's last-seen time is the session's start or resume.
			if sess.LastSeenAt.After(seen.seen) {
				seen.seen = sess.LastSeenAt
			}
			v.Session = &opapi.MemberSession{ID: sess.ID, Generation: sess.Generation, StartedAt: sess.CreatedAt,
				LastSeenAt: seen.seen, LastWaitedAt: seen.waited}
		}
		var (
			changed time.Time
			phase   store.AttemptPhase
		)
		if a := m.OpenAttempt; a != nil {
			v.OpenAttempt = &opapi.MemberAttempt{ID: a.ID, State: string(a.State), Phase: string(a.Phase),
				HubID: a.Task.HubID, ProjectID: a.Task.ProjectID, TaskID: a.Task.TaskID, UpdatedAt: a.UpdatedAt,
				HandoffUpdatedAt: a.HandoffUpdatedAt}
			if b := a.Block; b != nil {
				v.OpenAttempt.Block = &opapi.AttemptBlock{Reason: b.Reason, Since: b.Since}
			}
			if b := a.HubBlock; b != nil {
				v.OpenAttempt.HubBlock = &opapi.AttemptBlock{Reason: b.Reason, Since: b.Since}
			}
			changed, phase = a.UpdatedAt, a.Phase
		}
		v.State = store.DeriveState(st.Paused != nil || m.Paused != nil, m.Session != nil, seen.seen, seen.waited, now)
		v.Stalled = store.LooksStalled(v.State, phase, changed, now)
		if h := m.Hold; h != nil {
			v.Hold = &opapi.MemberHold{Kind: h.Kind, AttemptID: h.AttemptID, SpentUSD: h.SpentUSD, LimitUSD: h.LimitUSD,
				NextCheck: h.NextCheck, ResetAt: h.ResetAt, Command: h.Command, Since: h.Since}
		}
		out.Members = append(out.Members, v)
	}
	return out
}

// pauseRoute is a pause (paused set) or a resume of the team, or of one
// member when member is set.
func (s *Server) pauseRoute(member, paused bool) adminAction {
	return func(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
		return s.setPause(w, r, op, member, paused)
	}
}

func (s *Server) setPause(w http.ResponseWriter, r *http.Request, op store.Caller, member, paused bool) (string, string) {
	var in opapi.PauseRequest
	if !adminBody(w, r, &in) {
		return opapi.CodeInvalid, ""
	}
	if member != (in.AgentID != "") {
		msg := "a team's pause names no member: use the member routes"
		if member {
			msg = "name the member: agent_id"
		}
		adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, msg)
		return opapi.CodeInvalid, ""
	}
	res, err := s.store.SetPause(r.Context(), op, commandKey(), in.TeamID, in.AgentID, paused, in.Reason)
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, opapi.PauseResult{TeamID: res.TeamID, AgentID: res.AgentID, Paused: pauseOf(res.Paused),
		Changed: res.Changed})
	outcome := "unchanged"
	if res.Changed {
		outcome = map[bool]string{true: "paused", false: "resumed"}[paused]
	}
	return outcome, res.TeamID
}

func (s *Server) nudge(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var in opapi.NudgeRequest
	if !adminBody(w, r, &in) {
		return opapi.CodeInvalid, ""
	}
	m, err := s.store.Nudge(r.Context(), op, commandKey(), in.TeamID, in.AgentID, in.Note)
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, opapi.Nudge{MessageID: m.ID, Seq: m.Seq, TeamID: m.TeamID, AgentID: m.To, CreatedAt: m.CreatedAt})
	return "sent", m.ID
}

func pauseOf(p *store.Pause) *opapi.Pause {
	if p == nil {
		return nil
	}
	return &opapi.Pause{TeamID: p.TeamID, AgentID: p.AgentID, Reason: p.Reason, PausedBy: p.PausedBy, PausedAt: p.PausedAt}
}
