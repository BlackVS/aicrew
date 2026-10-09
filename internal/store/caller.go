package store

import (
	"fmt"
	"unicode/utf8"
)

type callerKind uint8

const (
	callerNone callerKind = iota
	callerOperator
	callerAgent
	// callerReconciler is aicrewd's own reconciler (crew-execution b3b): no
	// route constructs it, and it may only settle a pending step on the read
	// scope's answers and close an attempt as recovered.
	callerReconciler
	// callerArchitect is an architect credential (docs/DESIGN-CONTROL-PLANE.md,
	// D10): it reads and answers escalations, and nothing else.
	callerArchitect
)

func (k callerKind) String() string {
	switch k {
	case callerOperator:
		return "operator"
	case callerAgent:
		return "agent"
	case callerReconciler:
		return "reconciler"
	case callerArchitect:
		return "architect"
	default:
		return "none"
	}
}

// Caller is the trusted caller context of one store operation. The store
// enforces policy against it; it does not authenticate anyone. Whatever
// surface calls the store (none exists yet) must authenticate the request
// before constructing a Caller. The zero value has no authority.
type Caller struct {
	kind callerKind
	id   string
}

// OperatorCaller returns a caller with operator authority. Construct it only
// after the operator has been authenticated by a trusted surface.
func OperatorCaller(id string) (Caller, error) {
	if err := validateCallerID(id); err != nil {
		return Caller{}, err
	}
	return Caller{kind: callerOperator, id: id}, nil
}

// AgentCaller returns a caller acting as the given agent. In this increment
// agents may not change registry records; later increments add their own
// narrowly scoped operations.
func AgentCaller(agentID string) (Caller, error) {
	if err := validateCallerID(agentID); err != nil {
		return Caller{}, err
	}
	return Caller{kind: callerAgent, id: agentID}, nil
}

// ReconcilerCaller returns aicrewd's reconciler. Only the service's own
// reconciliation loop uses it; no request surface constructs it.
func ReconcilerCaller() Caller { return Caller{kind: callerReconciler, id: "aicrewd"} }

func (c Caller) String() string {
	if c.kind == callerNone {
		return "none"
	}
	return c.kind.String() + ":" + c.id
}

// requireAgent admits agent callers only. Operations using it also check
// that the agent acts on its own membership or session.
func requireAgent(c Caller) error {
	if c.kind != callerAgent || c.id == "" {
		return fmt.Errorf("%w: agent caller required, caller is %s", ErrForbidden, c)
	}
	return nil
}

// requireOperator is the only policy used by registry mutations. It looks at
// the caller kind alone: labels, model and client data never reach it.
func requireOperator(c Caller) error {
	if c.kind != callerOperator || c.id == "" {
		return fmt.Errorf("%w: operator required, caller is %s", ErrForbidden, c)
	}
	return nil
}

// requireEscalationReader admits the operator and an architect
// credential: the escalation reads and answers, the only operations an
// architect credential reaches.
func requireEscalationReader(c Caller) error {
	if (c.kind != callerOperator && c.kind != callerArchitect) || c.id == "" {
		return fmt.Errorf("%w: the operator or an architect credential is required, caller is %s", ErrForbidden, c)
	}
	return nil
}

// requireReconciler admits aicrewd's reconciler only.
func requireReconciler(c Caller) error {
	if c.kind != callerReconciler {
		return fmt.Errorf("%w: the reconciler is required, caller is %s", ErrForbidden, c)
	}
	return nil
}

// settleCallers admits every caller of the settle family: a member or the
// operator (mayReconcile decides which), and the reconciler.
func settleCallers(Caller) error { return nil }

func validateCallerID(id string) error {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) {
		return fmt.Errorf("%w: caller id must be 1-128 bytes of valid UTF-8", ErrInvalid)
	}
	for _, r := range id {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w: caller id contains whitespace or control characters", ErrInvalid)
		}
	}
	return nil
}
