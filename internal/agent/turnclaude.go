package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// One headless Claude Code turn (docs/DESIGN-CONTROL-PLANE.md, 1.4 and 1.6;
// docs/TURN-PROBE.md): `claude -p` with stream-json output, in the home,
// with standard input closed, `dontAsk` permissions and no prompts, the
// managed per-turn settings, and the conversation started with --session-id
// or resumed with --resume. The turn's events give its outcome, usage, cost
// and permission denials. A login failure ends the turn at the client's
// first authentication retry, and a time limit ends it as stopped.

// TurnSpec is one turn to run.
type TurnSpec struct {
	Client string // the client's executable
	Home   string
	Env    []string
	Prompt string
	// Settings is the per-turn settings file, relative to the home;
	// empty means TurnSettingsFile.
	Settings string
	// Conversation is the conversation's ID; Resume continues it.
	Conversation string
	Resume       bool
	Model        string
	BudgetUSD    float64
	Timeout      time.Duration
	// Transcript, when not nil, receives the turn's raw stream: the home's
	// own log of the turn, which never leaves the host (4.3).
	Transcript io.Writer
}

// TurnResult is what a turn ended with.
type TurnResult struct {
	Outcome   string
	Error     string
	LimitKind string
	ResetAt   time.Time
	// Usage is the turn's own token use.
	Usage TurnUsage
	// CostUSD is the turn's own cost, from CostSource. A Claude Code turn
	// the price list cannot price has CostSource CostClientTotal and no
	// CostUSD: the loop takes the difference of ClientTotalUSD since the
	// conversation's previous turn (pricing.go). Another client's adapter
	// reports its turn's own cost with an empty CostSource.
	CostUSD    float64
	CostSource string
	CostBasis  string
	// ClientTotalUSD is the client's own running total of the
	// conversation, as it reported it.
	ClientTotalUSD float64
	// Models are the models the turn used, sorted.
	Models  []string
	Denials []string
	Summary string
	// Context is the share of the client's context window the turn's last
	// model call used.
	Context float64
}

// ClaudeTurnArgs are the arguments a turn runs Claude Code with. No
// dangerous flag, permission bypass or fallback model is ever passed.
func ClaudeTurnArgs(spec TurnSpec) []string {
	settings := spec.Settings
	if settings == "" {
		settings = TurnSettingsFile
	}
	args := []string{"-p", spec.Prompt, "--output-format", "stream-json", "--verbose",
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--settings", settings}
	if spec.Resume {
		args = append(args, "--resume", spec.Conversation)
	} else {
		args = append(args, "--session-id", spec.Conversation)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", spec.BudgetUSD))
	}
	return args
}

// RunClaudeTurn runs one turn and reads its outcome. ctx ending stops it.
func RunClaudeTurn(ctx context.Context, spec TurnSpec) TurnResult {
	tctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.Command(spec.Client, ClaudeTurnArgs(spec)...)
	cmd.Dir = spec.Home
	cmd.Env = spec.Env
	cmd.Stdin = nil // closed: a client waits on an open one (TURN-PROBE)
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return TurnResult{Outcome: TurnFailed, Error: err.Error()}
	}
	bindToLauncher(cmd)
	if err := cmd.Start(); err != nil {
		return TurnResult{Outcome: TurnFailed, Error: "start the client: " + err.Error()}
	}
	release, _ := afterStart(cmd)
	defer release()
	stop := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		select {
		case <-tctx.Done():
			_ = cmd.Process.Kill()
		case <-stop:
			_ = cmd.Process.Kill()
		case <-ended:
		}
	}()
	var stream io.Reader = out
	if spec.Transcript != nil {
		stream = io.TeeReader(out, spec.Transcript)
	}
	res := ReadTurnStream(stream, func() { close(stop) })
	_, _ = io.Copy(io.Discard, stream)
	werr := cmd.Wait()
	close(ended)
	switch {
	case res.Outcome == TurnNeedsLogin:
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		res.Outcome, res.Error = TurnStopped, fmt.Sprintf("the turn's time limit (%s) passed", spec.Timeout)
	case ctx.Err() != nil:
		res.Outcome, res.Error = TurnStopped, "the turn loop is stopping"
	case res.Outcome == "" && werr != nil:
		res.Outcome, res.Error = TurnFailed, fmt.Sprintf("the client exited without a result: %v: %s", werr,
			strings.TrimSpace(stderr.String()))
	case res.Outcome == "":
		res.Outcome, res.Error = TurnFailed, "the client exited without a result"
	}
	return res
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// streamEvent is the part of a stream-json event a turn reads.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// system/api_retry
	Error string `json:"error"`
	// rate_limit_event
	RateLimitInfo *struct {
		Status   string `json:"status"`
		ResetsAt int64  `json:"resetsAt"`
	} `json:"rate_limit_info"`
	// assistant: the model call's usage
	Message *struct {
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	// result
	IsError        bool     `json:"is_error"`
	Result         string   `json:"result"`
	TotalCostUSD   float64  `json:"total_cost_usd"`
	APIErrorStatus int      `json:"api_error_status"`
	TerminalReason string   `json:"terminal_reason"`
	Errors         []string `json:"errors"`
	Usage          *struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheWrite    int64 `json:"cache_creation_input_tokens"`
		CacheCreation *struct {
			Write5m int64 `json:"ephemeral_5m_input_tokens"`
			Write1h int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		ContextWindow int64  `json:"contextWindow"`
		CostBasis     string `json:"costBasis"`
	} `json:"modelUsage"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

// maxStreamLine bounds one event line.
const maxStreamLine = 4 << 20

// ReadTurnStream reads a turn's stream-json events until the result event or
// the end of the stream. abort is called once when the turn must end at
// once: the client's first authentication retry, which it would otherwise
// repeat for minutes (TURN-PROBE).
func ReadTurnStream(r io.Reader, abort func()) TurnResult {
	var (
		res      TurnResult
		lastCall int64
		window   int64
		limited  bool
		aborted  bool
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxStreamLine)
	for sc.Scan() {
		var ev streamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "api_retry" && ev.Error == "authentication_failed":
			res.Outcome, res.Error = TurnNeedsLogin, "the provider refused the client's authentication"
			if !aborted {
				aborted = true
				abort()
			}
			return res
		case ev.Type == "rate_limit_event" && ev.RateLimitInfo != nil:
			if ev.RateLimitInfo.ResetsAt > 0 {
				res.ResetAt = time.Unix(ev.RateLimitInfo.ResetsAt, 0).UTC()
			}
			limited = ev.RateLimitInfo.Status != "" && ev.RateLimitInfo.Status != "allowed"
		case ev.Type == "assistant" && ev.Message != nil && ev.Message.Usage != nil:
			u := ev.Message.Usage
			lastCall = u.Input + u.CacheRead + u.CacheWrite
		case ev.Type == "result":
			readResult(&res, ev, limited)
			for model, m := range ev.ModelUsage {
				window = max(window, m.ContextWindow)
				if res.CostBasis == "" {
					res.CostBasis = m.CostBasis
				}
				res.Models = append(res.Models, model)
			}
			slices.Sort(res.Models)
			if window > 0 && lastCall > 0 {
				res.Context = float64(lastCall) / float64(window)
			}
			// modelUsage and total_cost_usd are the conversation's running
			// totals; the usage is the turn's own, so the turn is priced
			// from it when it used one model the price list knows.
			res.CostSource = CostClientTotal
			if len(res.Models) == 1 {
				if usd, ok := listCost(res.Models[0], res.Usage); ok {
					res.CostUSD, res.CostSource = usd, CostPriceList
				}
			}
			return res
		}
	}
	return res
}

// readResult reads the result event into res: a normal end is finished; a
// spent turn budget or a provider limit is limit_reached; a refused key is
// needs_login; any other error is failed.
func readResult(res *TurnResult, ev streamEvent, limited bool) {
	res.ClientTotalUSD = ev.TotalCostUSD
	if ev.Usage != nil {
		res.Usage = TurnUsage{Input: ev.Usage.Input, Output: ev.Usage.Output, CacheRead: ev.Usage.CacheRead,
			CacheWrite: ev.Usage.CacheWrite}
		if c := ev.Usage.CacheCreation; c != nil {
			res.Usage.CacheWrite5m, res.Usage.CacheWrite1h = c.Write5m, c.Write1h
		}
	}
	res.Denials = []string{}
	for _, d := range ev.PermissionDenials {
		res.Denials = append(res.Denials, d.ToolName)
	}
	res.Summary = ev.Result
	switch {
	case !ev.IsError && ev.Subtype == "success":
		res.Outcome = TurnFinished
	case ev.Subtype == "error_max_budget_usd" || ev.TerminalReason == "budget_exhausted":
		res.Outcome, res.LimitKind = TurnLimitReached, "budget"
		res.Error = "the turn's budget is spent"
	case ev.APIErrorStatus == 401 || ev.APIErrorStatus == 403:
		res.Outcome, res.Error = TurnNeedsLogin, "the provider refused the client's authentication"
	case ev.APIErrorStatus == 429 || limited:
		res.Outcome, res.LimitKind = TurnLimitReached, "provider"
		res.Error = "the provider's usage or rate limit is reached"
	default:
		res.Outcome = TurnFailed
		res.Error = strings.TrimSpace(strings.Join(append([]string{ev.Subtype, ev.TerminalReason}, ev.Errors...), " "))
	}
}
