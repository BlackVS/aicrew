package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The offer family's steps over the session API (crew-execution b1a-2). Each
// operation authenticates the session token itself: the acting agent, its
// session and generation come from the token, and every command rechecks the
// token inside its transaction, replays included. See steps.go for begin and
// settle.

// OfferInput is an offer as the coordinator's client sends it. The process
// pin and the instruction digest are the coordinator's: aimem verifies the pin
// against its current selection when the claim commits (D-b1(a)), and the
// digest only has to match the worker's at acceptance (D-b1a-3).
type OfferInput struct {
	WorkerAgentID    string         `json:"worker_agent_id"`
	Task             TaskRef        `json:"task"`
	ExpectedRevision int64          `json:"expected_revision"`
	BaseCommit       string         `json:"base_commit"`
	Branch           string         `json:"branch"`
	Process          TrustedProcess `json:"process"`
	ExpiresAt        time.Time      `json:"expires_at"`
}

// withToken makes cmd require token, still valid for its session at its
// generation, inside the command's transaction, on a new command and on a
// replay alike.
func (s *Store) withToken(cmd command, token string, t storedToken) command {
	valid := s.requireToken(token, t.sessionID, t.generation, nil)
	check, replay := cmd.check, cmd.replayCheck
	cmd.check = func(ctx context.Context, tx *sql.Tx) error {
		if err := valid(ctx, tx); err != nil {
			return err
		}
		if check != nil {
			return check(ctx, tx)
		}
		return nil
	}
	cmd.replayCheck = func(ctx context.Context, tx *sql.Tx, result string) error {
		if err := valid(ctx, tx); err != nil {
			return err
		}
		if replay != nil {
			return replay(ctx, tx, result)
		}
		return nil
	}
	return cmd
}

// tokenCaller finds the token's session and agent; the command then checks
// that the token is valid.
func (s *Store) tokenCaller(ctx context.Context, token string) (storedToken, Caller, error) {
	t, agentID, err := s.tokenOwner(ctx, token)
	if err != nil {
		return storedToken{}, Caller{}, err
	}
	return t, Caller{kind: callerAgent, id: agentID}, nil
}

// BeginOfferWithToken begins an offer as the token's session, which must be
// its team's current coordinator.
func (s *Store) BeginOfferWithToken(ctx context.Context, key, token string, in OfferInput) (Attempt, Step, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	var proof string
	cmd := offerCommand(c, key, OfferRequest{SessionID: t.sessionID, Generation: t.generation,
		WorkerAgentID: in.WorkerAgentID, Task: in.Task, ExpectedRevision: in.ExpectedRevision, BaseCommit: in.BaseCommit,
		Branch: in.Branch, Process: in.Process, ExpiresAt: in.ExpiresAt}, &proof)
	return s.beginOffer(ctx, c, s.withToken(cmd, token, t), &proof, t.sessionID, t.generation)
}

// BeginAcceptWithToken begins the acceptance of an offer by its worker, as
// the token's session. instructionDigest is the digest of the instructions
// the worker verified.
func (s *Store) BeginAcceptWithToken(ctx context.Context, key, token, attemptID, instructionDigest string) (Attempt, Step, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	var proof string
	in := AcceptRequest{SessionID: t.sessionID, Generation: t.generation, InstructionDigest: instructionDigest}
	cmd := s.withToken(acceptCommand(c, key, attemptID, in, false, &proof), token, t)
	return s.begin(ctx, c, cmd, attemptID, &proof, FactAcceptedAttempt, t.sessionID, t.generation)
}

// DeclineWithToken records the worker's decline, as the token's session. It
// is local: no step and no proof.
func (s *Store) DeclineWithToken(ctx context.Context, key, token, attemptID string) (Attempt, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Attempt{}, err
	}
	return s.decline(ctx, c, s.withToken(declineCommand(c, key, attemptID, t.sessionID, t.generation), token, t), attemptID)
}

// BeginWithdrawWithToken begins the release of an offer never accepted, as
// the token's session, which must be its team's current coordinator.
func (s *Store) BeginWithdrawWithToken(ctx context.Context, key, token, attemptID string) (Attempt, Step, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	var proof string
	cmd := s.withToken(releaseCommand(c, key, attemptID, t.sessionID, t.generation, &proof), token, t)
	return s.begin(ctx, c, cmd, attemptID, &proof, FactNeverAccepted, t.sessionID, t.generation)
}

// SettleWithToken settles the attempt's step with requestKey for the token's
// session, which must be in the attempt's team. reader is aimem's read scope,
// or nil while aicrew has none (the step then stays pending).
func (s *Store) SettleWithToken(ctx context.Context, token string, reader ReservationReader, attemptID, requestKey string,
	report StepReport) (Attempt, Settlement, error) {
	b, err := s.AuthenticateSessionToken(ctx, token)
	if err != nil {
		return Attempt{}, Settlement{}, err
	}
	a, err := s.GetAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, Settlement{}, err
	}
	if a.TeamID != b.TeamID {
		return Attempt{}, Settlement{}, fmt.Errorf("attempt %s: %w", attemptID, ErrNotFound)
	}
	return s.SettleStep(ctx, Caller{kind: callerAgent, id: b.AgentID}, reader, attemptID, requestKey, report)
}
