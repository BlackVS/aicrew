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
// Trust boundary: the requirement and the references are supplied by a
// trusted internal caller that read them from the project's process and the
// delivery systems. Arbitrary external callers must never supply them. A
// reference names where a check can be found; neither the reference, nor
// aicrew recording it, nor aimem storing it proves that the check passed.

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

// check requires a stated requirement and a non-empty reference for every
// required kind.
func (d TrustedDelivery) check() error {
	if len(d.Required) == 0 {
		return fmt.Errorf("%w: the project's delivery requirement is missing", ErrInvalid)
	}
	have := map[string]bool{}
	for _, e := range d.Evidence {
		if !validRefs(e.Kind, e.Ref) {
			return fmt.Errorf("%w: delivery evidence needs a kind and a reference", ErrInvalid)
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
