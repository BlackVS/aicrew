package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/BlackVS/aicrew/internal/forge"
)

// The member's capabilities (docs/proposals/PILOT-1-FOLLOWUPS.md, 3.6): what
// the home verifies against its team's requirements, and reports to aicrewd
// as planning facts. A missing capability never blocks the home.

const (
	requirementsPath = "/v1/crew/requirements"
	capabilitiesPath = "/v1/crew/capabilities"
)

// Requirement is a project the team's hub grants, with the repository the
// hub binds to it: what a member's home has to verify.
type Requirement struct {
	HubID      string `json:"hub_id"`
	ProjectID  string `json:"project_id"`
	Repository struct {
		Kind   string `json:"kind"`
		URL    string `json:"url"`
		Access string `json:"access"`
	} `json:"repository"`
}

// Capability is one forge host's verified repositories, as aicrewd stores
// them.
type Capability struct {
	Host         string                 `json:"host"`
	Kind         string                 `json:"kind"`
	Account      string                 `json:"account"`
	Repositories []RepositoryCapability `json:"repositories"`
}

// RepositoryCapability is a repository with the access the forge reports.
type RepositoryCapability struct {
	URL    string `json:"url"`
	Access string `json:"access"`
}

// The states of a requirement in a capability check.
const (
	CapabilityVerified     = "verified"     // the forge reports the required access or more
	CapabilityInsufficient = "insufficient" // the forge reports less than the required access
	CapabilityMissing      = "missing"      // the home holds no credential for the host
	CapabilityRefused      = "refused"      // the forge rejected the token, or does not show the repository
	CapabilityUnreachable  = "unreachable"  // the forge did not answer
)

// CapabilityRow is one requirement in a check: never a blocker.
type CapabilityRow struct {
	Project    string `json:"project"`
	Repository string `json:"repository"`
	Host       string `json:"host"`
	Account    string `json:"account,omitempty"`
	Required   string `json:"required"`
	Access     string `json:"access,omitempty"`
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
	// Token is what the token itself may do for a write requirement, apart
	// from the account's access: verified, insufficient or not_checked.
	Token       string `json:"token,omitempty"`
	TokenDetail string `json:"token_detail,omitempty"`
}

// AccessAPI is what a capability check asks of a forge; forge.Client is
// the real one.
type AccessAPI interface {
	RepositoryAccess(ctx context.Context, host string, k forge.Kind, token, path string) (string, error)
	TokenRights(ctx context.Context, host string, k forge.Kind, token, path string) []forge.RightCheck
}

// The states of a token's own rights in a capability row.
const (
	TokenVerified     = "verified"     // the forge shows the token holds each right a write needs
	TokenInsufficient = "insufficient" // the forge names a right the token lacks
	TokenNotChecked   = "not_checked"  // the forge does not show it; the detail says why
)

// tokenRights sums the rights a write requirement needs into a row's
// token state and detail.
func tokenRights(checks []forge.RightCheck) (string, string) {
	var missing, unknown []string
	for _, c := range checks {
		switch c.State {
		case forge.RightMissing:
			missing = append(missing, c.Right)
		case forge.RightNotChecked:
			unknown = append(unknown, c.Right+": "+c.Detail)
		}
	}
	switch {
	case len(checks) == 0:
		return TokenNotChecked, "the token's own rights are not checked: the forge client read none"
	case len(missing) > 0:
		return TokenInsufficient, fmt.Sprintf("the token lacks %s, which a member's writes need "+
			"(docs/BRING-UP.md, \"The forge token's rights\")", strings.Join(missing, " and "))
	case len(unknown) > 0:
		return TokenNotChecked, "the token's own rights are not checked: " + strings.Join(unknown, "; ")
	}
	return TokenVerified, ""
}

// VerifyCapabilities checks every requirement with the home's own
// credential for its repository's host, as the forge itself reports the
// access, and returns a row per requirement and the capabilities to
// report: per host, each repository verified with the access the forge
// reports.
func VerifyCapabilities(ctx context.Context, home string, api AccessAPI, reqs []Requirement) ([]CapabilityRow, []Capability) {
	doc, _, err := readAgentDoc(home)
	byHost := map[string]*Capability{}
	// lowered are the repositories whose token the forge states lacks a
	// write right: every entry for them is reported as read, whatever
	// other requirement bound the same repository.
	lowered := map[string]bool{}
	rows := make([]CapabilityRow, 0, len(reqs))
	for _, r := range reqs {
		row := CapabilityRow{Project: r.ProjectID, Repository: r.Repository.URL, Required: r.Repository.Access}
		host, path, perr := forge.Repository(r.Repository.URL)
		if perr != nil {
			row.State, row.Detail = CapabilityRefused, "the hub's repository URL is not one a home can read"
			rows = append(rows, row)
			continue
		}
		row.Host = host
		if err != nil {
			row.State, row.Detail = CapabilityMissing, err.Error()
			rows = append(rows, row)
			continue
		}
		e, tok, terr := forgeTokenFor(home, doc, host)
		if terr != nil {
			row.State, row.Detail = CapabilityMissing, terr.Error()
			rows = append(rows, row)
			continue
		}
		row.Account = e.Account
		access, aerr := api.RepositoryAccess(ctx, host, forge.Kind(e.Kind), tok, path)
		var scopeErr *forge.ScopeError
		switch {
		case errors.As(aerr, &scopeErr):
			row.State, row.Detail = CapabilityInsufficient, fmt.Sprintf("reading the repository: %v "+
				"(docs/BRING-UP.md, \"The forge token's rights\")", aerr)
		case errors.Is(aerr, forge.ErrUnreachable):
			row.State, row.Detail = CapabilityUnreachable, aerr.Error()
		case errors.Is(aerr, forge.ErrRejected):
			row.State, row.Detail = CapabilityRefused, "the forge rejected the token (revoked or expired): ask the operator for a new one"
		case errors.Is(aerr, forge.ErrNotFound):
			row.State, row.Detail = CapabilityRefused, fmt.Sprintf("the account %s cannot see the repository", e.Account)
		case aerr != nil:
			row.State, row.Detail = CapabilityRefused, aerr.Error()
		case access == forge.AccessNone:
			row.State, row.Detail = CapabilityInsufficient, fmt.Sprintf("the account %s has no access to the repository", e.Account)
		default:
			row.Access = access
			row.State = CapabilityVerified
			if access == forge.AccessRead && r.Repository.Access == forge.AccessWrite {
				row.State, row.Detail = CapabilityInsufficient, fmt.Sprintf("the account %s can read the repository, not push to it", e.Account)
			}
			if access == forge.AccessWrite && r.Repository.Access == forge.AccessWrite {
				row.Token, row.TokenDetail = tokenRights(api.TokenRights(ctx, host, forge.Kind(e.Kind), tok, path))
				if row.Token == TokenInsufficient {
					// The account may push, the token may not: what the
					// member can do is read.
					row.State, row.Detail = CapabilityInsufficient, row.TokenDetail
					lowered[r.Repository.URL] = true
				}
			}
			c := byHost[host]
			if c == nil {
				c = &Capability{Host: host, Kind: e.Kind, Account: e.Account, Repositories: []RepositoryCapability{}}
				byHost[host] = c
			}
			c.Repositories = append(c.Repositories, RepositoryCapability{URL: r.Repository.URL, Access: access})
		}
		rows = append(rows, row)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	caps := make([]Capability, 0, len(hosts))
	for _, h := range hosts {
		c := byHost[h]
		for i, r := range c.Repositories {
			if lowered[r.URL] {
				c.Repositories[i].Access = forge.AccessRead
			}
		}
		caps = append(caps, *c)
	}
	return rows, caps
}

// SessionReader is the part of aicrewd's session API a capability report
// reads and writes through.
type SessionReader interface {
	Read(ctx context.Context, token, path string) (json.RawMessage, error)
	LocalStep(ctx context.Context, key, token, path string, body []byte) (json.RawMessage, error)
}

// ReportCapabilities reads the team's requirements, verifies them with the
// home's credentials and reports the capabilities to aicrewd, all as the
// session of token. It returns the rows of the check.
func ReportCapabilities(ctx context.Context, home string, crew SessionReader, api AccessAPI, token string) ([]CapabilityRow, error) {
	raw, err := crew.Read(ctx, token, requirementsPath)
	if err != nil {
		return nil, err
	}
	var req struct {
		Projects []Requirement `json:"projects"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("the requirements: %w", err)
	}
	rows, caps := VerifyCapabilities(ctx, home, api, req.Projects)
	body, err := json.Marshal(struct {
		Capabilities []Capability `json:"capabilities"`
	}{caps})
	if err != nil {
		return nil, err
	}
	if _, err := crew.LocalStep(ctx, newKey("capabilities"), token, capabilitiesPath, body); err != nil {
		return rows, err
	}
	return rows, nil
}
