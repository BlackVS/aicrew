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
	ClaimPath    = AttemptsPath + "/claim"

	maxStepBody = 4 << 10
)

// The actions on one attempt, at AttemptsPath/{id}/{action}.
var attemptActions = []string{"accept", "decline", "withdraw", "settle", "stop", "confirm-stop", "release", "review",
	"confirm-delivery", "finalize", "work"}

var attemptIDShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func attemptRoute(action string) string { return AttemptsPath + "/{id}/" + action }

func (s *Server) registerAttempts() {
	s.handle(http.MethodPost, AttemptsPath, s.offer)
	s.handle(http.MethodPost, ClaimPath, s.claim)
	handlers := map[string]http.HandlerFunc{"accept": s.accept, "decline": s.decline, "withdraw": s.withdraw,
		"settle": s.settle, "stop": s.requestStop, "confirm-stop": s.confirmStop, "release": s.releaseStopped,
		"review": s.review, "confirm-delivery": s.confirmDelivery, "finalize": s.finalize, "work": s.work}
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
	{store.ErrDeliveryUnconfirmed, "delivery_unconfirmed"},
	{store.ErrSupersedeLimit, "supersede_limit"},
	{store.ErrOutcomeUnknown, "outcome_unknown"},
	{store.ErrProjectNotGranted, "project_not_granted"},
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
// of a step that has already settled has no step to send: no request key.
// (A work update's step has no proof at all, so a missing proof says
// nothing.)
func (s *Server) writeStep(w http.ResponseWriter, r *http.Request, a store.Attempt, step store.Step, err error) {
	switch {
	case err != nil:
		s.refuseSession(w, r, attemptRefusal(err), false, 0)
	case step.RequestKey == "":
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
	// DependencyEvidence is the client's read of the task's dependencies,
	// recorded in the offer's audit (1aad G1).
	DependencyEvidence []store.DependencyEvidence `json:"dependency_evidence"`
}

// offer begins the coordinator's offer of a task to a named worker. The
// reply's Location names the new attempt.
func (s *Server) offer(w http.ResponseWriter, r *http.Request) {
	var in offerBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok || !s.granted(w, r, token, store.RoleCoordinator, in.Task) {
		return
	}
	a, step, err := s.store.BeginOfferWithToken(r.Context(), key, token, store.OfferInput{
		WorkerAgentID: in.WorkerAgentID, Task: in.Task, ExpectedRevision: in.ExpectedRevision,
		BaseCommit: in.BaseCommit, Branch: in.Branch, ExpiresAt: in.ExpiresAt,
		Process: store.TrustedProcess{InstructionDigest: in.InstructionDigest, Identity: store.ProcessIdentity{
			Repository: in.Process.Repo, Commit: in.Process.Commit, Manifest: in.Process.Manifest}},
		Dependencies: in.DependencyEvidence,
	})
	s.writeStep(w, r, a, step, err)
}

// granted checks, for an offer or a claim, that the team's hub grants the
// task's project, by one live team.read (liveGrant), and refuses the step
// otherwise; it reports whether the step may go on. A session in another
// role reads nothing: the store refuses its step.
func (s *Server) granted(w http.ResponseWriter, r *http.Request, token string, role store.Role, task store.TaskRef) bool {
	b, err := s.store.AuthenticateSessionToken(r.Context(), token)
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return false
	}
	if b.Role != role {
		return true
	}
	if code := s.liveGrant(r.Context(), b.TeamID, task); code != "" {
		var after time.Duration
		if code == "hub_unavailable" {
			after = hubRetryAfter
		}
		s.refuseSession(w, r, code, false, after)
		return false
	}
	return true
}

// hubRetryAfter is the Retry-After of a step refused because the team's hub
// did not answer its team read.
const hubRetryAfter = 30 * time.Second

// accept begins the worker's acceptance of its offer.
func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in acceptBody
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
	Stop        string `json:"stop,omitempty"`
	Phase       string `json:"phase,omitempty"`
	CloseReason string `json:"close_reason,omitempty"`
	// AcceptedResult is the result the current coordinator accepted, and
	// DeliveryResult the one whose delivery a team member confirmed.
	AcceptedResult int64 `json:"accepted_result,omitempty"`
	DeliveryResult int64 `json:"delivery_result,omitempty"`
	// ProcessVerifiedReceipt names the committed claim receipt under which
	// aimem verified the attempt's process pin; absent while the pin is
	// unverified input.
	ProcessVerifiedReceipt string `json:"process_verified_receipt,omitempty"`
}

func viewOf(a store.Attempt) attemptView {
	return attemptView{ID: a.ID, State: string(a.State), Declined: a.Declined, Stop: string(a.Stop), Phase: string(a.Phase),
		CloseReason: a.CloseReason, AcceptedResult: a.AcceptedResult, DeliveryResult: a.DeliveryResult,
		ProcessVerifiedReceipt: a.ProcessVerifiedReceipt}
}

// writeAttempt answers a local step with the attempt.
func (s *Server) writeAttempt(w http.ResponseWriter, r *http.Request, a store.Attempt, err error) {
	if err != nil {
		s.refuseSession(w, r, attemptRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, viewOf(a))
}

// decline records the worker's decline. It is local: no step, no proof.
func (s *Server) decline(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	token, key, ok := s.stepRequest(w, r, true, nil)
	if !ok {
		return
	}
	a, err := s.store.DeclineWithToken(r.Context(), key, token, id)
	s.writeAttempt(w, r, a, err)
}

type claimBody struct {
	Task              store.TaskRef `json:"task"`
	ExpectedRevision  int64         `json:"expected_revision"`
	BaseCommit        string        `json:"base_commit"`
	Branch            string        `json:"branch"`
	Process           processPin    `json:"process"`
	InstructionDigest string        `json:"instruction_digest"`
}

// claim begins an independent member's claim of a task for itself. The
// reply's Location names the new attempt.
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var in claimBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok || !s.granted(w, r, token, store.RoleIndependent, in.Task) {
		return
	}
	a, step, err := s.store.BeginClaimWithToken(r.Context(), key, token, store.ClaimInput{
		Task: in.Task, ExpectedRevision: in.ExpectedRevision, BaseCommit: in.BaseCommit, Branch: in.Branch,
		InstructionDigest: in.InstructionDigest,
		Process: store.TrustedProcess{InstructionDigest: in.InstructionDigest, Identity: store.ProcessIdentity{
			Repository: in.Process.Repo, Commit: in.Process.Commit, Manifest: in.Process.Manifest}},
	})
	s.writeStep(w, r, a, step, err)
}

// requestStop records the team's current coordinator's request to stop a
// running attempt. It is local: no step, no proof.
func (s *Server) requestStop(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in stopBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	a, err := s.store.RequestStopWithToken(r.Context(), key, token, id, in.Reason)
	s.writeAttempt(w, r, a, err)
}

// confirmStop records the worker's confirmation that it stopped. It is
// local: no step, no proof.
func (s *Server) confirmStop(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	token, key, ok := s.stepRequest(w, r, true, nil)
	if !ok {
		return
	}
	a, err := s.store.ConfirmStopWithToken(r.Context(), key, token, id)
	s.writeAttempt(w, r, a, err)
}

// releaseStopped begins the holder's release of its stopped attempt, to
// READY or to BLOCKED with a blocker, under the stopped fact.
func (s *Server) releaseStopped(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in releaseBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	a, step, err := s.store.BeginStopReleaseWithToken(r.Context(), key, token, id, in.Target, in.Blocker)
	s.writeStep(w, r, a, step, err)
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

// review records the team's current coordinator's decision on the latest
// submitted result: accept or rework. It is local.
func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in reviewBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	a, err := s.store.ReviewWithToken(r.Context(), key, token, id, in.ResultSeq, in.Decision)
	s.writeAttempt(w, r, a, err)
}

// confirmDelivery records the team's current coordinator's confirmation that
// the accepted result was delivered, with the evidence it gathered from the
// forge. It is local; aicrew queries no forge.
func (s *Server) confirmDelivery(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in deliveryBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	a, err := s.store.ConfirmDeliveryWithToken(r.Context(), key, token, id, in.ResultSeq, in.Evidence)
	s.writeAttempt(w, r, a, err)
}

// finalize begins finalizing the accepted result as DONE. The body names the
// result only: its terminal evidence is the confirmed delivery's, which the
// begin response returns for the member to send.
func (s *Server) finalize(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in finalizeBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	a, step, err := s.store.BeginFinalizeWithToken(r.Context(), key, token, id, in.ResultSeq)
	s.writeStep(w, r, a, step, err)
}

// work begins the holder's work update: block with a blocker, submit with a
// result reference, or resume. The step has no proof. With supersedes, the
// same update (repeating its intent and detail) gets a new request key, and
// the superseded key becomes an alias of the step.
func (s *Server) work(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in workBody
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	var (
		a    store.Attempt
		step store.Step
		err  error
	)
	if in.Supersedes != "" {
		a, step, err = s.store.SupersedeWorkWithToken(r.Context(), key, token, id, in.Supersedes, in.Intent, in.Detail)
	} else {
		a, step, err = s.store.BeginWorkWithToken(r.Context(), key, token, id, in.Intent, in.Detail)
	}
	s.writeStep(w, r, a, step, err)
}
