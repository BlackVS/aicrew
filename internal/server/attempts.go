package server

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// The offer family's member-driven steps (docs/CREW-CONTRACT.md, "Attempt
// steps"). Under D4(a) the acting member's own aimem connection sends each
// reservation mutation, so a step is two calls: begin records the intent and
// returns coordination.v1's begin response, with the coordination proof, to
// the member; settle then confirms the outcome through aimem's read scope,
// with the member's report as a hint only. Every route authenticates by the
// session token, which alone names the agent, session and generation.

const (
	AttemptsPath = "/v1/crew/attempts"

	maxStepBody = 4 << 10
)

// The actions on one attempt, at AttemptsPath/{id}/{action}.
var attemptActions = []string{"accept", "decline", "withdraw", "settle"}

var attemptIDShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func attemptRoute(action string) string { return AttemptsPath + "/{id}/" + action }

func (s *Server) registerAttempts() {
	s.handle(http.MethodPost, AttemptsPath, s.offer)
	handlers := map[string]http.HandlerFunc{"accept": s.accept, "decline": s.decline, "withdraw": s.withdraw,
		"settle": s.settle}
	for _, action := range attemptActions {
		s.handle(http.MethodPost, attemptRoute(action), handlers[action])
	}
}

// attemptPath splits an attempt action path into the attempt ID and the
// action's route. The ID must have the attempt ID shape exactly: nothing is
// decoded or cleaned.
func attemptPath(path string) (id, route string, ok bool) {
	rest, found := strings.CutPrefix(path, AttemptsPath+"/")
	if !found {
		return "", "", false
	}
	id, action, found := strings.Cut(rest, "/")
	if !found || !attemptIDShape.MatchString(id) {
		return "", "", false
	}
	for _, a := range attemptActions {
		if action == a {
			return id, attemptRoute(a), true
		}
	}
	return "", "", false
}

// attemptRefusals are the step routes' own refusal codes, in the session
// API's envelope.
var attemptRefusals = []struct {
	err  error
	code string
}{
	{store.ErrTaskBusy, "task_busy"},
	{store.ErrAgentBusy, "agent_busy"},
	{store.ErrAttemptState, "attempt_state"},
	{store.ErrOfferExpired, "offer_expired"},
	{store.ErrOfferDeclined, "offer_declined"},
	{store.ErrOfferStale, "offer_stale"},
	{store.ErrInstructionMismatch, "instruction_mismatch"},
	{store.ErrProcessChanged, "process_changed"},
	{store.ErrStepUnknown, "step_unknown"},
	{store.ErrOutcomeUnknown, "outcome_unknown"},
}

func attemptRefusal(err error) string {
	for _, r := range attemptRefusals {
		if errors.Is(err, r.err) {
			return r.code
		}
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrForbidden) {
		return "attempt_forbidden"
	}
	return refusalCode(err)
}

// stepRequest checks what every step route needs: first a valid session
// token, so nothing about the request is judged for an unauthenticated
// caller; then an Idempotency-Key when key is wanted, and a JSON body within
// the bound, decoded strictly into v. An empty body stands for an empty
// object. The store operation authenticates the token again inside its
// transaction.
func (s *Server) stepRequest(w http.ResponseWriter, r *http.Request, wantKey bool, v any) (token, key string, ok bool) {
	token = sessionToken(r)
	if _, err := s.store.AuthenticateSessionToken(r.Context(), token); err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return "", "", false
	}
	if wantKey {
		if key, ok = idempotencyKey(r); !ok {
			s.refuseSession(w, r, "invalid_request", false, 0)
			return "", "", false
		}
	}
	body, bodyOK := readBody(r, maxStepBody)
	switch {
	case !bodyOK:
		s.refuseSession(w, r, "invalid_request", false, 0)
		return "", "", false
	case len(body) == 0 && v == nil:
		return token, key, true
	case !mediaType(r, "application/json"):
		s.refuseSession(w, r, "invalid_request", false, 0)
		return "", "", false
	case v == nil:
		if !emptyObject(body) {
			s.refuseSession(w, r, "invalid_request", false, 0)
			return "", "", false
		}
	default:
		if decodeStrict(body, v) != nil {
			s.refuseSession(w, r, "invalid_request", false, 0)
			return "", "", false
		}
	}
	return token, key, true
}

// writeStep answers a begin with coordination.v1's begin response. A replay
// of a step that has already settled has no step to send.
func (s *Server) writeStep(w http.ResponseWriter, r *http.Request, a store.Attempt, step store.Step, err error) {
	switch {
	case err != nil:
		s.refuseSession(w, r, attemptRefusal(err), false, 0)
	case step.CoordinationProof == "":
		s.refuseSession(w, r, "step_settled", false, 0)
	default:
		w.Header().Set("Location", AttemptsPath+"/"+a.ID)
		writeJSON(w, http.StatusOK, step)
	}
}

type offerBody struct {
	WorkerAgentID     string        `json:"worker_agent_id"`
	Task              store.TaskRef `json:"task"`
	ExpectedRevision  int64         `json:"expected_revision"`
	BaseCommit        string        `json:"base_commit"`
	Branch            string        `json:"branch"`
	Process           processPin    `json:"process"`
	InstructionDigest string        `json:"instruction_digest"`
	ExpiresAt         time.Time     `json:"expires_at"`
}

// offer begins the coordinator's offer of a task to a named worker. The
// reply's Location names the new attempt.
func (s *Server) offer(w http.ResponseWriter, r *http.Request) {
	var in offerBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	a, step, err := s.store.BeginOfferWithToken(r.Context(), key, token, store.OfferInput{
		WorkerAgentID: in.WorkerAgentID, Task: in.Task, ExpectedRevision: in.ExpectedRevision,
		BaseCommit: in.BaseCommit, Branch: in.Branch, ExpiresAt: in.ExpiresAt,
		Process: store.TrustedProcess{InstructionDigest: in.InstructionDigest, Identity: store.ProcessIdentity{
			Repository: in.Process.Repo, Commit: in.Process.Commit, Manifest: in.Process.Manifest}},
	})
	s.writeStep(w, r, a, step, err)
}

// accept begins the worker's acceptance of its offer.
func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in struct {
		InstructionDigest string `json:"instruction_digest"`
	}
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	a, step, err := s.store.BeginAcceptWithToken(r.Context(), key, token, id, in.InstructionDigest)
	s.writeStep(w, r, a, step, err)
}

// withdraw begins the release of an offer never accepted, by the team's
// current coordinator.
func (s *Server) withdraw(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	token, key, ok := s.stepRequest(w, r, true, nil)
	if !ok {
		return
	}
	a, step, err := s.store.BeginWithdrawWithToken(r.Context(), key, token, id)
	s.writeStep(w, r, a, step, err)
}

type attemptView struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Declined    bool   `json:"declined"`
	CloseReason string `json:"close_reason,omitempty"`
}

func viewOf(a store.Attempt) attemptView {
	return attemptView{ID: a.ID, State: string(a.State), Declined: a.Declined, CloseReason: a.CloseReason}
}

// decline records the worker's decline. It is local: no step, no proof.
func (s *Server) decline(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	token, key, ok := s.stepRequest(w, r, true, nil)
	if !ok {
		return
	}
	a, err := s.store.DeclineWithToken(r.Context(), key, token, id)
	if err != nil {
		s.refuseSession(w, r, attemptRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, viewOf(a))
}

type settleBody struct {
	RequestKey string         `json:"request_key"`
	Outcome    store.StepHint `json:"outcome"`
	Code       string         `json:"code,omitempty"`
}

type settleReply struct {
	Settled bool        `json:"settled"`
	Outcome string      `json:"outcome,omitempty"`
	Attempt attemptView `json:"attempt"`
}

// settle settles a step the member began. It is idempotent: a pending step
// answers 202 with Retry-After, and the client settles again.
func (s *Server) settle(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in settleBody
	token, _, ok := s.stepRequest(w, r, false, &in)
	if !ok {
		return
	}
	if !keyShape.MatchString(in.RequestKey) {
		s.refuseSession(w, r, "invalid_request", false, 0)
		return
	}
	a, set, err := s.store.SettleWithToken(r.Context(), token, s.reader, id, in.RequestKey,
		store.StepReport{Outcome: in.Outcome, Code: in.Code})
	switch {
	case err != nil:
		s.refuseSession(w, r, attemptRefusal(err), false, set.RetryAfter)
	case set.Settled:
		writeJSON(w, http.StatusOK, settleReply{Settled: true, Outcome: set.Outcome, Attempt: viewOf(a)})
	default:
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(set.RetryAfter)))
		writeJSON(w, http.StatusAccepted, settleReply{Attempt: viewOf(a)})
	}
}

// retrySeconds is Retry-After in whole seconds, rounded up, at least one.
func retrySeconds(d time.Duration) int {
	return max(int((d+time.Second-1)/time.Second), 1)
}
