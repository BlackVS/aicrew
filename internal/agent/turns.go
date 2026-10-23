package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The turn loop's settings, state and records (docs/DESIGN-CONTROL-PLANE.md,
// sections 1.6 to 1.8 and 3.1; increment A3). Until the provider profile
// exists (A5), the limits come from agent.json's "turns" object, with the
// design's defaults. The state is nonsecret and the home's own: which
// conversation each turn resumes (D4), what each attempt's turns cost and
// whether they progressed, and when the member may next have a turn. Each
// turn is recorded as one line of logs/turns.jsonl, redacted, without a
// transcript.

// TurnLimits are a home's turn limits: agent.json's "turns" object.
type TurnLimits struct {
	// TurnTimeout ends a turn as stopped (default 30 minutes).
	TurnTimeout time.Duration
	// TurnBudgetUSD is passed to Claude Code as --max-budget-usd.
	TurnBudgetUSD float64
	// AttemptBudgetUSD and DailyBudgetUSD are checked before a turn: a
	// spent budget gives no turn until the operator raises it or the day
	// turns.
	AttemptBudgetUSD float64
	DailyBudgetUSD   float64
	// MaxContinuations bounds the continuation turns of one attempt.
	MaxContinuations int
	// Model is passed to the client with --model; empty is the client's
	// default.
	Model string
}

// The design's defaults (3.1) and fixed rules (1.3, 1.7, 1.8).
const (
	defaultTurnTimeout      = 30 * time.Minute
	defaultTurnBudgetUSD    = 2
	defaultAttemptBudgetUSD = 20
	defaultDailyBudgetUSD   = 50
	defaultMaxContinuations = 20
	// turnGap is the least time between two turn starts, so a burst of
	// messages coalesces into one turn.
	turnGap = 5 * time.Second
	// stallTurns is how many consecutive turns of an attempt with no step
	// and no new commit stall it.
	stallTurns = 3
	// handoffStallTurns is how many consecutive turns of an attempt with no
	// step that leave the handoff's CURRENT ATTEMPT section unchanged stall
	// it (handoff.go).
	handoffStallTurns = 2
)

// ReadTurnLimits reads agent.json's "turns" object, with the defaults for
// what it does not name or names out of range.
func ReadTurnLimits(home string) TurnLimits {
	l := TurnLimits{TurnTimeout: defaultTurnTimeout, TurnBudgetUSD: defaultTurnBudgetUSD,
		AttemptBudgetUSD: defaultAttemptBudgetUSD, DailyBudgetUSD: defaultDailyBudgetUSD,
		MaxContinuations: defaultMaxContinuations}
	doc, ok, err := readAgentDoc(home)
	if err != nil || !ok {
		return l
	}
	var t struct {
		TurnTimeout      string   `json:"turn_timeout"`
		TurnBudgetUSD    *float64 `json:"turn_budget_usd"`
		AttemptBudgetUSD *float64 `json:"attempt_budget_usd"`
		DailyBudgetUSD   *float64 `json:"daily_budget_usd"`
		MaxContinuations *int     `json:"max_continuations"`
		Model            string   `json:"model"`
	}
	raw, ok := doc.top["turns"]
	if !ok || json.Unmarshal(raw, &t) != nil {
		return l
	}
	if d, err := time.ParseDuration(t.TurnTimeout); err == nil && d >= time.Minute && d <= 6*time.Hour {
		l.TurnTimeout = d
	}
	for _, f := range []struct {
		in  *float64
		out *float64
	}{{t.TurnBudgetUSD, &l.TurnBudgetUSD}, {t.AttemptBudgetUSD, &l.AttemptBudgetUSD}, {t.DailyBudgetUSD, &l.DailyBudgetUSD}} {
		if f.in != nil && *f.in > 0 {
			*f.out = *f.in
		}
	}
	if t.MaxContinuations != nil && *t.MaxContinuations >= 0 {
		l.MaxContinuations = *t.MaxContinuations
	}
	if modelShape.MatchString(t.Model) {
		l.Model = t.Model
	}
	return l
}

// modelShape is a model name or alias the client takes.
var modelShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,99}$`)

// TurnStateFile is the loop's state, relative to the home.
const TurnStateFile = "state/turns.json"

// attemptTurns is what the loop keeps of one attempt's turns. A state
// written before every turn started fresh (increment 3b) also names the
// attempt's conversation, which is read over and left out.
type attemptTurns struct {
	Continuations int `json:"continuations"`
	// NoProgress counts the attempt's consecutive turns with no step and no
	// new commit on its branch; Head is the branch's last commit seen.
	NoProgress int     `json:"no_progress"`
	Head       string  `json:"head,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	// Stalled stops its continuation turns until a message arrives.
	Stalled bool `json:"stalled,omitempty"`
	// Unchanged counts the attempt's consecutive turns with no step that
	// left the handoff's CURRENT ATTEMPT section unchanged; Pushed is the
	// fingerprint of the doc last pushed to aicrewd (handoff.go).
	Unchanged int    `json:"handoff_unchanged,omitempty"`
	Pushed    string `json:"handoff_pushed,omitempty"`
}

// turnState is TurnStateFile.
type turnState struct {
	Attempts map[string]*attemptTurns `json:"attempts"`
	// Days is the cost per UTC day, the last few days. It and the
	// attempts' costs are a cache of the totals aicrewd keeps
	// (turncosts.go); Unreported is the turns' costs not yet reported to it.
	Days       map[string]float64 `json:"days"`
	Unreported []turnReport       `json:"unreported,omitempty"`
	// Running is the turn in progress, so a restart records it as
	// interrupted.
	Running *TurnRecord `json:"running,omitempty"`
	// HoldUntil is when the member may next have a turn, after a limit, a
	// login failure or failed turns; Hold says why.
	HoldUntil time.Time     `json:"hold_until,omitzero"`
	Hold      string        `json:"hold,omitempty"`
	Backoff   time.Duration `json:"backoff,omitempty"`
	// HoldReport is the hold aicrewd is to have, nil for none, and
	// HoldPending says it does not have it yet (holds.go).
	HoldReport  *holdReport `json:"hold_report,omitempty"`
	HoldPending bool        `json:"hold_pending,omitempty"`
	// Continue names the attempt whose last turn ended in work, without a
	// block or a submit: it is due a continuation turn.
	Continue string `json:"continue,omitempty"`
	// Handoff is the attempt whose CURRENT ATTEMPT section the launcher
	// last kept in docs/HANDOFF.md (handoff.go).
	Handoff string `json:"handoff_attempt,omitempty"`
}

func turnStatePath(home string) string { return filepath.Join(home, filepath.FromSlash(TurnStateFile)) }

func readTurnState(home string) turnState {
	st := turnState{}
	if raw, err := os.ReadFile(turnStatePath(home)); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Attempts == nil {
		st.Attempts = map[string]*attemptTurns{}
	}
	if st.Days == nil {
		st.Days = map[string]float64{}
	}
	return st
}

func writeTurnState(home string, st turnState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(filepath.Dir(turnStatePath(home)), 0o700); err != nil {
		return err
	}
	return writeAtomic(turnStatePath(home), append(b, '\n'))
}

// attempt is the state of attempt id, created on first use.
func (st *turnState) attempt(id string) *attemptTurns {
	a := st.Attempts[id]
	if a == nil {
		a = &attemptTurns{}
		st.Attempts[id] = a
	}
	return a
}

// spend records a turn's cost against its day and its attempt, keeping the
// last week of days.
func (st *turnState) spend(day, attemptID string, usd float64) {
	st.Days[day] += usd
	if attemptID != "" {
		st.attempt(attemptID).CostUSD += usd
	}
	for d := range st.Days {
		if d < dayOf(mustParseDay(day).AddDate(0, 0, -7)) {
			delete(st.Days, d)
		}
	}
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

func mustParseDay(day string) time.Time {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return time.Time{}
	}
	return t
}

// The turn outcomes (1.6).
const (
	TurnFinished     = "finished"
	TurnFailed       = "failed"
	TurnNeedsLogin   = "needs_login"
	TurnLimitReached = "limit_reached"
	TurnStopped      = "stopped"
	TurnInterrupted  = "interrupted"
)

// TurnUsage is a turn's own token use. CacheWrite is split by the cache's
// lifetime when the client reports it, since the two are priced apart.
type TurnUsage struct {
	Input        int64 `json:"input_tokens"`
	Output       int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read_tokens"`
	CacheWrite   int64 `json:"cache_write_tokens"`
	CacheWrite5m int64 `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1h int64 `json:"cache_write_1h_tokens,omitempty"`
}

// TurnRecord is one turn, as logs/turns.jsonl records it. It holds no
// secret and no transcript.
type TurnRecord struct {
	ID        string   `json:"id"`
	AgentID   string   `json:"agent_id"`
	TeamID    string   `json:"team_id"`
	AttemptID string   `json:"attempt_id,omitempty"`
	Task      *TaskRef `json:"task,omitempty"`
	Trigger   string   `json:"trigger"`
	// Role and Client are the member's worker type.
	Role   string `json:"role,omitempty"`
	Client string `json:"client"`
	// Model is the model the turn used, or the configured one when the
	// client did not say; several are joined with commas.
	Model string `json:"model,omitempty"`
	// Conversation is the turn's own, fresh conversation (docs/DESIGN-TURNS.md,
	// T1). Resumed is false since increment 3b; records of earlier releases
	// carry true for a turn that resumed the previous one's conversation.
	Conversation string    `json:"conversation"`
	Resumed      bool      `json:"resumed"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitzero"`
	// DurationMS is the turn's wall time.
	DurationMS int64  `json:"duration_ms,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	Error      string `json:"error,omitempty"`
	// LimitKind is "budget" for the profile's own budget, "provider" for
	// the provider's limit; ResetAt is when the provider lifts it.
	LimitKind string    `json:"limit_kind,omitempty"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	Usage     TurnUsage `json:"usage"`
	// CostUSD is the turn's own cost, derived as CostSource says
	// (pricing.go); PriceList is the price list's version when it priced
	// it. Records written before these fields existed carry the client's
	// running total of the conversation in cost_usd instead.
	CostUSD    float64 `json:"cost_usd"`
	CostSource string  `json:"cost_source,omitempty"`
	PriceList  string  `json:"price_list,omitempty"`
	// CostBasis is "list" for a subscription's notional list price.
	CostBasis string `json:"cost_basis,omitempty"`
	// ClientTotalUSD is the client's own total for the turn's conversation,
	// kept as reported.
	ClientTotalUSD float64  `json:"client_total_usd,omitempty"`
	Denials        []string `json:"permission_denials"`
	Steps          int      `json:"steps"`
	Summary        string   `json:"summary,omitempty"`
	// BriefMessages are the ids of the messages the brief carried, which
	// aicrewd counted delivered when the brief read them; BriefUnacted says
	// the turn did not finish, so it carried them without acting.
	BriefMessages []string `json:"brief_messages,omitempty"`
	BriefUnacted  bool     `json:"brief_messages_unacted,omitempty"`
}

// end sets the turn's outcome, and whether it carried the brief's messages
// without acting on them.
func (r *TurnRecord) end(outcome string) {
	r.Outcome = outcome
	r.BriefUnacted = len(r.BriefMessages) > 0 && outcome != TurnFinished
}

// TurnLogFile is the turn records, relative to the home.
const TurnLogFile = "logs/turns.jsonl"

// appendTurnRecord appends r, redacted, to the home's turn log.
func appendTurnRecord(home string, r TurnRecord) error {
	r.Summary = redactTurnText(home, r.Summary)
	r.Error = redactTurnText(home, r.Error)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := filepath.Join(home, filepath.FromSlash(TurnLogFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// tokenShapes are the known token shapes a turn's text is redacted of
// (4.3): aicrew's operator, architect, session, proof, handle and
// introspection credentials, aimem's tokens and receipts, forge tokens and
// provider keys. Redaction is best-effort.
var tokenShapes = regexp.MustCompile(`(aop_|aar_|ast1_|acp1_|acs1_|amr1_|aicrew_introspect_|aimem_[a-z]+_|ghp_|gho_|ghs_|ghu_|github_pat_|glpat-|sk-ant-|sk-)[A-Za-z0-9_.\-]{8,}`)

// maxTurnSummary bounds the redacted summary a record keeps.
const maxTurnSummary = 2 << 10

// redactTurnText is s without the home's forge tokens and the known token
// shapes, and at most maxTurnSummary bytes (its tail).
func redactTurnText(home, s string) string {
	s = tokenShapes.ReplaceAllString(RedactForgeTokens(home, s), "[redacted]")
	if len(s) > maxTurnSummary {
		s = strings.ToValidUTF8("…"+s[len(s)-maxTurnSummary:], "")
	}
	return s
}
