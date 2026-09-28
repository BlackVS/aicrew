package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Session introspection for aimem (identity.v1 §3; docs/CREW-CONTRACT.md,
// "Session introspection").
//
// Aimem asks aicrew, over an authenticated call, whether an aimem-scoped
// session handle names a current aicrew session. Aicrew issues those
// handles to a session's own agent and stores only their digests. A handle
// is bound to one aimem hub, this service, the session and the session's
// generation; it lives at most HandleLifetime. Issuing a handle again for the
// same session and generation is the refresh: the handles it replaces stay
// valid for at most HandleOverlap more, never past their own expiry. Any
// change of generation, and the end of the session, makes every handle of
// the session inactive, because a handle is active only while its session is
// active at its generation. Nothing revokes a handle separately.
//
// Aimem authenticates with an introspection credential that aicrew issues,
// bound to one hub. Aicrew stores only its digest; the bearer is returned
// once. At most maxActiveCredentials are active per hub, so a rotation can
// overlap.
//
// A bearer or a handle is never part of a command's recorded result: the
// audit and the receipt keep metadata only, and a replay under the same key
// returns that metadata without the secret. A lost reply is recovered by
// issuing again.

// ErrUnauthenticated refuses an introspection credential that is unknown,
// revoked or expired.
var ErrUnauthenticated = errors.New("peer_unauthenticated")

// OpIntrospection and OpCoordination are the operations an introspection
// credential may permit: session introspection (identity.v1 §3) and
// coordination facts (coordination.v1). A credential permits one or both.
const (
	OpIntrospection = "crew.introspection"
	OpCoordination  = "crew.coordination"
)

// peerOperations is every operation, in their stored order.
var peerOperations = []string{OpCoordination, OpIntrospection}

// peerOps returns the operations to record for a new credential: both by
// default, since aimem uses one credential for both routes.
func peerOps(ops []string) ([]string, error) {
	if len(ops) == 0 {
		return slices.Clone(peerOperations), nil
	}
	want := map[string]bool{}
	for _, op := range ops {
		if !slices.Contains(peerOperations, op) {
			return nil, fmt.Errorf("%w: unknown operation %q; use %s", ErrInvalid, op, strings.Join(peerOperations, " or "))
		}
		want[op] = true
	}
	var out []string
	for _, op := range peerOperations {
		if want[op] {
			out = append(out, op)
		}
	}
	return out, nil
}

// ErrCredentialLimit refuses a third active introspection credential for a
// hub.
var ErrCredentialLimit = errors.New("credential_limit")

const (
	// HandleLifetime and HandleOverlap are identity.v1's bounds.
	HandleLifetime = 15 * time.Minute
	HandleOverlap  = 60 * time.Second
	// CredentialLifetime bounds an introspection credential.
	CredentialLifetime   = 366 * 24 * time.Hour
	maxActiveCredentials = 2

	handlePrefix     = "acs1_"
	credentialPrefix = "aicrew_introspect_"

	opIssueCredential  = "introspection.credential.issue"
	opRevokeCredential = "introspection.credential.revoke"
	opIssueHandle      = "session.handle.issue"
)

var (
	handleShape     = regexp.MustCompile(`^acs1_[A-Za-z0-9_-]{43}$`)
	credentialShape = regexp.MustCompile(`^aicrew_introspect_[0-9a-f]{64}$`)
	// peerIDShape is identity.v1's ID shape.
	peerIDShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

func secretDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// IntrospectionCredential is a credential's metadata; never the bearer.
type IntrospectionCredential struct {
	ID        string    `json:"id"`
	HubID     string    `json:"hub_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
	// Operations are the operations the credential permits.
	Operations []string `json:"operations"`
}

// Permits reports whether the credential permits the operation.
func (c IntrospectionCredential) Permits(op string) bool {
	return slices.Contains(c.Operations, op)
}

// Active reports whether the credential authenticates at now.
func (c IntrospectionCredential) Active(now time.Time) bool {
	return c.RevokedAt.IsZero() && now.Before(c.ExpiresAt)
}

// IssueIntrospectionCredential issues aimem's introspection credential for
// one hub, permitting the given operations (both when none are given). The
// operator only. It returns the metadata and the bearer, which exists nowhere
// else: a replay of the same key returns the metadata and an empty bearer.
func (s *Store) IssueIntrospectionCredential(ctx context.Context, c Caller, key, hubID string, ops ...string) (IntrospectionCredential, string, error) {
	var out IntrospectionCredential
	var bearer string
	permitted, opsErr := peerOps(ops)
	err := s.run(ctx, c, command{
		op: opIssueCredential, scope: hubID, key: key, input: struct {
			HubID      string   `json:"hub_id"`
			Operations []string `json:"operations"`
		}{hubID, permitted},
		authorize: requireOperator,
		validate: func() error {
			if !peerIDShape.MatchString(hubID) {
				return fmt.Errorf("%w: hub ID must be 1 to 128 characters from [A-Za-z0-9._:-]", ErrInvalid)
			}
			return opsErr
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			var active int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM introspection_credentials WHERE hub_id = ? AND revoked_at = '' AND expires_at > ?`,
				hubID, formatTime(now)).Scan(&active); err != nil {
				return nil, fmt.Errorf("count credentials: %w", err)
			}
			if active >= maxActiveCredentials {
				return nil, fmt.Errorf("hub %s has %d active introspection credentials: %w", hubID, active, ErrCredentialLimit)
			}
			var raw [32]byte
			if _, err := rand.Read(raw[:]); err != nil {
				return nil, err
			}
			secret := credentialPrefix + hex.EncodeToString(raw[:])
			id, err := newID(now)
			if err != nil {
				return nil, err
			}
			cred := IntrospectionCredential{ID: id, HubID: hubID, CreatedAt: now, ExpiresAt: now.Add(CredentialLifetime),
				Operations: permitted}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO introspection_credentials (id, hub_id, digest, created_at, expires_at, operations)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				cred.ID, hubID, secretDigest(secret), formatTime(cred.CreatedAt), formatTime(cred.ExpiresAt),
				strings.Join(permitted, ",")); err != nil {
				return nil, fmt.Errorf("insert credential: %w", err)
			}
			bearer = secret
			return cred, nil
		},
	}, &out)
	if err != nil {
		return IntrospectionCredential{}, "", err
	}
	return out, bearer, nil
}

// RevokeIntrospectionCredential revokes one credential. The operator only.
// Revoking a revoked credential changes nothing.
func (s *Store) RevokeIntrospectionCredential(ctx context.Context, c Caller, key, id string) (IntrospectionCredential, error) {
	var out IntrospectionCredential
	err := s.run(ctx, c, command{
		op: opRevokeCredential, scope: id, key: key, input: struct {
			ID string `json:"id"`
		}{id},
		authorize: requireOperator,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE introspection_credentials SET revoked_at = ? WHERE id = ? AND revoked_at = ''`,
				formatTime(now), id); err != nil {
				return nil, fmt.Errorf("revoke credential: %w", err)
			}
			return getCredential(ctx, tx, id)
		},
	}, &out)
	return out, err
}

// ListIntrospectionCredentials lists every credential's metadata, newest
// first. The operator only.
func (s *Store) ListIntrospectionCredentials(ctx context.Context, c Caller) ([]IntrospectionCredential, error) {
	if err := requireOperator(c); err != nil {
		return nil, err
	}
	var out []IntrospectionCredential
	err := s.snapshot(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT `+credentialColumns+` FROM introspection_credentials ORDER BY created_at DESC, id DESC`)
		if err != nil {
			return fmt.Errorf("list credentials: %w", err)
		}
		defer rows.Close()
		out = []IntrospectionCredential{}
		for rows.Next() {
			cred, err := scanCredential(rows)
			if err != nil {
				return err
			}
			out = append(out, cred)
		}
		return rows.Err()
	})
	return out, err
}

// AuthenticateIntrospection returns the hub an introspection bearer is
// bound to, or ErrUnauthenticated, including for a credential that does not
// permit introspection.
func (s *Store) AuthenticateIntrospection(ctx context.Context, bearer string) (string, error) {
	return s.authenticatePeer(ctx, bearer, OpIntrospection)
}

// AuthenticateCoordination returns the hub a bearer is bound to if it
// permits coordination facts, or ErrUnauthenticated: a credential without the
// operation answers exactly like an unknown one.
func (s *Store) AuthenticateCoordination(ctx context.Context, bearer string) (string, error) {
	return s.authenticatePeer(ctx, bearer, OpCoordination)
}

func (s *Store) authenticatePeer(ctx context.Context, bearer, op string) (string, error) {
	if !credentialShape.MatchString(bearer) {
		return "", ErrUnauthenticated
	}
	var cred IntrospectionCredential
	err := s.snapshot(ctx, func(q querier) error {
		row := q.QueryRowContext(ctx,
			`SELECT `+credentialColumns+` FROM introspection_credentials WHERE digest = ?`,
			secretDigest(bearer))
		var err error
		cred, err = scanCredential(row)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return "", ErrUnauthenticated
	}
	if err != nil {
		return "", err
	}
	if !cred.Active(s.now()) || !cred.Permits(op) {
		return "", ErrUnauthenticated
	}
	return cred.HubID, nil
}

type rowScanner interface{ Scan(dest ...any) error }

const credentialColumns = `id, hub_id, created_at, expires_at, revoked_at, operations`

func scanCredential(r rowScanner) (IntrospectionCredential, error) {
	var (
		c                              IntrospectionCredential
		created, expires, revoked, ops string
	)
	if err := r.Scan(&c.ID, &c.HubID, &created, &expires, &revoked, &ops); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c, fmt.Errorf("introspection credential: %w", ErrNotFound)
		}
		return c, fmt.Errorf("read credential: %w", err)
	}
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return c, err
	}
	if c.ExpiresAt, err = parseTime(expires); err != nil {
		return c, err
	}
	if revoked != "" {
		if c.RevokedAt, err = parseTime(revoked); err != nil {
			return c, err
		}
	}
	c.Operations = strings.Split(ops, ",")
	return c, nil
}

func getCredential(ctx context.Context, q querier, id string) (IntrospectionCredential, error) {
	return scanCredential(q.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM introspection_credentials WHERE id = ?`, id))
}

// SessionHandle is an issued handle's metadata; never the handle.
type SessionHandle struct {
	SessionID  string    `json:"session_id"`
	Generation int64     `json:"generation"`
	HubID      string    `json:"hub_id"`
	ServiceID  string    `json:"service_id"`
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// IssueSessionHandle issues an aimem-scoped handle for the caller's own
// session at its current generation, bound to the agent's linked aimem hub
// and to serviceID, this aicrew service. The trusted caller supplies
// serviceID from its configuration. Issuing again for the same session and
// generation refreshes: earlier handles stay valid for at most HandleOverlap
// more. It returns the metadata and the handle, which exists nowhere else: a
// replay of the same key returns the metadata and an empty handle.
func (s *Store) IssueSessionHandle(ctx context.Context, c Caller, key, sessionID string, generation int64, serviceID string) (SessionHandle, string, error) {
	var out SessionHandle
	var handle string
	err := s.run(ctx, c, command{
		op: opIssueHandle, scope: sessionID, key: key, input: struct {
			SessionID  string `json:"session_id"`
			Generation int64  `json:"generation"`
			ServiceID  string `json:"service_id"`
		}{sessionID, generation, serviceID},
		authorize: requireAgent,
		validate: func() error {
			if !peerIDShape.MatchString(serviceID) {
				return fmt.Errorf("%w: service ID must be 1 to 128 characters from [A-Za-z0-9._:-]", ErrInvalid)
			}
			return nil
		},
		replayCheck: sessionCurrent(c, sessionID, generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, sessionID, generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, sessionID)
			if err != nil {
				return nil, err
			}
			h, secret, err := issueHandle(ctx, tx, sess, serviceID, now, time.Time{})
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

// issueHandle issues a handle for sess at its current generation, bound to
// the agent's linked hub and to serviceID, and supersedes the session's
// earlier handles of that generation. It expires after HandleLifetime, or at
// notAfter if that is sooner (the session token's expiry). It returns the
// metadata and the handle.
func issueHandle(ctx context.Context, tx *sql.Tx, sess Session, serviceID string, now, notAfter time.Time) (SessionHandle, string, error) {
	agent, err := getAgent(ctx, tx, sess.AgentID)
	if err != nil {
		return SessionHandle{}, "", err
	}
	if agent.Linked == nil {
		return SessionHandle{}, "", fmt.Errorf("agent %s: %w", agent.ID, ErrIdentityLinkRequired)
	}
	if agent.Linked.TokenID != sess.TokenID {
		return SessionHandle{}, "", fmt.Errorf("session %s is bound to an earlier credential: %w", sess.ID, ErrContextStale)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return SessionHandle{}, "", err
	}
	secret := handlePrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	at := formatTime(now)
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_handles WHERE session_id = ? AND expires_at <= ?`, sess.ID, at); err != nil {
		return SessionHandle{}, "", fmt.Errorf("prune handles: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE session_handles SET superseded_at = ? WHERE session_id = ? AND generation = ? AND superseded_at = ''`,
		at, sess.ID, sess.Generation); err != nil {
		return SessionHandle{}, "", fmt.Errorf("supersede handles: %w", err)
	}
	expires := now.Add(HandleLifetime)
	if !notAfter.IsZero() && notAfter.Before(expires) {
		expires = notAfter
	}
	h := SessionHandle{SessionID: sess.ID, Generation: sess.Generation, HubID: agent.Linked.HubID,
		ServiceID: serviceID, IssuedAt: now, ExpiresAt: expires}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_handles (digest, session_id, generation, hub_id, service_id, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		secretDigest(secret), h.SessionID, h.Generation, h.HubID, h.ServiceID, at, formatTime(h.ExpiresAt)); err != nil {
		return SessionHandle{}, "", fmt.Errorf("insert handle: %w", err)
	}
	return h, secret, nil
}

// Introspection is aicrew's answer about one handle. When Active is false
// every other field is empty: the answer carries no reason.
type Introspection struct {
	Active     bool
	UserID     string
	TokenID    string
	AgentID    string
	TeamID     string
	Role       Role
	SessionID  string
	Generation int64
	// ExpiresAt is when the handle stops being active: its expiry, or the
	// end of its refresh overlap, whichever is first.
	ExpiresAt time.Time
}

// Introspect answers whether handle names a current session for hubID and
// serviceID, from aicrew's own state only. It reads one snapshot and writes
// nothing. An error means the store could not answer, never that the handle
// is inactive.
func (s *Store) Introspect(ctx context.Context, handle, hubID, serviceID string) (Introspection, error) {
	if !handleShape.MatchString(handle) {
		return Introspection{}, nil
	}
	now := s.now()
	var out Introspection
	err := s.snapshot(ctx, func(q querier) error {
		var (
			h                           SessionHandle
			issued, expires, superseded string
		)
		err := q.QueryRowContext(ctx,
			`SELECT session_id, generation, hub_id, service_id, issued_at, expires_at, superseded_at
			 FROM session_handles WHERE digest = ?`, secretDigest(handle)).
			Scan(&h.SessionID, &h.Generation, &h.HubID, &h.ServiceID, &issued, &expires, &superseded)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read handle: %w", err)
		}
		if h.HubID != hubID || h.ServiceID != serviceID {
			return nil
		}
		until, err := parseTime(expires)
		if err != nil {
			return err
		}
		if superseded != "" {
			replaced, err := parseTime(superseded)
			if err != nil {
				return err
			}
			if end := replaced.Add(HandleOverlap); end.Before(until) {
				until = end
			}
		}
		if !now.Before(until) {
			return nil
		}
		sess, err := getSession(ctx, q, h.SessionID)
		if err != nil {
			return err
		}
		// Ending a session and removing its member both advance the
		// generation too; the state and membership checks state the rule
		// directly rather than rely on that.
		if sess.State != SessionActive || sess.Generation != h.Generation {
			return nil
		}
		agent, err := getAgent(ctx, q, sess.AgentID)
		if err != nil {
			return err
		}
		if agent.Linked == nil || agent.Linked.HubID != hubID || agent.Linked.TokenID != sess.TokenID {
			return nil
		}
		if _, err := getMembership(ctx, q, sess.TeamID, sess.AgentID); errors.Is(err, ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		out = Introspection{Active: true, UserID: agent.Linked.UserID, TokenID: sess.TokenID, AgentID: sess.AgentID,
			TeamID: sess.TeamID, Role: sess.Role, SessionID: sess.ID, Generation: sess.Generation, ExpiresAt: until}
		return nil
	})
	if err != nil {
		return Introspection{}, err
	}
	return out, nil
}
