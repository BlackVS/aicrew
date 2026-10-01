package store

import (
	"errors"
	"testing"
)

// A work update advances the hold's fence by exactly one, as every aimem
// mutation does (aimem's task reservations at the b4 pin; found by b4a
// against a real hub). Its committed result is confirmed only at the next
// fence: the old fence, or a jump past it, stays unconfirmed. The read
// scope's receipt, which settles and reconciliation use, follows the same
// rule.
func TestUpdateAdvancesTheFenceByOne(t *testing.T) {
	a := Attempt{ID: "a1", Task: TaskRef{HubID: "h", ProjectID: "p", TaskID: "t"}, PendingOp: ReservationUpdate,
		PendingKey: "k", ReservationID: "r", Fence: "2", TaskRevision: 5}
	result := func(fence string) ReservationResult {
		return ReservationResult{
			Receipt:      ReservationReceipt{ID: "rc", State: ReceiptCommitted, Operation: string(ReservationUpdate), RequestKey: "k"},
			TaskRevision: 6,
			Reservation:  ReservationState{ID: "r", Fence: fence, Active: true, HolderMode: "external", OwnWorkRef: a.attemptRef()},
		}
	}
	for fence, want := range map[string]outcomeKind{"3": outcomeCommitted, "2": outcomeUnknown, "4": outcomeUnknown, "1": outcomeUnknown, "x": outcomeUnknown} {
		if got := classify(a, result(fence), nil).kind; got != want {
			t.Errorf("an update from fence 2 answered at fence %s: %s, want %s", fence, got, want)
		}
	}
	noFence := a
	noFence.Fence = ""
	if got := classify(noFence, result("0"), nil).kind; got != outcomeUnknown {
		t.Errorf("an attempt with no fence confirmed an update: %s", got)
	}

	receipt := func(fence string) *ScopeReceipt {
		return &ScopeReceipt{ID: "rc", Operation: string(ReservationUpdate), TaskID: "t", RequestKeyDigest: requestKeyDigest("k"),
			ReservationID: "r", Fence: fence, TaskRevision: 6, VerifiedMode: "team"}
	}
	if res, err := resultFromReceipt(a, receipt("3"), "k"); err != nil || res.Reservation.Fence != "3" {
		t.Errorf("the read scope's receipt at the next fence: %+v %v", res, err)
	}
	for _, fence := range []string{"2", "4"} {
		if _, err := resultFromReceipt(a, receipt(fence), "k"); !errors.Is(err, ErrOutcomeUnknown) {
			t.Errorf("the read scope's receipt at fence %s confirmed the update: %v", fence, err)
		}
	}
}
