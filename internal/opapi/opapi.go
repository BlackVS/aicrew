// Package opapi is aicrewd's operator API: its routes and the JSON its
// requests and answers carry. aicrewd serves it on its HTTPS listener to the
// holder of the operator credential (package optoken); `aicrew` is its
// console client. Secrets travel only in an issue's answer, once: a
// credential's bearer and an invitation's code. Every other answer is
// metadata.
//
// The package depends on nothing of aicrew's, so a client links neither the
// store nor its database driver: the service converts its records here.
package opapi

import "time"

// Routes. Reads take their filter in the query; writes take a JSON body.
const (
	Prefix = "/v1/admin/"

	// The credential a hub uses to call this service (introspection and
	// coordination facts).
	CredentialsPath      = "/v1/admin/hub-credentials"        // GET list (?hub=), POST issue
	CredentialRotatePath = "/v1/admin/hub-credentials/rotate" // POST
	CredentialRevokePath = "/v1/admin/hub-credentials/revoke" // POST
	TeamsPath            = "/v1/admin/teams"                  // GET list, POST create
	TeamPath             = "/v1/admin/team"                   // GET show (?id=)
	TeamRenamePath       = "/v1/admin/team/rename"            // POST
	TeamRegisterPath     = "/v1/admin/team/register"          // POST
	TeamGrantsPath       = "/v1/admin/team/grants"            // POST: read the team's grants live
	InvitationsPath      = "/v1/admin/invitations"            // GET list (?team=), POST issue
	InvitationRevokePath = "/v1/admin/invitations/revoke"     // POST

	// The credential routes' names before 0.3.0, served the same until
	// 0.6.0 removes them.
	LegacyCredentialsPath      = "/v1/admin/introspection-credentials"
	LegacyCredentialRotatePath = "/v1/admin/introspection-credentials/rotate"
	LegacyCredentialRevokePath = "/v1/admin/introspection-credentials/revoke"
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
	// CodeHubUnavailable is a hub that did not answer a live read, or
	// refused it; the message names the hub's code.
	CodeHubUnavailable = "hub_unavailable"
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

// Grant is a project the team's hub grants it, with the project's
// repository and selected process as the hub holds them.
type Grant struct {
	HubID      string           `json:"hub_id"`
	ProjectID  string           `json:"project_id"`
	Repository *GrantRepository `json:"repository,omitempty"`
	Process    *GrantProcess    `json:"process,omitempty"`
}

// GrantRepository is a granted project's repository binding.
type GrantRepository struct {
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Host   string `json:"host,omitempty"`
	Access string `json:"access,omitempty"`
}

// GrantProcess is a granted project's selected process pin.
type GrantProcess struct {
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Manifest string `json:"manifest"`
}

// Team is a team record.
type Team struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	Revision              int64     `json:"revision"`
	CoordinatorGeneration int64     `json:"coordinator_generation"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	// Grants are the projects the team's hub grants it, as aicrewd last
	// read them (GrantsState "enabled", "disabled" or "not_registered", at
	// GrantsReadAt); empty before any read. Offers and claims never use
	// this copy: each reads the hub live.
	Grants       []Grant    `json:"grants"`
	GrantsState  string     `json:"grants_state,omitempty"`
	GrantsReadAt *time.Time `json:"grants_read_at,omitempty"`
	// Hub is the alias of the team's aimem block, or "" for a team created
	// before teams named their hub.
	Hub string `json:"hub,omitempty"`
	// Registration is the outcome of the team's last registration on its
	// hub (team.register), or absent when none was attempted.
	Registration *TeamRegistration `json:"registration,omitempty"`
}

// TeamRegistration is the outcome of a team's registration on its hub:
// state "registered", or the hub's refusal code, or "hub_unavailable".
type TeamRegistration struct {
	State  string    `json:"state"`
	Detail string    `json:"detail,omitempty"`
	Name   string    `json:"name,omitempty"`
	At     time.Time `json:"at"`
}

// TeamSummary is a team in a list, with its current member count.
type TeamSummary struct {
	Team
	Members int `json:"members"`
}

// TeamRequest creates a team.
type TeamRequest struct {
	Name string `json:"name"`
	// Hub is the alias of an aimem block of aicrewd.json.
	Hub string `json:"hub,omitempty"`
}

// TeamRegisterRequest registers a team on its hub again. Hub names the hub
// of a team created before teams named one; a team keeps its hub.
type TeamRegisterRequest struct {
	ID  string `json:"id"`
	Hub string `json:"hub,omitempty"`
}

// TeamGrantsRequest reads a team's grants from its hub, live.
type TeamGrantsRequest struct {
	ID string `json:"id"`
}

// TeamGrants is a team as a live read of its hub left it, with the service
// ID the hub knows this service by: the peer a grant names.
type TeamGrants struct {
	Team
	ServiceID string `json:"service_id"`
}

// TeamRenameRequest renames a team.
type TeamRenameRequest struct {
	TeamID           string `json:"team_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Name             string `json:"name"`
}

// TeamDetail is a team and its current members.
type TeamDetail struct {
	Team
	Members []Member `json:"members"`
}

// Member is one current membership.
type Member struct {
	AgentID   string    `json:"agent_id"`
	Role      string    `json:"role"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
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
	TeamName       string    `json:"team_name,omitempty"` // the team's current name, beside its ID
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

// Escalations (docs/DESIGN-CONTROL-PLANE.md, section 7.5): the coordinator's
// requests and their answers. The routes take the operator credential or an
// architect credential, which reaches these routes and nothing else.
const (
	EscalationsPath      = "/v1/admin/escalations"        // GET list (?team=, ?open=1)
	EscalationPath       = "/v1/admin/escalation"         // GET show (?id=)
	EscalationAnswerPath = "/v1/admin/escalations/answer" // POST

	// The architect credentials, managed with the operator credential only.
	ArchitectCredentialsPath      = "/v1/admin/architect-credentials"        // GET list, POST issue
	ArchitectCredentialRevokePath = "/v1/admin/architect-credentials/revoke" // POST

	// CodeEscalationAnswered refuses a second answer.
	CodeEscalationAnswered = "escalation_answered"
	// CodeForbidden refuses an architect credential on a route that is the
	// operator's own.
	CodeForbidden = "forbidden"
)

// EscalationTask is the aimem task an escalation is about.
type EscalationTask struct {
	HubID     string `json:"hub_id"`
	ProjectID string `json:"project_id"`
	TaskID    string `json:"task_id"`
}

// EscalationOption is one answer the coordinator offers, with its
// consequence.
type EscalationOption struct {
	Option      string `json:"option"`
	Consequence string `json:"consequence"`
}

// Escalation is a request and, once given, its answer.
type Escalation struct {
	ID                 string             `json:"id"`
	TeamID             string             `json:"team_id"`
	CoordinatorAgentID string             `json:"coordinator_agent_id"`
	Task               EscalationTask     `json:"task"`
	AttemptID          string             `json:"attempt_id,omitempty"`
	Category           string             `json:"category"`
	Question           string             `json:"question"`
	Context            string             `json:"context,omitempty"`
	Options            []EscalationOption `json:"options"`
	Recommendation     string             `json:"recommendation"`
	Blocked            string             `json:"blocked,omitempty"`
	Urgency            string             `json:"urgency"`
	CreatedAt          time.Time          `json:"created_at"`
	Answer             *EscalationAnswer  `json:"answer,omitempty"`
}

// EscalationAnswer is the decision, who gave it ("operator:..." or
// "architect:<credential ID>") and when.
type EscalationAnswer struct {
	Decision   string    `json:"decision"`
	Rationale  string    `json:"rationale"`
	AnsweredBy string    `json:"answered_by"`
	AnsweredAt time.Time `json:"answered_at"`
}

// AnswerRequest answers one escalation.
type AnswerRequest struct {
	ID        string `json:"id"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale"`
}

// ArchitectCredentialRequest issues an architect credential.
type ArchitectCredentialRequest struct {
	Label string `json:"label"`
}

// ArchitectCredential is an architect credential's metadata. Bearer is set
// only in an issue answer, once.
type ArchitectCredential struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
	Bearer    string    `json:"bearer,omitempty"`
}
