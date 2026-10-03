// Package opapi is aicrewd's operator API: its routes and the JSON its
// requests and answers carry. aicrewd serves it on its HTTPS listener to the
// holder of the operator credential (package optoken); `aicrew` is its
// console client. Secrets travel only in an issue's answer, once: a
// credential's bearer and an invitation's code. Every other answer is
// metadata.
package opapi

import (
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// Routes. Reads take their filter in the query; writes take a JSON body.
const (
	Prefix = "/v1/admin/"

	CredentialsPath      = "/v1/admin/introspection-credentials"        // GET list (?hub=), POST issue
	CredentialRotatePath = "/v1/admin/introspection-credentials/rotate" // POST
	CredentialRevokePath = "/v1/admin/introspection-credentials/revoke" // POST
	TeamsPath            = "/v1/admin/teams"                            // GET list, POST create
	TeamPath             = "/v1/admin/team"                             // GET show (?id=)
	TeamProjectsPath     = "/v1/admin/team/projects"                    // POST
	TeamRenamePath       = "/v1/admin/team/rename"                      // POST
	InvitationsPath      = "/v1/admin/invitations"                      // GET list (?team=), POST issue
	InvitationRevokePath = "/v1/admin/invitations/revoke"               // POST
)

// Error is every refusal: a stable code and a message that never carries a
// secret.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Refusal codes beyond the store's validation.
const (
	CodeUnauthorized     = "unauthorized"
	CodeRateLimited      = "rate_limited"
	CodeUnavailable      = "operator_unavailable"
	CodeInvalid          = "invalid_request"
	CodeNotFound         = "not_found"
	CodeRevisionConflict = "revision_conflict"
	CodeTeamExists       = "team_exists"
	CodeCredentialLimit  = "credential_limit"
	CodeRotateNeedsOne   = "rotate_needs_one_active"
	CodeInvitationFinal  = "invitation_final"
	CodeInternal         = "internal_error"
)

// CredentialRequest issues or rotates an introspection credential. An empty
// Operations permits both introspection and coordination.
type CredentialRequest struct {
	HubID      string   `json:"hub_id"`
	Operations []string `json:"operations,omitempty"`
}

// IDRequest names one record to revoke.
type IDRequest struct {
	ID string `json:"id"`
}

// Credential is a credential's metadata. Bearer is set only in an issue or
// rotate answer, once.
type Credential struct {
	ID         string    `json:"id"`
	HubID      string    `json:"hub_id"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
	Operations []string  `json:"operations"`
	Bearer     string    `json:"bearer,omitempty"`
	Replaces   string    `json:"replaces,omitempty"`
}

// CredentialOf is a stored credential's metadata at now.
func CredentialOf(c store.IntrospectionCredential, now time.Time) Credential {
	return Credential{ID: c.ID, HubID: c.HubID, Active: c.Active(now), CreatedAt: c.CreatedAt,
		ExpiresAt: c.ExpiresAt, RevokedAt: c.RevokedAt, Operations: c.Operations}
}

// TeamRequest creates a team.
type TeamRequest struct {
	Name     string             `json:"name"`
	Projects []store.ProjectRef `json:"projects,omitempty"`
}

// TeamProjectsRequest replaces a team's intended projects.
type TeamProjectsRequest struct {
	TeamID           string             `json:"team_id"`
	ExpectedRevision int64              `json:"expected_revision"`
	Projects         []store.ProjectRef `json:"projects"`
}

// TeamRenameRequest renames a team.
type TeamRenameRequest struct {
	TeamID           string `json:"team_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Name             string `json:"name"`
}

// TeamDetail is a team and its current members.
type TeamDetail struct {
	store.Team
	Members []Member `json:"members"`
}

// Member is one current membership.
type Member struct {
	AgentID   string     `json:"agent_id"`
	Role      store.Role `json:"role"`
	Revision  int64      `json:"revision"`
	CreatedAt time.Time  `json:"created_at"`
}

// InvitationRequest issues an invitation. TTL is a Go duration ("24h");
// empty means the store's default.
type InvitationRequest struct {
	Purpose        string `json:"purpose"`
	TeamID         string `json:"team_id"`
	Role           string `json:"role"`
	HubID          string `json:"hub_id"`
	AgentID        string `json:"agent_id,omitempty"`
	ExpectedUserID string `json:"expected_user_id,omitempty"`
	Label          string `json:"label,omitempty"`
	TTL            string `json:"ttl,omitempty"`
}

// Invitation is an invitation's metadata. Code is set only in an issue
// answer, once.
type Invitation struct {
	ID             string    `json:"id"`
	Purpose        string    `json:"purpose"`
	TeamID         string    `json:"team_id"`
	Role           string    `json:"role"`
	HubID          string    `json:"hub_id"`
	AgentID        string    `json:"agent_id,omitempty"`
	ExpectedUserID string    `json:"expected_user_id,omitempty"`
	Label          string    `json:"label,omitempty"`
	IssuedBy       string    `json:"issued_by"`
	State          string    `json:"state"`
	Expired        bool      `json:"expired"`
	Attempts       int       `json:"attempts"`
	Revision       int64     `json:"revision"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	Code           string    `json:"code,omitempty"`
}

// InvitationOf is a stored invitation's metadata at now.
func InvitationOf(inv store.Invitation, now time.Time) Invitation {
	open := inv.State == store.InvitationIssued || inv.State == store.InvitationLocked
	return Invitation{ID: inv.ID, Purpose: string(inv.Purpose), TeamID: inv.TeamID, Role: string(inv.Role),
		HubID: inv.HubID, AgentID: inv.AgentID, ExpectedUserID: inv.ExpectedUserID, Label: inv.Label,
		IssuedBy: inv.IssuedBy, State: string(inv.State), Expired: open && !now.Before(inv.ExpiresAt),
		Attempts: inv.Attempts, Revision: inv.Revision, ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt}
}
