package store

import "fmt"

type callerKind uint8

const (
	callerNone callerKind = iota
	callerOperator
	callerAgent
)

func (k callerKind) String() string {
	switch k {
	case callerOperator:
		return "operator"
	case callerAgent:
		return "agent"
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

func (c Caller) String() string {
	if c.kind == callerNone {
		return "none"
	}
	return c.kind.String() + ":" + c.id
}

// requireOperator is the only policy used by registry mutations. It looks at
// the caller kind alone: labels, model and client data never reach it.
func requireOperator(c Caller) error {
	if c.kind != callerOperator || c.id == "" {
		return fmt.Errorf("%w: operator required, caller is %s", ErrForbidden, c)
	}
	return nil
}

func validateCallerID(id string) error {
	if id == "" || len(id) > 128 {
		return fmt.Errorf("%w: caller id must be 1-128 characters", ErrInvalid)
	}
	for _, r := range id {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w: caller id contains whitespace or control characters", ErrInvalid)
		}
	}
	return nil
}
