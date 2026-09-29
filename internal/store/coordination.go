package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// Coordination proofs are aicrew's side of aimem's coordination.v1 (aimem
// docs/DESIGN-AIFORGE-COORDINATION-WIRE.md, frozen at f6d6fc5). A step that
// needs an aicrew fact before aimem commits it carries a proof: an opaque,
// single-use reference to one pending intent. aimem asks aicrew about the
// proof, and aicrew answers the fact from its current state. The store keeps
// only the proof's digest; the proof itself exists only in memory, on its
// way to the member's mutation.

// ProofLifetime bounds a proof; it also ends as soon as its intent settles.
const ProofLifetime = 15 * time.Minute

const proofPrefix = "acp1_"

// FactKind is one of coordination.v1's six facts.
type FactKind string

const (
	FactOffer                   FactKind = "offer"
	FactAcceptedAttempt         FactKind = "accepted_attempt"
	FactNeverAccepted           FactKind = "never_accepted"
	FactStopped                 FactKind = "stopped"
	FactAcceptedForFinalization FactKind = "accepted_for_finalization"
	FactIndependentClaim        FactKind = "independent_claim"
)

// FactMember is the acting member a fact names: the one whose verified
// connection may send the step.
type FactMember struct {
	UserID     string
	AgentID    string
	TeamID     string
	Role       Role
	SessionID  string
	Generation int64
}

// FactWorker is the worker an offer names.
type FactWorker struct {
	UserID  string
	AgentID string
}

// Fact is the answer for one proof. An inactive fact carries nothing else:
// the reason is never disclosed.
type Fact struct {
	Active           bool
	Kind             FactKind
	Operation        ReservationOp
	Task             TaskRef
	RequestKeyDigest string
	Member           FactMember
	OfferRef         string
	AttemptRef       string
	IntendedWorker   *FactWorker
	// Process is the pin aicrew recorded for a step that starts work (an
	// offer, an acceptance or an independent claim), for aimem to check
	// against the project's current selection (coordination.v1 C5-w2).
	Process *TrustedProcess
	// EvidenceDigest is the e1_ digest of the terminal evidence a finalize
	// sends (C5-w3): set on accepted_for_finalization only.
	EvidenceDigest string
	ExpiresAt      time.Time
}

// requestKeyDigest is identity.v1's k1_ digest of a request key.
func requestKeyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// issueProof issues the proof for the attempt's pending intent, acted by the
// given session. It runs in the transaction that starts the intent.
func issueProof(ctx context.Context, tx *sql.Tx, a Attempt, kind FactKind, sessionID string, generation int64,
	now time.Time) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("coordination proof: %w", err)
	}
	proof := proofPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO coordination_proofs (digest, attempt_id, request_key, operation, kind, session_id, generation,
		        issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		secretDigest(proof), a.ID, a.PendingKey, string(a.PendingOp), string(kind), sessionID, generation,
		formatTime(now), formatTime(now.Add(ProofLifetime))); err != nil {
		return "", fmt.Errorf("record coordination proof: %w", err)
	}
	return proof, nil
}

// endProofs ends the attempt's live proofs once its intent has settled.
func endProofs(ctx context.Context, tx *sql.Tx, attemptID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE coordination_proofs SET ended_at = ? WHERE attempt_id = ? AND ended_at = ''`,
		formatTime(now), attemptID); err != nil {
		return fmt.Errorf("end coordination proofs: %w", err)
	}
	return nil
}

// CoordinationFact answers aimem's question about a proof, for the named
// hub, from one snapshot of current state. The proof's record only locates
// the intent: the fact is active only while that intent is still pending
// and every rule of its kind still holds.
func (s *Store) CoordinationFact(ctx context.Context, proof, hubID string) (Fact, error) {
	var out Fact
	err := s.snapshot(ctx, func(q querier) error {
		var p proofRecord
		err := q.QueryRowContext(ctx,
			`SELECT attempt_id, request_key, operation, kind, session_id, generation, expires_at, ended_at
			 FROM coordination_proofs WHERE digest = ?`, secretDigest(proof)).
			Scan(&p.attemptID, &p.key, &p.op, &p.kind, &p.sessionID, &p.generation, &p.expires, &p.ended)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read coordination proof: %w", err)
		}
		expires, err := parseTime(p.expires)
		if err != nil {
			return err
		}
		if p.ended != "" || !s.now().Before(expires) {
			return nil
		}
		a, err := getAttempt(ctx, q, p.attemptID)
		if err != nil {
			return err
		}
		if a.PendingKey != p.key || string(a.PendingOp) != p.op || a.Task.HubID != hubID {
			return nil
		}
		member, ok, err := actingMember(ctx, q, a, p, hubID)
		if err != nil || !ok {
			return err
		}
		f := Fact{Active: true, Kind: FactKind(p.kind), Operation: a.PendingOp, Task: a.Task,
			RequestKeyDigest: requestKeyDigest(p.key), Member: member, ExpiresAt: expires}
		if ok, err = f.fill(ctx, q, a, hubID); err != nil || !ok {
			return err
		}
		out = f
		return nil
	})
	if err != nil {
		return Fact{}, err
	}
	return out, nil
}

type proofRecord struct {
	attemptID, key, op, kind, sessionID string
	generation                          int64
	expires, ended                      string
}

// actingMember is the proof's acting session, if it is still active at the
// proof's generation, in the attempt's team, a member with its session's
// role, and linked on the hub with the credential its session is bound to.
func actingMember(ctx context.Context, q querier, a Attempt, p proofRecord, hubID string) (FactMember, bool, error) {
	sess, err := getSession(ctx, q, p.sessionID)
	if err != nil {
		return FactMember{}, false, err
	}
	if sess.State != SessionActive || sess.Generation != p.generation || sess.TeamID != a.TeamID {
		return FactMember{}, false, nil
	}
	m, err := getMembership(ctx, q, sess.TeamID, sess.AgentID)
	if errors.Is(err, ErrNotFound) {
		return FactMember{}, false, nil
	} else if err != nil {
		return FactMember{}, false, err
	}
	if m.Role != sess.Role {
		return FactMember{}, false, nil
	}
	agent, err := getAgent(ctx, q, sess.AgentID)
	if err != nil {
		return FactMember{}, false, err
	}
	if agent.Linked == nil || agent.Linked.HubID != hubID || agent.Linked.TokenID != sess.TokenID {
		return FactMember{}, false, nil
	}
	return FactMember{UserID: agent.Linked.UserID, AgentID: sess.AgentID, TeamID: sess.TeamID, Role: sess.Role,
		SessionID: sess.ID, Generation: sess.Generation}, true, nil
}

// fill applies the rule of the fact's kind to the attempt as it is now, and
// adds the references the kind carries. The intent is pending (its key and
// operation matched), perhaps reconciling after a lost reply: while the proof
// lives, the member may retry the same key, so the fact still answers.
func (f *Fact) fill(ctx context.Context, q querier, a Attempt, hubID string) (bool, error) {
	m := f.Member
	holder := m.AgentID == a.WorkerAgentID
	switch f.Kind {
	case FactOffer:
		team, err := getTeam(ctx, q, a.TeamID)
		if err != nil {
			return false, err
		}
		if a.Origin != OriginOffer || m.Role != RoleCoordinator || m.SessionID != a.CoordinatorSessionID ||
			team.CoordinatorGeneration != a.CoordinatorGeneration {
			return false, nil
		}
		worker, err := getAgent(ctx, q, a.WorkerAgentID)
		if err != nil {
			return false, err
		}
		if worker.Linked == nil || worker.Linked.HubID != hubID {
			return false, nil
		}
		f.OfferRef, f.Process = a.offerRef(), &a.Process
		f.IntendedWorker = &FactWorker{UserID: worker.Linked.UserID, AgentID: a.WorkerAgentID}
	case FactAcceptedAttempt:
		team, err := getTeam(ctx, q, a.TeamID)
		if err != nil {
			return false, err
		}
		if m.Role != RoleWorker || !holder ||
			team.CoordinatorGeneration != a.CoordinatorGeneration {
			return false, nil
		}
		f.OfferRef, f.AttemptRef, f.Process = a.offerRef(), a.attemptRef(), &a.Process
	case FactNeverAccepted:
		// Any active coordinator session is the team's current coordinator,
		// the one that made the offer or its successor.
		if a.Origin != OriginOffer || a.PendingFrom != AttemptOffered ||
			a.Stop != StopNone || m.Role != RoleCoordinator {
			return false, nil
		}
		f.OfferRef = a.offerRef()
	case FactStopped:
		if a.Stop != StopConfirmed || !holder ||
			(m.Role != RoleWorker && m.Role != RoleIndependent) {
			return false, nil
		}
		f.AttemptRef = a.attemptRef()
	case FactAcceptedForFinalization:
		reviewer := m.Role == RoleCoordinator && m.SessionID == a.AcceptedBySession && m.Generation == a.AcceptedByGeneration
		if a.Phase != PhaseAccepted || a.AcceptedResult == 0 ||
			!(holder || reviewer) {
			return false, nil
		}
		// The digest covers exactly the references the finalize sends; a
		// record they cannot be read from answers inactive.
		refs, err := pendingRefs(a)
		if err != nil {
			return false, nil
		}
		f.AttemptRef, f.EvidenceDigest = a.attemptRef(), evidenceDigest(refs)
	case FactIndependentClaim:
		if a.Origin != OriginClaim || m.Role != RoleIndependent || !holder ||
			m.SessionID != a.WorkerSessionID || m.Generation != a.WorkerGeneration {
			return false, nil
		}
		f.AttemptRef, f.Process = a.attemptRef(), &a.Process
	default:
		return false, nil
	}
	// A pinned fact carries only a pin in the hub selection's forms; one
	// recorded otherwise answers inactive rather than reach aimem malformed.
	if f.Process != nil && !ValidProcessIdentity(f.Process.Identity) {
		return false, nil
	}
	return true, nil
}
