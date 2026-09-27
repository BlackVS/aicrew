package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Session entry with an aimem proof, and aicrew session tokens
// (docs/CREW-CONTRACT.md, "Session tokens").
//
// An agent enters or resumes a team session by presenting a fresh aimem
// proof receipt for an agent challenge. The store redeems it through the
// Verifier with no transaction open, then commits in one transaction: the
// challenge is consumed, a credential rotation is applied, the session
// starts or resumes, and a session token and a first aimem-scoped handle are
// issued. The proof, not an identity, is the authority; the agent is the one
// the challenge names.
//
// A session token is aicrew's own, short-lived bearer for that session: it
// authorizes the session's handle refresh and leave, nothing else. It is
// bound to the session and its generation and lives at most TokenLifetime,
// with no refresh; after that the agent resumes with a new proof. It is valid
// only while its session is active at its generation, so leave, stop,
// removal, resume, re-proof and rotation end it with no separate revocation.
// Aicrew stores only its digest, and a handle issued under it never outlives
// it. A token is never a handle, and a handle or the introspection
// credential is never a token: each has its own prefix and table.
//
// A lost entry reply is recovered by a retry with the same key and exactly
// the same input, the receipt included, while the challenge's deadline has
// not passed. The retry returns the recorded session with a fresh token and
// handle, and deletes the token and handles of that session and generation,
// which only the lost reply carried, in one transaction. It never starts a
// second session. Neither secret is ever part of a recorded result, an audit
// record or an error; the receipt enters a command's input only as a digest.

// ErrTokenInvalid refuses a session token that is malformed, unknown,
// expired, replaced or fenced by a generation change, without saying which.
var ErrTokenInvalid = errors.New("invalid_token")

const (
	// TokenLifetime is a session token's fixed ceiling.
	TokenLifetime = 8 * time.Hour

	tokenPrefix = "ast1_"

	opEnterSession  = "session.enter"
	opResumeByProof = "session.resume_proof"
	opReissueEntry  = "session.entry.reissue"
	opRefreshHandle = "session.handle.refresh"
	entryKeyPrefix  = "session-entry:"
)

var tokenShape = regexp.MustCompile(`^ast1_[A-Za-z0-9_-]{43}$`)

// entrant is the caller of a proof entry: the proof is the authority, and
// the command's input binds it to one challenge.
var entrant = Caller{}

// SessionToken is an issued token's metadata; never the token.
type SessionToken struct {
	SessionID  string    `json:"session_id"`
	Generation int64     `json:"generation"`
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// SessionEntry is the result of entering or resuming a session with a
// proof: the session, the metadata of the token and handle issued with it,
// and whether the proof rotated the agent's aimem credential.
type SessionEntry struct {
	Session Session       `json:"session"`
	Token   SessionToken  `json:"token"`
	Handle  SessionHandle `json:"handle"`
	Rotated bool          `json:"rotated"`
}

// EntrySecrets are the token and handle issued with an entry. They exist
// only in the reply; formatting shows placeholders.
type EntrySecrets struct {
	Token  Secret
	Handle Secret
}

// TokenBinding is what a valid session token stands for.
type TokenBinding struct {
	AgentID    string
	TeamID     string
	SessionID  string
	Generation int64
	Role       Role
	HubID      string
	UserID     string
	TokenID    string
	ExpiresAt  time.Time
}

type sessionEntryInput struct {
	ChallengeID   string `json:"challenge_id"`
	TeamID        string `json:"team_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	ServiceID     string `json:"service_id"`
	ReceiptDigest string `json:"receipt_digest"`
}

// EnterSession starts the challenge's agent's session in a team with the
// aimem receipt the agent obtained for the challenge. serviceID is this aicrew
// service, from the trusted caller's configuration; the handle is bound to
// it.
func (s *Store) EnterSession(ctx context.Context, v Verifier, key, challengeID string, receipt Secret,
	teamID, serviceID string) (SessionEntry, EntrySecrets, error) {
	in := sessionEntryInput{ChallengeID: challengeID, TeamID: teamID, ServiceID: serviceID}
	precheck := func(ctx context.Context, q querier, c Caller) error {
		if _, err := getMembership(ctx, q, teamID, c.id); errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: agent %s is not a member of team %s", ErrForbidden, c.id, teamID)
		} else if err != nil {
			return err
		}
		if _, err := activeSession(ctx, q, teamID, c.id); err == nil {
			return fmt.Errorf("team %s agent %s: %w", teamID, c.id, ErrSessionActive)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	}
	enter := func(ctx context.Context, tx *sql.Tx, c Caller, now time.Time) (Session, error) {
		return startSessionTx(ctx, tx, c, teamID, now)
	}
	return s.enterWithProof(ctx, v, opEnterSession, key, receipt, in, precheck, enter)
}

// ResumeSessionWithProof resumes the challenge's agent's own active session
// under a new generation, fencing every holder of the old one, with the aimem
// receipt the agent obtained for the challenge.
func (s *Store) ResumeSessionWithProof(ctx context.Context, v Verifier, key, challengeID string, receipt Secret,
	sessionID, serviceID string) (SessionEntry, EntrySecrets, error) {
	in := sessionEntryInput{ChallengeID: challengeID, SessionID: sessionID, ServiceID: serviceID}
	precheck := func(ctx context.Context, q querier, c Caller) error {
		sess, err := ownSession(ctx, q, c, sessionID)
		if err != nil {
			return err
		}
		if sess.State != SessionActive {
			return fmt.Errorf("session %s is %s: %w", sessionID, sess.State, ErrContextStale)
		}
		return nil
	}
	enter := func(ctx context.Context, tx *sql.Tx, c Caller, now time.Time) (Session, error) {
		return resumeSessionTx(ctx, tx, c, sessionID, now)
	}
	return s.enterWithProof(ctx, v, opResumeByProof, key, receipt, in, precheck, enter)
}

// enterWithProof is the shared order of work of a proof entry:
//  0. An identical entry still running is waited for (inflight.go).
//  1. A retry of a committed entry is a lost reply: reissueEntry.
//  2. Pre-checks on a read snapshot, writing nothing, so aimem is not asked
//     about an entry that cannot succeed.
//  3. The Verifier redeems the receipt with no transaction open. If it fails
//     or aimem is unavailable, nothing changes.
//  4. One transaction rechecks the challenge, requires the proof to be the
//     agent's linked identity, applies a rotation, consumes the challenge,
//     enters the session and issues the token and the handle.
func (s *Store) enterWithProof(ctx context.Context, v Verifier, op, key string, receipt Secret, in sessionEntryInput,
	precheck func(context.Context, querier, Caller) error,
	enter func(context.Context, *sql.Tx, Caller, time.Time) (Session, error)) (SessionEntry, EntrySecrets, error) {
	var out SessionEntry
	var secrets EntrySecrets
	if !peerIDShape.MatchString(in.ServiceID) {
		return out, secrets, fmt.Errorf("%w: service ID must be 1 to 128 characters from [A-Za-z0-9._:-]", ErrInvalid)
	}
	if receipt.Reveal() == "" {
		return out, secrets, fmt.Errorf("%w: empty receipt", ErrInvalid)
	}
	in.ReceiptDigest = secretDigest(receipt.Reveal())
	cmd := command{op: op, scope: in.ChallengeID, key: key, input: in, authorize: anyCaller}
	release, err := s.flights.acquire(ctx, entrant, cmd, s.flightWait)
	if err != nil {
		return out, secrets, err
	}
	defer release()
	if done, err := s.receiptExists(ctx, entrant, cmd); err != nil {
		return out, secrets, err
	} else if done {
		return s.reissueEntry(ctx, cmd, in)
	}
	if s.afterReceiptLookup != nil {
		s.afterReceiptLookup(cmd.op)
	}

	var ch Challenge
	err = s.snapshot(ctx, func(q querier) error {
		var err error
		if ch, err = currentChallenge(ctx, q, in.ChallengeID, challengeAgent, s.now()); err != nil {
			return err
		}
		return precheck(ctx, q, Caller{kind: callerAgent, id: ch.AgentID})
	})
	if err != nil {
		return out, secrets, err
	}
	verified, err := verifyChallenge(ctx, v, ch, receipt, entryKeyPrefix+in.ChallengeID+":"+key)
	if err != nil {
		return out, secrets, err
	}

	self := Caller{kind: callerAgent, id: ch.AgentID}
	cmd.check = func(ctx context.Context, tx *sql.Tx) error {
		_, err := currentChallenge(ctx, tx, in.ChallengeID, challengeAgent, s.now())
		return err
	}
	cmd.apply = func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		agent, err := getAgent(ctx, tx, ch.AgentID)
		if err != nil {
			return nil, err
		}
		if agent.Linked == nil {
			return nil, fmt.Errorf("agent %s: %w", agent.ID, ErrIdentityLinkRequired)
		}
		if agent.Linked.HubID != verified.HubID || agent.Linked.UserID != verified.UserID {
			return nil, fmt.Errorf("agent %s is linked to another identity: %w", agent.ID, ErrIdentityMismatch)
		}
		rotated, err := rotateIfNeeded(ctx, tx, agent, verified, now)
		if err != nil {
			return nil, err
		}
		if err := consumeChallenge(ctx, tx, in.ChallengeID, now); err != nil {
			return nil, err
		}
		sess, err := enter(ctx, tx, self, now)
		if err != nil {
			return nil, err
		}
		entry, issued, err := issueEntrySecrets(ctx, tx, sess, in.ServiceID, now, now.Add(TokenLifetime))
		if err != nil {
			return nil, err
		}
		entry.Rotated = rotated
		secrets = issued
		return entry, nil
	}
	if err := s.run(ctx, entrant, cmd, &out); err != nil {
		return SessionEntry{}, EntrySecrets{}, err
	}
	return out, secrets, nil
}

// issueEntrySecrets issues a token expiring at tokenExpires and a handle
// capped at it, for sess at its current generation.
func issueEntrySecrets(ctx context.Context, tx *sql.Tx, sess Session, serviceID string, now, tokenExpires time.Time) (SessionEntry, EntrySecrets, error) {
	tok, token, err := issueToken(ctx, tx, sess, now, tokenExpires)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, err
	}
	h, handle, err := issueHandle(ctx, tx, sess, serviceID, now, tok.ExpiresAt)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, err
	}
	return SessionEntry{Session: sess, Token: tok, Handle: h},
		EntrySecrets{Token: NewSecret(token), Handle: NewSecret(handle)}, nil
}

// reissueEntry answers a retry of a committed entry. Only exactly the same
// input qualifies, only before the challenge's deadline, and only while the
// session is still active at the recorded generation. It deletes the token
// and handles of that session and generation, issues fresh ones and records
// the reissue in the audit, in one transaction; it never enters a session.
func (s *Store) reissueEntry(ctx context.Context, cmd command, in sessionEntryInput) (SessionEntry, EntrySecrets, error) {
	input, digest, err := inputDigest(cmd.input)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var prevDigest, prevResult string
	if err := tx.QueryRowContext(ctx,
		`SELECT input_digest, result FROM receipts
		 WHERE caller_kind = ? AND caller_id = ? AND operation = ? AND scope = ? AND key = ?`,
		entrant.kind.String(), entrant.id, cmd.op, cmd.scope, cmd.key).Scan(&prevDigest, &prevResult); err != nil {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("read receipt: %w", err)
	}
	if prevDigest != digest {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("%w: %s key %q", ErrIdempotencyConflict, cmd.op, cmd.key)
	}
	var recorded SessionEntry
	if err := json.Unmarshal([]byte(prevResult), &recorded); err != nil {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("decode recorded entry: %w", err)
	}
	now := s.now()
	ch, err := getChallenge(ctx, tx, in.ChallengeID)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, err
	}
	if !now.Before(ch.ExpiresAt) {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("challenge %s expired %s; enter again with a new proof: %w",
			ch.ID, ch.ExpiresAt.Format(time.RFC3339), ErrChallengeInvalid)
	}
	sess, err := getSession(ctx, tx, recorded.Session.ID)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, err
	}
	if sess.State != SessionActive || sess.Generation != recorded.Session.Generation {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("session %s is %s at generation %d, not active at %d: %w",
			sess.ID, sess.State, sess.Generation, recorded.Session.Generation, ErrContextStale)
	}
	for _, stmt := range []string{
		`DELETE FROM session_tokens WHERE session_id = ? AND generation = ?`,
		`DELETE FROM session_handles WHERE session_id = ? AND generation = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, sess.ID, sess.Generation); err != nil {
			return SessionEntry{}, EntrySecrets{}, fmt.Errorf("revoke lost secrets: %w", err)
		}
	}
	entry, secrets, err := issueEntrySecrets(ctx, tx, sess, in.ServiceID, now, recorded.Token.ExpiresAt)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, err
	}
	entry.Session, entry.Rotated = recorded.Session, recorded.Rotated
	result, err := json.Marshal(entry)
	if err != nil {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("encode result: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit (at, caller_kind, caller_id, operation, scope, key, input_digest, input, result)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(now), entrant.kind.String(), entrant.id, opReissueEntry, cmd.scope, cmd.key, digest, input, string(result)); err != nil {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("write audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SessionEntry{}, EntrySecrets{}, fmt.Errorf("commit: %w", err)
	}
	return entry, secrets, nil
}

// issueToken issues a session token for sess at its current generation,
// expiring at expires, and drops the session's tokens of earlier
// generations and expired ones, which can never be valid again.
func issueToken(ctx context.Context, tx *sql.Tx, sess Session, now, expires time.Time) (SessionToken, string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return SessionToken{}, "", err
	}
	secret := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	at := formatTime(now)
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_tokens WHERE session_id = ? AND (generation < ? OR expires_at <= ?)`,
		sess.ID, sess.Generation, at); err != nil {
		return SessionToken{}, "", fmt.Errorf("prune tokens: %w", err)
	}
	tok := SessionToken{SessionID: sess.ID, Generation: sess.Generation, IssuedAt: now, ExpiresAt: expires}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_tokens (digest, session_id, generation, issued_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		secretDigest(secret), tok.SessionID, tok.Generation, at, formatTime(tok.ExpiresAt)); err != nil {
		return SessionToken{}, "", fmt.Errorf("insert token: %w", err)
	}
	return tok, secret, nil
}

// storedToken is a token row: the session and generation it is bound to.
type storedToken struct {
	sessionID  string
	generation int64
	expiresAt  time.Time
}

// lookupToken finds the row of a well-formed token, valid or not.
func lookupToken(ctx context.Context, q querier, token string) (storedToken, error) {
	if !tokenShape.MatchString(token) {
		return storedToken{}, ErrTokenInvalid
	}
	var (
		t       storedToken
		expires string
	)
	err := q.QueryRowContext(ctx,
		`SELECT session_id, generation, expires_at FROM session_tokens WHERE digest = ?`, secretDigest(token)).
		Scan(&t.sessionID, &t.generation, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return storedToken{}, ErrTokenInvalid
	}
	if err != nil {
		return storedToken{}, fmt.Errorf("read token: %w", err)
	}
	if t.expiresAt, err = parseTime(expires); err != nil {
		return storedToken{}, err
	}
	return t, nil
}

// tokenBinding returns what a valid token stands for at now, or
// ErrTokenInvalid. Valid means unexpired, its session active at the token's
// generation, the session bound to the agent's current linked credential and
// the agent still a member.
func tokenBinding(ctx context.Context, q querier, token string, now time.Time) (TokenBinding, error) {
	t, err := lookupToken(ctx, q, token)
	if err != nil {
		return TokenBinding{}, err
	}
	if !now.Before(t.expiresAt) {
		return TokenBinding{}, ErrTokenInvalid
	}
	sess, err := getSession(ctx, q, t.sessionID)
	if err != nil {
		return TokenBinding{}, err
	}
	if sess.State != SessionActive || sess.Generation != t.generation {
		return TokenBinding{}, ErrTokenInvalid
	}
	agent, err := getAgent(ctx, q, sess.AgentID)
	if err != nil {
		return TokenBinding{}, err
	}
	if agent.Linked == nil || agent.Linked.TokenID != sess.TokenID {
		return TokenBinding{}, ErrTokenInvalid
	}
	if _, err := getMembership(ctx, q, sess.TeamID, sess.AgentID); errors.Is(err, ErrNotFound) {
		return TokenBinding{}, ErrTokenInvalid
	} else if err != nil {
		return TokenBinding{}, err
	}
	return TokenBinding{AgentID: sess.AgentID, TeamID: sess.TeamID, SessionID: sess.ID, Generation: sess.Generation,
		Role: sess.Role, HubID: agent.Linked.HubID, UserID: agent.Linked.UserID, TokenID: sess.TokenID,
		ExpiresAt: t.expiresAt}, nil
}

// AuthenticateSessionToken returns what a valid session token stands for, or
// ErrTokenInvalid. It reads one snapshot and writes nothing.
func (s *Store) AuthenticateSessionToken(ctx context.Context, token string) (TokenBinding, error) {
	var b TokenBinding
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		b, err = tokenBinding(ctx, q, token, s.now())
		return err
	})
	return b, err
}

// requireToken rechecks, inside a command's transaction, that token is still
// valid for sessionID at generation.
func (s *Store) requireToken(token, sessionID string, generation int64, into *TokenBinding) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		b, err := tokenBinding(ctx, tx, token, s.now())
		if err != nil {
			return err
		}
		if b.SessionID != sessionID || b.Generation != generation {
			return ErrTokenInvalid
		}
		if into != nil {
			*into = b
		}
		return nil
	}
}

// RefreshHandle issues a new aimem-scoped handle for the token's session,
// under IssueSessionHandle's refresh rules and never past the token's expiry.
// serviceID is this aicrew service, from the trusted caller's configuration.
// The token must be valid, a replay included; a replay of the same key
// returns the metadata and an empty handle, and a lost reply is recovered by
// refreshing again with a new key.
func (s *Store) RefreshHandle(ctx context.Context, key, token, serviceID string) (SessionHandle, string, error) {
	t, agentID, err := s.tokenOwner(ctx, token)
	if err != nil {
		return SessionHandle{}, "", err
	}
	c := Caller{kind: callerAgent, id: agentID}
	var (
		out    SessionHandle
		handle string
		bound  TokenBinding
	)
	valid := s.requireToken(token, t.sessionID, t.generation, &bound)
	err = s.run(ctx, c, command{
		op: opRefreshHandle, scope: t.sessionID, key: key, input: struct {
			SessionID  string `json:"session_id"`
			Generation int64  `json:"generation"`
			ServiceID  string `json:"service_id"`
		}{t.sessionID, t.generation, serviceID},
		authorize: requireAgent,
		validate: func() error {
			if !peerIDShape.MatchString(serviceID) {
				return fmt.Errorf("%w: service ID must be 1 to 128 characters from [A-Za-z0-9._:-]", ErrInvalid)
			}
			return nil
		},
		replayCheck: func(ctx context.Context, tx *sql.Tx, _ string) error { return valid(ctx, tx) },
		check:       valid,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, t.sessionID)
			if err != nil {
				return nil, err
			}
			h, secret, err := issueHandle(ctx, tx, sess, serviceID, now, bound.ExpiresAt)
			if err != nil {
				return nil, err
			}
			handle = secret
			return h, nil
		},
	}, &out)
	if err != nil {
		return SessionHandle{}, "", err
	}
	return out, handle, nil
}

// tokenOwner finds the session and agent a well-formed, stored token names,
// valid or not. Commands then check its validity inside their transaction.
func (s *Store) tokenOwner(ctx context.Context, token string) (storedToken, string, error) {
	var (
		t       storedToken
		agentID string
	)
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		if t, err = lookupToken(ctx, q, token); err != nil {
			return err
		}
		sess, err := getSession(ctx, q, t.sessionID)
		if err != nil {
			return err
		}
		agentID = sess.AgentID
		return nil
	})
	return t, agentID, err
}

// LeaveWithToken ends the token's session under LeaveSession's rules,
// work_outstanding included. The token must be valid, except that a retry
// with the same key of a leave that already committed replays its recorded
// result: that leave is what ended the token.
func (s *Store) LeaveWithToken(ctx context.Context, key, token string) (Session, error) {
	t, agentID, err := s.tokenOwner(ctx, token)
	if err != nil {
		return Session{}, err
	}
	c := Caller{kind: callerAgent, id: agentID}
	cmd := leaveCommand(c, key, t.sessionID, t.generation)
	var out Session
	if done, err := s.receiptExists(ctx, c, cmd); err != nil {
		return Session{}, err
	} else if !done {
		leaveCheck, valid := cmd.check, s.requireToken(token, t.sessionID, t.generation, nil)
		cmd.check = func(ctx context.Context, tx *sql.Tx) error {
			if err := valid(ctx, tx); err != nil {
				return err
			}
			return leaveCheck(ctx, tx)
		}
	}
	err = s.run(ctx, c, cmd, &out)
	return out, err
}
