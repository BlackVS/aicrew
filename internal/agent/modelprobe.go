package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// The turn model's probe (01a13613-709c). A home's turn model, agent.json's
// turns.model, resolves through the member's own Claude Code configuration
// and its provider or gateway, so only a call shows that it is served.
// check and serve make one minimal headless call before any turn: a
// one-line prompt and system prompt, no tool, no MCP server, one turn, no
// session kept, a small budget and its own ceiling. A model the provider
// does not serve costs nothing: Claude Code 2.1.310 answers it with its own
// model_not_found message and a 404 result before any model call.

const (
	// ModelProbeCeiling bounds the probe; a served model answers in about
	// two seconds.
	ModelProbeCeiling   = 60 * time.Second
	modelProbeBudgetUSD = 0.05
	modelProbePrompt    = "Reply with OK."
)

// The probe's answers.
const (
	ModelAvailable   = "available"
	ModelUnavailable = "unavailable"
	// ModelUnknown is a probe that showed neither: a refused login, a
	// limit, its ceiling or another failure, which a turn reports in its
	// own way.
	ModelUnknown = "unknown"
)

// ErrModelUnavailable refuses serve when the turn model is not served.
var ErrModelUnavailable = errors.New("the turn model is not available")

// TurnModelReport is the probe's answer: the configured model ("" is the
// client's default), the model the client resolved it to, and the state,
// with what was found when the state is not available.
type TurnModelReport struct {
	Configured string `json:"configured"`
	Resolved   string `json:"resolved,omitempty"`
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
}

// ModelProbeArgs are the probe's arguments: no tool, no MCP server and no
// session, so the call is the prompt and its answer.
func ModelProbeArgs(model string) []string {
	args := []string{"-p", modelProbePrompt, "--output-format", "stream-json", "--verbose",
		"--system-prompt", modelProbePrompt, "--tools", "", "--strict-mcp-config", "--max-turns", "1",
		"--no-session-persistence", "--max-budget-usd", fmt.Sprintf("%.2f", modelProbeBudgetUSD)}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// ProbeTurnModel makes the probe's call with client in home, under env,
// within ceiling.
func ProbeTurnModel(ctx context.Context, client, home string, env []string, model string, ceiling time.Duration) TurnModelReport {
	rep := TurnModelReport{Configured: model, State: ModelUnknown}
	ctx, cancel := context.WithTimeout(ctx, ceiling)
	defer cancel()
	cmd := exec.CommandContext(ctx, client, ModelProbeArgs(model)...)
	cmd.Dir, cmd.Env = home, env
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4 << 10}
	out, err := cmd.StdoutPipe()
	if err != nil {
		rep.Detail = err.Error()
		return rep
	}
	if err := cmd.Start(); err != nil {
		rep.Detail = "the client could not be run: " + err.Error()
		return rep
	}
	res := ReadTurnStream(out, cancel)
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	switch {
	case res.Outcome == TurnModelUnavailable:
		rep.State, rep.Detail = ModelUnavailable, modelUnavailable(model)
	case res.Outcome == TurnFinished || (res.Outcome == TurnLimitReached && res.LimitKind == "budget"):
		rep.State = ModelAvailable
		rep.Resolved = strings.Join(res.Models, ", ")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		rep.Detail = fmt.Sprintf("the probe did not finish within %s", ceiling)
	case res.Outcome != "":
		rep.Detail = res.Outcome + ": " + res.Error
	default:
		rep.Detail = fmt.Sprintf("the client exited without a result (%v): %s", werr, tail(stderr.String(), 300))
	}
	return rep
}

// modelUnavailable is the instruction for a turn model the provider does
// not serve.
func modelUnavailable(model string) string {
	what := fmt.Sprintf("the turn model %q", model)
	if model == "" {
		what = "the client's default model"
	}
	return what + " is not served by the client's provider or gateway: set turns.model in the home's agent.json " +
		"to a model it serves"
}
