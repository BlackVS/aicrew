package store

import (
	"context"
	"fmt"
)

// The aimem reservation service, as the store sees it (aimem reservation
// wire contract v1, docs/DESIGN-AIFORGE-RESERVATION-WIRE.md at aimem
// feb36ed). Aimem is the task authority: it decides who holds a task, and
// aicrew conforms to its receipts (docs/CREW-CONTRACT.md, "Attempts and the
// aimem reservation").
//
// Reservations is an internal port. In this increment only a test fake
// implements it; the real v1 client arrives with crew-execution b, once
// aimem's authorization and routes exist. An implementation owns the
// transport details the store does not hold, such as the full task content
// a release carries and the caller's verified context.

// ReservationOp names a reservation mutation.
type ReservationOp string

const (
	ReservationClaim    ReservationOp = "claim"
	ReservationTransfer ReservationOp = "transfer"
	ReservationRelease  ReservationOp = "release"
)

// ReservationHolder is the holder a claim or transfer asks for. Aicrew
// always uses the external mode, naming its own offer or attempt.
type ReservationHolder struct {
	Mode    string `json:"mode"`
	WorkRef string `json:"work_ref"`
}

// ReservationRequest is one v1 mutation. Task and RequestKey travel outside
// the JSON body (in the path and the idempotency key).
//
// CoordinationProof is an opaque reference. Aicrew attaches no authority to
// it, never checks it and never trusts one it receives; how aimem verifies
// a transition belongs to the reviewed context and authorization work.
type ReservationRequest struct {
	Task              TaskRef            `json:"-"`
	RequestKey        string             `json:"-"`
	ExpectedRevision  int64              `json:"expected_revision"`
	ReservationID     string             `json:"reservation_id,omitempty"`
	Fence             string             `json:"fence,omitempty"`
	Holder            *ReservationHolder `json:"holder,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	CoordinationProof string             `json:"coordination_proof,omitempty"`
}

// ReservationReceipt is aimem's record of a committed mutation.
type ReservationReceipt struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Operation    string `json:"operation"`
	RequestKey   string `json:"request_key"`
	Replayed     bool   `json:"replayed"`
	ActorID      string `json:"actor_id"`
	VerifiedMode string `json:"verified_mode"`
}

// ReservationState is the reservation after a mutation. A closed one has no
// ID and is not active; its fence stays advanced.
type ReservationState struct {
	ID         string `json:"id"`
	Fence      string `json:"fence"`
	Active     bool   `json:"active"`
	HolderMode string `json:"holder_mode,omitempty"`
	OwnWorkRef string `json:"own_work_ref,omitempty"`
}

// ReservationResult is a committed mutation's response.
type ReservationResult struct {
	Receipt      ReservationReceipt `json:"receipt"`
	TaskRevision int64              `json:"task_revision"`
	Reservation  ReservationState   `json:"reservation"`
}

// Receipt lookup states (wire contract, "Response, reconciliation and
// refusal").
const (
	ReceiptCommitted    = "committed"
	ReceiptNotCommitted = "not_committed"
	ReceiptUnresolved   = "unresolved"
)

// ReceiptLookup answers a receipt query for an earlier request key. Result
// is set only for a committed request.
type ReceiptLookup struct {
	State  string             `json:"state"`
	Result *ReservationResult `json:"result,omitempty"`
}

// HoldStatus is the caller's own hold on a task, or state "none".
type HoldStatus struct {
	State         string `json:"state"`
	ReservationID string `json:"reservation_id,omitempty"`
	Fence         string `json:"fence,omitempty"`
	OwnWorkRef    string `json:"own_work_ref,omitempty"`
	TaskRevision  int64  `json:"task_revision,omitempty"`
}

// ReservationRefusal is aimem's typed refusal. Only a refusal that is not
// retryable tells aicrew that nothing was committed; anything else, including
// a transport error or timeout, leaves the outcome unknown.
type ReservationRefusal struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	ActiveMode    string `json:"active_mode,omitempty"`
	Retryable     bool   `json:"retryable"`
	NextAction    string `json:"next_action"`
	CorrelationID string `json:"correlation_id"`
}

func (r *ReservationRefusal) Error() string {
	return fmt.Sprintf("aimem refused the reservation: %s (%s)", r.Code, r.Message)
}

// Reservations is the reservation service port.
type Reservations interface {
	// Mutate sends one mutation. The same request key and request must
	// return the recorded outcome of an earlier identical request.
	Mutate(ctx context.Context, op ReservationOp, req ReservationRequest) (ReservationResult, error)
	// Receipt looks up an earlier request by its operation and key.
	Receipt(ctx context.Context, task TaskRef, op ReservationOp, requestKey string) (ReceiptLookup, error)
	// Status reads the caller's own hold on a task.
	Status(ctx context.Context, task TaskRef) (HoldStatus, error)
}
