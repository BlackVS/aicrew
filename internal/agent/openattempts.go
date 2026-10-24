package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A coordinator's open attempts (pilot finding 2026-10-11). A coordinator
// restarted with an offer outstanding kept waiting on it after the offer had
// gone stale. At a coordinator's session start the launcher reads the team's
// open attempts from aicrewd and records them in OpenAttemptsFile, which the
// first instruction names; `aicrew-agent attempts` reads the current list.

const openAttemptsPath = "/v1/crew/attempts"

// attemptsReader reads aicrewd's session API with a session token.
type attemptsReader interface {
	Read(ctx context.Context, token, path string) (json.RawMessage, error)
}

// OpenAttemptsFile is the home file, relative to the home, that holds the
// open attempts as read at the coordinator's last session start.
const OpenAttemptsFile = "state/open-attempts.md"

// OpenAttempt is one open attempt as aicrewd lists it.
type OpenAttempt struct {
	ID                    string    `json:"id"`
	State                 string    `json:"state"`
	Phase                 string    `json:"phase,omitempty"`
	Task                  TaskRef   `json:"task"`
	WorkerAgentID         string    `json:"worker_agent_id"`
	WorkerLabel           string    `json:"worker_label"`
	CoordinatorGeneration int64     `json:"coordinator_generation,omitempty"`
	OfferExpiresAt        time.Time `json:"offer_expires_at,omitzero"`
	Stale                 bool      `json:"stale"`
	Expired               bool      `json:"expired"`
	Declined              bool      `json:"declined"`
	NextAction            string    `json:"next_action"`
	// HandoffUpdatedAt is when the worker last pushed the attempt's
	// handoff; zero when it has none, or when aicrewd is older than 280c.
	HandoffUpdatedAt time.Time `json:"handoff_updated_at,omitzero"`
}

// TaskRef names an aimem task.
type TaskRef struct {
	HubID     string `json:"hub_id"`
	ProjectID string `json:"project_id"`
	TaskID    string `json:"task_id"`
}

// OpenAttempts is aicrewd's answer: the team's open attempts and its
// current coordinator generation.
type OpenAttempts struct {
	CoordinatorGeneration int64         `json:"coordinator_generation"`
	Attempts              []OpenAttempt `json:"attempts"`
}

// ReadOpenAttempts reads the open attempts of the team of the coordinator's
// session token.
func ReadOpenAttempts(ctx context.Context, crew attemptsReader, token string) (OpenAttempts, error) {
	raw, err := crew.Read(ctx, token, openAttemptsPath)
	if err != nil {
		return OpenAttempts{}, err
	}
	var out OpenAttempts
	if err := json.Unmarshal(raw, &out); err != nil {
		return OpenAttempts{}, fmt.Errorf("the team's open attempts: %w", err)
	}
	return out, nil
}

// why says why an offer can no longer be accepted, or "" if it can.
func (a OpenAttempt) why() string {
	var r []string
	if a.Stale {
		r = append(r, "stale: made under an earlier coordinator session")
	}
	if a.Expired {
		r = append(r, "expired")
	}
	if a.Declined {
		r = append(r, "declined by its worker")
	}
	return strings.Join(r, "; ")
}

// Line is the attempt as one Markdown list item.
func (a OpenAttempt) Line() string {
	state := a.State
	if a.Phase != "" {
		state += " (" + a.Phase + ")"
	}
	if w := a.why(); w != "" {
		state += ", " + w
	}
	switch {
	case !a.HandoffUpdatedAt.IsZero():
		state += ", handoff pushed " + a.HandoffUpdatedAt.UTC().Format(time.RFC3339)
	case a.State == "running":
		state += ", no handoff pushed"
	}
	return fmt.Sprintf("- Attempt `%s`: task `%s/%s/%s`, worker %s, %s. Next: %s", a.ID, a.Task.HubID,
		a.Task.ProjectID, a.Task.TaskID, a.WorkerLabel, state, a.NextAction)
}

// RenderOpenAttempts is OpenAttemptsFile's text for a list read at at, or,
// when readErr is not nil, for a list that could not be read.
func RenderOpenAttempts(o OpenAttempts, at time.Time, readErr error) string {
	var b strings.Builder
	b.WriteString("# Open attempts at this session's start\n\n")
	fmt.Fprintf(&b, "The launcher wrote this file when the coordinator's session started (%s). ",
		at.UTC().Format(time.RFC3339))
	b.WriteString("Reconcile these attempts before anything else, as `docs/ROLES.md` says under \"Reconcile " +
		"first\". `aicrew-agent attempts` reads the current list.\n\n")
	switch {
	case readErr != nil:
		fmt.Fprintf(&b, "The list could not be read: %s. Run `aicrew-agent attempts`.\n", readErr)
	case len(o.Attempts) == 0:
		b.WriteString("The team has no open attempts.\n")
	default:
		for _, a := range o.Attempts {
			b.WriteString(a.Line() + "\n")
		}
	}
	return b.String()
}

// openAttemptsBudget bounds the read at a coordinator's start, which the
// client waits for.
const openAttemptsBudget = 20 * time.Second

// RecordOpenAttempts reads the team's open attempts as the session of token
// and writes them to home's OpenAttemptsFile. A failed read is written too,
// so the file never shows an earlier session's list as this one's.
func RecordOpenAttempts(ctx context.Context, home string, crew attemptsReader, token string, now time.Time) (OpenAttempts, error) {
	ctx, cancel := context.WithTimeout(ctx, openAttemptsBudget)
	defer cancel()
	o, readErr := ReadOpenAttempts(ctx, crew, token)
	p := filepath.Join(home, filepath.FromSlash(OpenAttemptsFile))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return o, err
	}
	if err := writeAtomic(p, []byte(RenderOpenAttempts(o, now, readErr))); err != nil {
		return o, err
	}
	return o, readErr
}
