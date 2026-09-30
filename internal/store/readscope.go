package store

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// ReservationReader is aimem's read-only reservation scope for this service
// (coordination.v1 §2, with C5c-w's closure evidence). Aicrew holds no
// credential that can change a reservation: the acting member's own
// connection sends each mutation, and aicrew confirms what happened only
// through this scope, which sees exactly the receipts and holds its own
// proofs established. The production client is internal/aimemread; tests
// use fakes built from the scope's fixture exchanges.
type ReservationReader interface {
	// ReceiptByProof returns the transition committed under a proof, by the
	// proof's p1_ digest, or state "none".
	ReceiptByProof(ctx context.Context, proofDigest string) (ScopeReceiptLookup, error)
	// ReceiptByKey returns the transition committed under a request key's k1_
	// digest on a reservation this service's proof established, or "none".
	ReceiptByKey(ctx context.Context, task TaskRef, op ReservationOp, keyDigest string) (ScopeReceiptLookup, error)
	// HoldStatus returns the task's hold if this service's proof set it
	// ("held"), this service's most recent reservation on the task if it is
	// no longer active ("closed"), or "none".
	HoldStatus(ctx context.Context, task TaskRef) (ScopeHold, error)
}

// Read-scope receipt and hold states.
const (
	ScopeCommitted = "committed"
	ScopeNone      = "none"
	ScopeHeld      = "held"
	ScopeClosed    = "closed"
)

// ScopeReceiptLookup is a read-scope receipt answer.
type ScopeReceiptLookup struct {
	State   string        `json:"state"`
	Receipt *ScopeReceipt `json:"receipt,omitempty"`
}

// ScopeReceipt is a committed transition as the read scope shows it: never
// task content, another holder, the raw request key or a proof.
type ScopeReceipt struct {
	ID               string `json:"id"`
	Operation        string `json:"operation"`
	TaskID           string `json:"task_id"`
	RequestKeyDigest string `json:"request_key_digest"`
	ReservationID    string `json:"reservation_id"`
	Fence            string `json:"fence"`
	TaskRevision     int64  `json:"task_revision"`
	MemberUserID     string `json:"member_user_id"`
	VerifiedMode     string `json:"verified_mode"`
	CommittedAt      string `json:"committed_at"`
}

// ScopeHold is a read-scope hold status: a held reservation, a closed one
// (C5c-w closure evidence), or none.
type ScopeHold struct {
	State         string `json:"state"`
	ReservationID string `json:"reservation_id,omitempty"`
	Fence         string `json:"fence,omitempty"`
	HolderMode    string `json:"holder_mode,omitempty"`
	OwnWorkRef    string `json:"own_work_ref,omitempty"`
	TaskRevision  int64  `json:"task_revision,omitempty"`
	ClosingFence  string `json:"closing_fence,omitempty"`
	ClosedBy      string `json:"closed_by,omitempty"`
	ClosedAt      string `json:"closed_at,omitempty"`
}

// proofDigestP1 is coordination.v1's p1_ digest of a proof, from the SHA-256
// the store keeps (hex) instead of the proof.
func proofDigestP1(hexDigest string) (string, error) {
	sum, err := hex.DecodeString(hexDigest)
	if err != nil || len(sum) != 32 {
		return "", fmt.Errorf("stored proof digest is malformed")
	}
	return "p1_" + base64.RawURLEncoding.EncodeToString(sum), nil
}
