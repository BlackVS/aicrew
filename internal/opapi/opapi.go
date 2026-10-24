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
	TeamToolchainPath    = "/v1/admin/team/toolchain"         // POST: select a project's toolchain
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
	// Toolchains are the toolchains selected for the team's projects, by
	// project ID (pilot finding #18).
	Toolchains map[string]string `json:"toolchains,omitempty"`
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

// TeamToolchainRequest selects the toolchain a worker's turn runs for one
// of the team's projects: a name from internal/toolchain, or "" to clear it.
type TeamToolchainRequest struct {
	TeamID    string `json:"team_id"`
	ProjectID string `json:"project_id"`
	Toolchain string `json:"toolchain"`
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

// The operator's control of a team (docs/DESIGN-CONTROL-PLANE.md, section
// 4.2 and decision D12): the team's state, pause and resume of the team or
// one member, and a nudge, the operator's note to one member. The operator
// credential only.
const (
	TeamStatePath    = "/v1/admin/team/state"    // GET (?id=)
	TeamPausePath    = "/v1/admin/team/pause"    // POST
	TeamResumePath   = "/v1/admin/team/resume"   // POST
	MemberPausePath  = "/v1/admin/member/pause"  // POST
	MemberResumePath = "/v1/admin/member/resume" // POST
	MemberNudgePath  = "/v1/admin/member/nudge"  // POST
)

// TeamStatesPath is every team's state, or one team's with ?team=ID: the
// team state the architect seat reads (/arch-status). The operator
// credential and an architect credential may read it; an architect
// credential reaches no pause, resume or nudge.
const TeamStatesPath = "/v1/admin/team-states" // GET (?team=ID)

// Member states (docs/DESIGN-CONTROL-PLANE.md, section 1.6). needs_login and
// limit_reached arrive with the provider profile (A5).
const (
	StateWorking      = "working"
	StateWaiting      = "waiting"
	StateNeedsLogin   = "needs_login"
	StateLimitReached = "limit_reached"
	StatePaused       = "paused"
	StateOffline      = "offline"
)

// PauseRequest pauses or resumes a team, or one of its members when AgentID
// is set (the member routes). Reason is kept with a pause; a resume takes
// none.
type PauseRequest struct {
	TeamID  string `json:"team_id"`
	AgentID string `json:"agent_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Pause is a team's pause, or a member's when AgentID is set.
type Pause struct {
	TeamID   string    `json:"team_id"`
	AgentID  string    `json:"agent_id,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	PausedBy string    `json:"paused_by"`
	PausedAt time.Time `json:"paused_at"`
}

// PauseResult is the pause now in force for the target, or none, and
// whether the call changed it: pausing what is paused, or resuming what is
// not, changes nothing.
type PauseResult struct {
	TeamID  string `json:"team_id"`
	AgentID string `json:"agent_id,omitempty"`
	Paused  *Pause `json:"paused,omitempty"`
	Changed bool   `json:"changed"`
}

// NudgeRequest sends the operator's note to one member.
type NudgeRequest struct {
	TeamID  string `json:"team_id"`
	AgentID string `json:"agent_id"`
	Note    string `json:"note"`
}

// Nudge is the message a nudge wrote to the member's inbox.
type Nudge struct {
	MessageID string    `json:"message_id"`
	Seq       int64     `json:"seq"`
	TeamID    string    `json:"team_id"`
	AgentID   string    `json:"agent_id"`
	CreatedAt time.Time `json:"created_at"`
}

// TeamState is a team's pause and each current member's state, as aicrewd
// sees them at ReadAt.
type TeamState struct {
	TeamID   string        `json:"team_id"`
	TeamName string        `json:"team_name"`
	Paused   *Pause        `json:"paused,omitempty"`
	Members  []MemberState `json:"members"`
	ReadAt   time.Time     `json:"read_at"`
}

// MemberState is one member in the team state. State is one of the member
// states; Paused is the member's own pause (the team's is on TeamState).
// The current turn arrives with the turn reports (A4), and the runner and
// host with the scheduler (B2).
type MemberState struct {
	AgentID        string         `json:"agent_id"`
	Label          string         `json:"label"`
	Role           string         `json:"role"`
	State          string         `json:"state"`
	Profile        MemberProfile  `json:"profile"`
	Session        *MemberSession `json:"session,omitempty"`
	Paused         *Pause         `json:"paused,omitempty"`
	OpenAttempt    *MemberAttempt `json:"open_attempt,omitempty"`
	Unacknowledged int            `json:"unacknowledged"`
	// Stalled marks a member that looks stalled: working or waiting, with
	// an open attempt that has not changed for longer than the stall
	// window (store.StallWindow) and does not wait for the human merge or
	// an escalation's answer (store.LooksStalled).
	Stalled bool `json:"stalled,omitempty"`
	// Hold is the hold the member's launcher last reported: no turn
	// before NextCheck, because a budget is spent (with the amount spent
	// and the limit), the provider's limit is reached or the client's
	// login is missing (with the command that logs in).
	Hold *MemberHold `json:"hold,omitempty"`
}

// MemberHold is a member's hold in the team state.
type MemberHold struct {
	Kind      string    `json:"kind"`
	AttemptID string    `json:"attempt_id,omitempty"`
	SpentUSD  float64   `json:"spent_usd,omitempty"`
	LimitUSD  float64   `json:"limit_usd,omitempty"`
	NextCheck time.Time `json:"next_check"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	Command   string    `json:"command,omitempty"`
	Since     time.Time `json:"since"`
}

// MemberProfile is the client and model the member's agent last reported.
type MemberProfile struct {
	Model         string `json:"model,omitempty"`
	Client        string `json:"client,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
}

// MemberSession is the member's active session: when it started, and when
// aicrewd last heard from it and last saw its wake read the pending list
// (zero when not since aicrewd started).
type MemberSession struct {
	ID           string    `json:"id"`
	Generation   int64     `json:"generation"`
	StartedAt    time.Time `json:"started_at"`
	LastSeenAt   time.Time `json:"last_seen_at,omitzero"`
	LastWaitedAt time.Time `json:"last_waited_at,omitzero"`
}

// MemberAttempt is the newest attempt the member works on that is not
// closed, and when it last changed.
type MemberAttempt struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Phase     string    `json:"phase,omitempty"`
	HubID     string    `json:"hub_id"`
	ProjectID string    `json:"project_id"`
	TaskID    string    `json:"task_id"`
	UpdatedAt time.Time `json:"updated_at"`
	// HandoffUpdatedAt is when the worker's launcher last pushed the
	// attempt's handoff, which AttemptHandoffPath reads; absent when it has
	// none.
	HandoffUpdatedAt time.Time `json:"handoff_updated_at,omitzero"`
	// Block is the worker's blocked report: why, and since when. HubBlock
	// is the hub's block, when the team's project is no longer granted.
	Block    *AttemptBlock `json:"block,omitempty"`
	HubBlock *AttemptBlock `json:"hub_block,omitempty"`
}

// AttemptBlock is a block of an attempt: its reason and when it began.
type AttemptBlock struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// The attempt's handoff (docs/CREW-CONTRACT.md, "The attempt's handoff"):
// the worker's session doc as its launcher last pushed it to the attempt,
// in any state of the attempt. The operator credential and an architect
// credential may read it.
const AttemptHandoffPath = "/v1/admin/attempt/handoff" // GET (?id=ATTEMPT)

// AttemptHandoff is an attempt's handoff. The text is the worker's, as its
// launcher redacted and bounded it; Handoff is empty and UpdatedAt absent
// when the attempt has none.
type AttemptHandoff struct {
	AttemptID string    `json:"attempt_id"`
	TeamID    string    `json:"team_id"`
	Handoff   string    `json:"handoff"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// The pull requests waiting for the human merge (docs/DESIGN-CONTROL-PLANE.md,
// section 4.2; increment C3): the attempts whose latest result the
// coordinator accepted and whose delivery is not yet confirmed, read from
// aicrewd's records with no forge call. The operator credential and an
// architect credential may read it.
const MergesPath = "/v1/admin/merges" // GET (?team=ID)

// PendingMerge is one accepted result that waits for the human merge.
type PendingMerge struct {
	TeamID      string         `json:"team_id"`
	TeamName    string         `json:"team_name"`
	AttemptID   string         `json:"attempt_id"`
	Task        EscalationTask `json:"task"`
	WorkerID    string         `json:"worker_agent_id"`
	WorkerLabel string         `json:"worker_label"`
	ResultSeq   int64          `json:"result_seq"`
	ResultRef   string         `json:"result_ref"`
	SubmittedAt string         `json:"submitted_at"`
	Branch      string         `json:"branch"`
	// Pull is the pull request as the worker's launcher last observed it
	// (forge events); absent when it never reported one.
	Pull *PullSeen `json:"pull,omitempty"`
}

// PullSeen is a pull request as last observed.
type PullSeen struct {
	Number     int64    `json:"number"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	HeadCommit string   `json:"head_commit"`
	Labels     []string `json:"labels"`
	Checks     string   `json:"checks"`
	Comments   int64    `json:"comments"`
	ObservedAt string   `json:"observed_at"`
}
