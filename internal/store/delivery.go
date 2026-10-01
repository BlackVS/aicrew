package store

import (
	"fmt"
	"strings"
)

// Delivery evidence for finalizing an attempt as DONE. The kinds of
// evidence a finalize needs come from the project's selected process, not
// from the attempt state machine (work.go), which only checks that every
// required kind is present for the accepted result.
//
// Trust boundary: on the member-driven path, a team member confirms the
// references for the accepted result (confirm.go), and a finalize uses only
// that confirmed record. On the one-shot path, the requirement and the
// references come from a trusted internal caller. A finalizer never supplies
// its own. A reference names where a check can be found; neither the
// reference, nor aicrew recording it, nor aimem storing it proves that the
// check passed.

// Evidence is one delivery reference, such as the reviewed head of a PR.
type Evidence struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// TrustedDelivery is the delivery requirement of the project's process and
// the evidence offered for it, from a trusted internal caller.
type TrustedDelivery struct {
	Required []string   `json:"required"`
	Evidence []Evidence `json:"evidence"`
}

// DevelopmentDelivery is the requirement of this project's current
// development workflow: a reviewed head, a human merge and green
// post-merge CI.
var DevelopmentDelivery = []string{"reviewed_head", "human_merge", "post_merge_ci"}

// maxOfferedEvidence is the most references a member or a trusted caller
// may give: a finalize adds the attempt's identity (withAttemptIdentity)
// and carries at most maxDeliveryEvidence to aimem.
const maxOfferedEvidence = maxDeliveryEvidence - 1

// withAttemptIdentity is the evidence a finalize of a carries (1aad G2): the
// delivery references, then one naming the attempt and the member that
// worked it. It is part of the terminal evidence, so the
// accepted_for_finalization evidence digest covers it.
func withAttemptIdentity(a Attempt, delivered []Evidence) []Evidence {
	out := append([]Evidence(nil), delivered...)
	return append(out, Evidence{Kind: "text", Ref: attemptIdentity(a.ID, a.WorkerAgentID)})
}

// attemptIdentity names an attempt and its member in a reference aimem keeps
// with the task's results.
func attemptIdentity(attemptID, agentID string) string {
	return "aicrew attempt " + attemptID + " by member " + agentID
}

// check requires a stated requirement and a reference for every required
// kind, at most maxDeliveryEvidence of them, each of a shape aimem accepts
// as terminal evidence (checkEvidenceRef).
func (d TrustedDelivery) check() error {
	if len(d.Required) == 0 {
		return fmt.Errorf("%w: the project's delivery requirement is missing", ErrInvalid)
	}
	if len(d.Evidence) > maxOfferedEvidence {
		return fmt.Errorf("%w: at most %d delivery references", ErrInvalid, maxOfferedEvidence)
	}
	have := map[string]bool{}
	for _, e := range d.Evidence {
		if !validRefs(e.Kind) {
			return fmt.Errorf("%w: delivery evidence needs a kind", ErrInvalid)
		}
		if err := checkEvidenceRef(e.Ref); err != nil {
			return err
		}
		have[e.Kind] = true
	}
	var missing []string
	for _, kind := range d.Required {
		if !have[kind] {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: delivery evidence is missing %s", ErrInvalid, strings.Join(missing, ", "))
	}
	return nil
}
