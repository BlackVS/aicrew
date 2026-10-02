package server

import (
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

// Every example body the agent's role guidance shows decodes strictly into
// aicrewd's body for its route, so a renamed or removed field fails here
// before a member's guidance teaches it (pilot G2).
func TestRoleGuidanceBodiesDecode(t *testing.T) {
	route := map[string]func() any{
		"offer":            func() any { return &offerBody{} },
		"claim":            func() any { return &claimBody{} },
		"accept":           func() any { return &acceptBody{} },
		"stop":             func() any { return &stopBody{} },
		"release":          func() any { return &releaseBody{} },
		"review":           func() any { return &reviewBody{} },
		"confirm-delivery": func() any { return &deliveryBody{} },
		"finalize":         func() any { return &finalizeBody{} },
		"work":             func() any { return &workBody{} },
	}
	bodies := agent.GuidanceBodies()
	if len(bodies) == 0 {
		t.Fatal("the guidance shows no example bodies")
	}
	for op, examples := range bodies {
		newBody, ok := route[op]
		if !ok {
			t.Errorf("%s: the guidance shows a body, but its route takes none", op)
			continue
		}
		for _, b := range examples {
			if err := decodeStrict([]byte(b), newBody()); err != nil {
				t.Errorf("%s: %v\n%s", op, err, b)
			}
		}
	}
}
