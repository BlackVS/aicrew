package server

import (
	"context"

	st "github.com/BlackVS/aicrew/internal/store"
)

// Fixtures of TestExposureGuardRejectsBypasses. Each gets past a guard that
// matches the selector shapes s.store.Op and store.OperatorCaller; the guard
// that resolves identifiers through go/types rejects every one.

// An aliased import constructs the operator's caller outside the wrapper.
func bypassAliasedCaller() (st.Caller, error) { return st.OperatorCaller("operator") }

// An aliased import constructs an agent's caller.
func bypassAliasedAgent() (st.Caller, error) { return st.AgentCaller("agent") }

// A local variable holds the store, and calls the operator's operations
// outside the operator API.
func (s *Server) bypassLocalStore(ctx context.Context, c st.Caller) error {
	db := s.store
	if _, err := db.RenameTeam(ctx, c, "key", "team", 1, "name"); err != nil {
		return err
	}
	_, err := db.IssueInvitation(ctx, c, "key", st.InvitationRequest{}, st.NewSecret("code"))
	return err
}

// A method value of the store reaches the wrapper's operation elsewhere.
func (s *Server) bypassMethodValue(ctx context.Context) (st.Caller, error) {
	authenticate := s.store.AuthenticateArchitect
	return authenticate(ctx, "aar_x")
}

// A store function the guard does not know is refused until a review lists it.
func bypassUnknownFunction() bool { return st.ValidProcessIdentity(st.ProcessIdentity{}) }
