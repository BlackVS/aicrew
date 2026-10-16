// Package events is the data half of aicrew's event model
// (docs/DESIGN-TURNS.md, sections 12 and 13): the subscriptions that say
// which members an event wakes, and through which handler. The bus itself,
// the journal and delivery, is the store's (internal/store/events.go).
//
// The subscriptions are data, not code: subscriptions.json, embedded in the
// release and checked against subscriptions.schema.json. A subscription
// names the event kinds it matches, its recipients by role, each with the
// conditions under which the role is woken, and its handler. The kinds,
// roles, conditions and handlers a file may name are the ones this release
// implements.
package events

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// The event kinds.
const (
	// KindBoardChanged is a task's change on a hub's board feed.
	KindBoardChanged = "board.changed"
	// KindForgeChanged is a change of an attempt's pull request that its
	// worker's launcher observed.
	KindForgeChanged = "forge.changed"
)

// The roles a subscription wakes.
const (
	RoleWorker      = "worker"
	RoleCoordinator = "coordinator"
)

// The conditions under which a recipient is woken. Every condition a
// recipient names must hold.
const (
	// WhenAlways always holds.
	WhenAlways = "always"
	// WhenNotOwnChange holds for a board change that none of aicrew's own
	// committed steps made.
	WhenNotOwnChange = "not-own-change"
	// WhenReadyOrTeamAttempt holds for a board change to READY, or of a
	// task the team has an attempt for.
	WhenReadyOrTeamAttempt = "ready-or-team-attempt"
	// WhenSubmitOrSubmitted holds for a pull request change that bears on a
	// submit (a merge, a close or a reopen), or any change while the
	// attempt's result is submitted.
	WhenSubmitOrSubmitted = "submit-or-submitted"
)

// HandlerWake writes one lifecycle message per handler run, carrying the
// run's events, to the recipients they name.
const HandlerWake = "wake"

var (
	kinds      = []string{KindBoardChanged, KindForgeChanged}
	roles      = []string{RoleWorker, RoleCoordinator}
	conditions = []string{WhenAlways, WhenNotOwnChange, WhenReadyOrTeamAttempt, WhenSubmitOrSubmitted}
	handlers   = []string{HandlerWake}
	nameShape  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// FileVersion is the subscription file's format version.
const FileVersion = 1

// File is a subscription file.
type File struct {
	Schema        string         `json:"$schema,omitempty"`
	Version       int            `json:"version"`
	Subscriptions []Subscription `json:"subscriptions"`
}

// Subscription says which recipients an event of its kinds wakes, and
// through which handler.
type Subscription struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Kinds       []string    `json:"kinds"`
	To          []Recipient `json:"to"`
	Handler     string      `json:"handler"`
}

// Recipient is a role a subscription wakes when each of its conditions
// holds.
type Recipient struct {
	Role string   `json:"role"`
	When []string `json:"when"`
}

// Set is a parsed subscription file and its version: the digest of the
// file's bytes, which every handler run records.
type Set struct {
	File
	Version string
}

//go:embed subscriptions.json
var defaultsJSON []byte

// SchemaJSON is the subscription file's JSON Schema.
//
//go:embed subscriptions.schema.json
var SchemaJSON []byte

// Defaults is the release's subscriptions.
func Defaults() Set {
	s, err := Parse(defaultsJSON)
	if err != nil {
		panic("the embedded subscriptions are invalid: " + err.Error())
	}
	return s
}

// Parse reads and checks a subscription file: its fields, and the names it
// uses against this release's kinds, roles, conditions and handlers.
func Parse(raw []byte) (Set, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return Set{}, fmt.Errorf("the subscription file is not valid: %w", err)
	}
	if dec.More() {
		return Set{}, errors.New("the subscription file holds more than one document")
	}
	if f.Version != FileVersion {
		return Set{}, fmt.Errorf("the subscription file's version is %d, this release reads %d", f.Version, FileVersion)
	}
	if len(f.Subscriptions) == 0 {
		return Set{}, errors.New("the subscription file names no subscription")
	}
	seen := map[string]bool{}
	for _, s := range f.Subscriptions {
		if !nameShape.MatchString(s.Name) || seen[s.Name] {
			return Set{}, fmt.Errorf("subscription %q: a name is lowercase letters, digits and '-', and unique", s.Name)
		}
		seen[s.Name] = true
		if err := s.check(); err != nil {
			return Set{}, fmt.Errorf("subscription %s: %w", s.Name, err)
		}
	}
	sum := sha256.Sum256(raw)
	return Set{File: f, Version: hex.EncodeToString(sum[:8])}, nil
}

func (s Subscription) check() error {
	if len(s.Kinds) == 0 || len(s.To) == 0 {
		return errors.New("it names no kind or no recipient")
	}
	for _, k := range s.Kinds {
		if !slices.Contains(kinds, k) {
			return fmt.Errorf("unknown event kind %q", k)
		}
	}
	for _, r := range s.To {
		if !slices.Contains(roles, r.Role) {
			return fmt.Errorf("unknown role %q", r.Role)
		}
		if len(r.When) == 0 {
			return fmt.Errorf("the %s recipient names no condition", r.Role)
		}
		for _, w := range r.When {
			if !slices.Contains(conditions, w) {
				return fmt.Errorf("unknown condition %q", w)
			}
		}
	}
	if !slices.Contains(handlers, s.Handler) {
		return fmt.Errorf("unknown handler %q", s.Handler)
	}
	return nil
}

// Matching is the subscriptions that match kind.
func (s Set) Matching(kind string) []Subscription {
	var out []Subscription
	for _, sub := range s.Subscriptions {
		if slices.Contains(sub.Kinds, kind) {
			out = append(out, sub)
		}
	}
	return out
}

// Named is the subscription called name.
func (s Set) Named(name string) (Subscription, bool) {
	for _, sub := range s.Subscriptions {
		if sub.Name == name {
			return sub, true
		}
	}
	return Subscription{}, false
}

// Facts are what the conditions are evaluated on, as the event's source
// records them for one team.
type Facts struct {
	OwnChange   bool // a board change one of aicrew's committed steps made
	Ready       bool // a board change to READY
	TeamAttempt bool // the team has an attempt for the board change's task
	Submit      bool // a pull request change that bears on a submit
	Submitted   bool // the attempt's result is submitted
}

// Roles is the roles s wakes for an event with facts f, each once, in the
// subscription's order.
func (s Subscription) Roles(f Facts) []string {
	var out []string
	for _, r := range s.To {
		if slices.Contains(out, r.Role) {
			continue
		}
		all := true
		for _, w := range r.When {
			all = all && holds(w, f)
		}
		if all {
			out = append(out, r.Role)
		}
	}
	return out
}

func holds(when string, f Facts) bool {
	switch when {
	case WhenAlways:
		return true
	case WhenNotOwnChange:
		return !f.OwnChange
	case WhenReadyOrTeamAttempt:
		return f.Ready || f.TeamAttempt
	case WhenSubmitOrSubmitted:
		return f.Submit || f.Submitted
	}
	return false
}
