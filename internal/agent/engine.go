package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
)

// Timing (CC3 decisions D-3b and D-3c).
const (
	// A handle is refreshed when a third of its life remains, and never
	// later than refreshFloor before it expires.
	refreshFloor = 90 * time.Second
	// The session is resumed with a new proof this long before the session
	// token's ceiling, so a long session never stalls.
	resumeLead = 10 * time.Minute
	// Retries back off from retryBase to retryCap. At least 10 s between
	// refreshes keeps within aicrewd's 6 refreshes a minute.
	retryBase = 10 * time.Second
	retryCap  = 60 * time.Second
	// entryRounds bounds how many challenges one entry may use, and
	// proofRenewals how many fresh proofs one challenge may use after its
	// proof was refused.
	entryRounds   = 5
	proofRenewals = 3
	// leaveAttempts bounds the retries of a leave whose outcome is unknown.
	leaveAttempts = 6
)

var (
	// ErrSessionEnded reports that aicrew ended the session (left, stopped
	// or removed) while the engine held it.
	ErrSessionEnded = errors.New("the team session has ended")
)

// WorkOutstanding reports a leave that aicrew refused because the member
// still has open work. The session is kept, never forced (D-3g).
type WorkOutstanding struct{ NextAction string }

func (w *WorkOutstanding) Error() string {
	return "the session was kept: the member still has open work in the team. Next: " + w.NextAction
}

// Engine holds one agent's team session. Its secrets exist only in its
// memory. One goroutine drives it: Start, then Run until its context ends,
// or Leave.
type Engine struct {
	Cfg   Config
	Crew  CrewAPI
	Aimem Aimem
	Log   *slog.Logger
	// Now and Sleep are the clock; tests replace them.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	session   Session
	serviceID string
	hubID     string
	token     string
	tokenEnds time.Time
	handleAt  time.Time // when the current handle was obtained
	handleEnd time.Time
	aimemFile string
	// pendingHandle is an entry's handle until bind gives it to aimem.
	pendingHandle string
}

// NewEngine assembles an engine for cfg. Aimem's lifecycle commands are
// always serialized per session.
func NewEngine(cfg Config, crew CrewAPI, aimem Aimem, log *slog.Logger) *Engine {
	return &Engine{Cfg: cfg, Crew: crew, Aimem: Serialize(aimem, LockDir(cfg.Home)), Log: log, Now: time.Now, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// LockDir is where an agent home keeps its per-session aimem locks.
func LockDir(home string) string { return filepath.Join(home, "state", "locks") }

func newKey(purpose string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "aicrew-agent-" + purpose + "-" + hex.EncodeToString(b[:])
}

func backoff(attempt int, hint time.Duration) time.Duration {
	d := retryBase << min(attempt, 3)
	if d > retryCap {
		d = retryCap
	}
	return max(d, hint)
}

func retryAfter(err error) time.Duration {
	var r *Refusal
	if errors.As(err, &r) {
		return r.RetryAfter
	}
	return 0
}

// SessionID is the held session's ID, and AimemFile the path of aimem's
// session file for it (the value of AIMEM_TEAM_SESSION).
func (e *Engine) SessionID() string { return e.session.ID }
func (e *Engine) AimemFile() string { return e.aimemFile }

// Start enters the agent's team, or resumes the session a restarted client
// recorded, and binds aimem to it. A recorded session that has ended is
// forgotten and the team entered anew.
func (e *Engine) Start(ctx context.Context) error {
	st, ok, err := LoadState(e.Cfg.Home)
	if err != nil {
		return err
	}
	if ok && st.AgentID == e.Cfg.AgentID && st.TeamID == e.Cfg.TeamID {
		err := e.enter(ctx, st.SessionID)
		switch code := codeOf(err); {
		case err == nil:
			e.aimemFile = st.AimemFile
			return e.bind(ctx)
		case code == "context_stale" || code == "role_forbidden":
			e.Log.Info("the recorded session has ended; entering the team anew", "session", st.SessionID)
			if err := ClearState(e.Cfg.Home); err != nil {
				return err
			}
		default:
			return err
		}
	}
	if err := e.enter(ctx, ""); err != nil {
		return err
	}
	return e.bind(ctx)
}

// enter proves the agent's identity and enters its team, or resumes
// sessionID. It follows the client rule of the session API: a retry reuses
// the key only after the earlier attempt was abandoned, the secrets kept are
// those of the latest request sent, and a challenge that has expired is
// replaced by a new one with a new proof.
func (e *Engine) enter(ctx context.Context, sessionID string) error {
	for round := 0; round < entryRounds; round++ {
		ch, err := e.challenge(ctx)
		if err != nil {
			return err
		}
		receipt, err := e.Aimem.Proof(ctx, ch.ServiceID, ch.HubID, ch.ID)
		if err != nil {
			return err
		}
		key := newKey("enter")
		team := ""
		if sessionID == "" {
			team = e.Cfg.TeamID
		}
		renewals := 0
		for attempt := 0; ; attempt++ {
			entry, err := e.Crew.Enter(ctx, key, receipt, ch.ServiceID, ch.ID, team, sessionID)
			if err == nil {
				e.adopt(entry, ch)
				return nil
			}
			if codeOf(err) == "challenge_invalid" || !e.Now().Before(ch.ExpiresAt) {
				break // a new challenge and a new proof
			}
			// A refused proof is a known outcome: aicrewd refused before
			// committing anything. The receipt may have outlived its 60 s
			// while the challenge is still valid, so a fresh proof for the
			// same challenge is tried, a bounded number of times. It is new
			// input, so it goes under a new key; a key whose request did
			// commit is answered by aicrewd's reissue, never by this refusal.
			if codeOf(err) == "proof_invalid" && renewals < proofRenewals {
				renewals++
				e.Log.Info("the proof was refused; obtaining a fresh one for the same challenge")
				if receipt, err = e.Aimem.Proof(ctx, ch.ServiceID, ch.HubID, ch.ID); err != nil {
					return err
				}
				key = newKey("enter")
				continue
			}
			if !Retryable(err) {
				return err
			}
			e.Log.Info("entry not answered; retrying with the same key", "reason", reason(err))
			if err := e.Sleep(ctx, backoff(attempt, retryAfter(err))); err != nil {
				return err
			}
		}
	}
	return errors.New("no entry succeeded within its challenges; try again later")
}

func (e *Engine) challenge(ctx context.Context) (Challenge, error) {
	key := newKey("challenge")
	for attempt := 0; ; attempt++ {
		ch, err := e.Crew.Challenge(ctx, key, e.Cfg.AgentID)
		if err == nil || !Retryable(err) {
			return ch, err
		}
		if err := e.Sleep(ctx, backoff(attempt, retryAfter(err))); err != nil {
			return Challenge{}, err
		}
	}
}

// adopt takes an entry's secrets and times; they replace any earlier ones.
func (e *Engine) adopt(entry Entry, ch Challenge) {
	e.session, e.serviceID, e.hubID = entry.Session, ch.ServiceID, ch.HubID
	e.token, e.tokenEnds = entry.Token, entry.TokenExpires
	e.handleAt, e.handleEnd = e.Now(), entry.HandleExpires
	e.pendingHandle = entry.Handle
}

// bind gives aimem the current handle: a new session file, or a refresh of
// the file aimem already holds for the session, then records the session.
func (e *Engine) bind(ctx context.Context) error {
	handle := e.pendingHandle
	e.pendingHandle = ""
	// The session is recorded before aimem is asked: if the binding fails,
	// a restarted client still resumes this session instead of trying to
	// enter the team beside it.
	if err := e.record(e.aimemFile); err != nil {
		return err
	}
	path, open, err := e.Aimem.Status(ctx, e.session.ID)
	if err != nil {
		return err
	}
	if open {
		if err := e.Aimem.Refresh(ctx, e.session.ID, handle); err != nil {
			return err
		}
	} else if path, err = e.Aimem.Open(ctx, e.serviceID, e.session.TeamID, e.session.ID, handle); err != nil {
		return err
	}
	e.aimemFile = path
	e.Log.Info("in session", "session", e.session.ID, "team", e.session.TeamID, "generation", e.session.Generation)
	return e.record(path)
}

// record saves the nonsecret recovery record of the held session.
func (e *Engine) record(aimemFile string) error {
	return SaveState(e.Cfg.Home, State{AgentID: e.Cfg.AgentID, TeamID: e.session.TeamID, ServiceID: e.serviceID,
		HubID: e.hubID, SessionID: e.session.ID, AimemFile: aimemFile, UpdatedAt: e.Now().UTC()})
}

// next is when the engine acts again, and whether that is a resume (before
// the token's ceiling) rather than a handle refresh.
func (e *Engine) next() (time.Time, bool) {
	life := e.handleEnd.Sub(e.handleAt)
	lead := max(life/3, refreshFloor)
	refreshAt := e.handleEnd.Add(-lead)
	resumeAt := e.tokenEnds.Add(-resumeLead)
	if !resumeAt.After(refreshAt) {
		return resumeAt, true
	}
	return refreshAt, false
}

// Run keeps the session alive until ctx ends, then leaves it. It returns
// ErrSessionEnded if aicrew ended the session meanwhile.
func (e *Engine) Run(ctx context.Context) error {
	for {
		at, resume := e.next()
		if err := e.Sleep(ctx, at.Sub(e.Now())); err != nil {
			if ctx.Err() != nil {
				return e.leave(context.WithoutCancel(ctx))
			}
			return err
		}
		var err error
		if resume {
			err = e.resume(ctx)
		} else {
			err = e.refresh(ctx)
		}
		if ctx.Err() != nil {
			return e.leave(context.WithoutCancel(ctx))
		}
		if err != nil {
			return err
		}
	}
}

// resume proves afresh and resumes the held session under a new generation.
func (e *Engine) resume(ctx context.Context) error {
	if err := e.enter(ctx, e.session.ID); err != nil {
		if code := codeOf(err); code == "context_stale" || code == "role_forbidden" {
			_ = ClearState(e.Cfg.Home)
			return ErrSessionEnded
		}
		return err
	}
	return e.bind(ctx)
}

// refresh obtains a new handle and gives it to aimem. Each attempt uses a
// new key: a replayed refresh returns no handle, and the previous handle
// stays valid for a short overlap. A token that stopped working (a resume
// by another client, a rotation) is replaced by a resume.
func (e *Engine) refresh(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		h, err := e.Crew.Refresh(ctx, newKey("refresh"), e.token, e.hubID)
		if err == nil {
			if err := e.Aimem.Refresh(ctx, e.session.ID, h.Value); err != nil {
				return err
			}
			e.handleAt, e.handleEnd = e.Now(), h.Expires
			return nil
		}
		switch code := codeOf(err); {
		case code == "invalid_token" || code == "context_stale":
			return e.resume(ctx)
		case !Retryable(err):
			return err
		}
		if !e.Now().Before(e.handleEnd) {
			e.Log.Warn("the handle expired before a refresh succeeded; resuming", "reason", reason(err))
			return e.resume(ctx)
		}
		e.Log.Info("refresh not answered; retrying with a new key", "reason", reason(err))
		if err := e.Sleep(ctx, backoff(attempt, retryAfter(err))); err != nil {
			return err
		}
	}
}

// LeaveRecorded leaves the session this agent home recorded, from a client
// that does not hold it (aicrew-agent session leave). It proves afresh and
// resumes that session, which fences any client still holding it, then
// leaves. It never enters the team: if the recorded session has already
// ended, it only closes aimem's binding of it and clears the record.
func (e *Engine) LeaveRecorded(ctx context.Context) error {
	st, ok, err := LoadState(e.Cfg.Home)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no team session is recorded in this agent home")
	}
	err = e.enter(ctx, st.SessionID)
	switch code := codeOf(err); {
	case err == nil:
		// The resume moved the session to a new generation. aimem is given
		// its handle before the leave, so that a leave refused for open work
		// keeps the session with a binding that still works.
		e.aimemFile = st.AimemFile
		if err := e.bind(ctx); err != nil {
			return err
		}
		return e.leave(ctx)
	case code == "context_stale" || code == "role_forbidden":
		e.session = Session{ID: st.SessionID, TeamID: st.TeamID}
		if err := e.Aimem.Close(ctx, st.SessionID); err != nil {
			return fmt.Errorf("the session has ended, but aimem kept its binding: %w", err)
		}
		e.Log.Info("the recorded session had already ended; its binding is closed", "session", st.SessionID)
		return ClearState(e.Cfg.Home)
	}
	return err
}

// Leave ends the held session: aicrew first, then aimem's binding, which
// aimem drops only once its hub confirms the end. Outstanding work keeps the
// session and is reported (D-3g). A leave whose outcome is unknown is
// retried with the same key; aicrew replays a committed leave.
func (e *Engine) Leave(ctx context.Context) error { return e.leave(ctx) }

func (e *Engine) leave(ctx context.Context) error {
	key := newKey("leave")
	resumed := false
leaving:
	for attempt := 0; ; attempt++ {
		_, err := e.Crew.Leave(ctx, key, e.token)
		if err == nil {
			break
		}
		var r *Refusal
		switch code := codeOf(err); {
		case code == "work_outstanding" && errors.As(err, &r):
			return &WorkOutstanding{NextAction: r.NextAction}
		case code == "invalid_token" && !resumed:
			// The token ended (its ceiling, or a resume elsewhere): prove
			// afresh, then leave with the new one.
			resumed = true
			if err := e.enter(ctx, e.session.ID); err != nil {
				if c := codeOf(err); c == "context_stale" || c == "role_forbidden" {
					break leaving // the session has already ended
				}
				return err
			}
			key = newKey("leave")
			continue
		case !Retryable(err) || attempt+1 >= leaveAttempts:
			return err
		}
		if err := e.Sleep(ctx, backoff(attempt, retryAfter(err))); err != nil {
			return err
		}
	}
	if err := e.Aimem.Close(ctx, e.session.ID); err != nil {
		return fmt.Errorf("left the session, but aimem kept its binding: %w", err)
	}
	e.token = ""
	e.Log.Info("left the session", "session", e.session.ID)
	return ClearState(e.Cfg.Home)
}

// reason names an error for a log line: a refusal code, or "unreachable".
func reason(err error) string {
	if c := codeOf(err); c != "" {
		return c
	}
	return "unreachable"
}
