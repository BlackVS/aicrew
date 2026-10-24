package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// modelProbeEnv makes the test binary the client and has it answer the probe
// with the stream lines.
func modelProbeEnv(t *testing.T, lines ...string) (env []string, logf string) {
	t.Helper()
	dir := t.TempDir()
	logf, streamf := filepath.Join(dir, "args.json"), filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(streamf, []byte(stream(lines...)), 0o600); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), fakeTurnEnv+"="+logf, fakeTurnStreamEnv+"="+streamf), logf
}

// The probe is one minimal call: no tool, no MCP server, no session, one
// turn, a small budget, the turn model. A served model is reported with
// the model the client resolved it to; one the provider does not serve
// names turns.model; anything else is unknown, and so is a probe past its
// ceiling.
func TestProbeTurnModel(t *testing.T) {
	self, _ := os.Executable()
	home := t.TempDir()
	ctx := context.Background()

	env, logf := modelProbeEnv(t, streamInit, `{"type":"assistant","message":{"model":"claude-haiku-5-5","usage":{"input_tokens":2}}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"OK","total_cost_usd":0.0015}`)
	rep := ProbeTurnModel(ctx, self, home, env, "haiku", time.Minute)
	if rep != (TurnModelReport{Configured: "haiku", Resolved: "claude-haiku-5-5", State: ModelAvailable}) {
		t.Fatalf("a served model: %+v", rep)
	}
	var got struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	}
	raw, _ := os.ReadFile(logf)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "Reply with OK.", "--output-format", "stream-json", "--verbose", "--system-prompt",
		"Reply with OK.", "--tools", "", "--strict-mcp-config", "--max-turns", "1", "--no-session-persistence",
		"--max-budget-usd", "0.05", "--model", "haiku"}
	if !slices.Equal(got.Args, want) || got.Stdin != "" {
		t.Errorf("the probe ran with %q, stdin %q", got.Args, got.Stdin)
	}
	if args := ModelProbeArgs(""); slices.Contains(args, "--model") {
		t.Errorf("the client's default model is passed: %q", args)
	}

	// A spent probe budget still shows the model answered.
	env, _ = modelProbeEnv(t, `{"type":"assistant","message":{"model":"claude-opus-5-5"}}`,
		`{"type":"result","subtype":"error_max_budget_usd","is_error":true}`)
	if rep := ProbeTurnModel(ctx, self, home, env, "", time.Minute); rep.State != ModelAvailable || rep.Resolved != "claude-opus-5-5" {
		t.Errorf("a spent probe budget: %+v", rep)
	}

	env, _ = modelProbeEnv(t, streamInit, streamNoModel, streamNoModelResult)
	rep = ProbeTurnModel(ctx, self, home, append(env, fakeTurnExitEnv+"=1"), "claude-3-haiku", time.Minute)
	if rep.State != ModelUnavailable || rep.Resolved != "" || !strings.Contains(rep.Detail, `"claude-3-haiku"`) ||
		!strings.Contains(rep.Detail, "turns.model") {
		t.Errorf("a model the provider does not serve: %+v", rep)
	}
	env, _ = modelProbeEnv(t, streamInit, streamNoModel, streamNoModelResult)
	if rep := ProbeTurnModel(ctx, self, home, env, "", time.Minute); !strings.Contains(rep.Detail, "the client's default model") {
		t.Errorf("the default model not served: %+v", rep)
	}

	env, _ = modelProbeEnv(t, `{"type":"result","subtype":"success","is_error":true,"api_error_status":401}`)
	if rep := ProbeTurnModel(ctx, self, home, env, "haiku", time.Minute); rep.State != ModelUnknown ||
		!strings.Contains(rep.Detail, TurnNeedsLogin) {
		t.Errorf("a refused login: %+v", rep)
	}
	env, _ = modelProbeEnv(t, streamInit)
	start := time.Now()
	rep = ProbeTurnModel(ctx, self, home, append(env, fakeTurnSleepEnv+"=30s"), "haiku", 300*time.Millisecond)
	if rep.State != ModelUnknown || !strings.Contains(rep.Detail, "did not finish within 300ms") || time.Since(start) > 20*time.Second {
		t.Errorf("a probe past its ceiling: %+v after %s", rep, time.Since(start))
	}
	if rep := ProbeTurnModel(ctx, filepath.Join(home, "no-such-client"), home, env, "haiku", time.Minute); rep.State != ModelUnknown {
		t.Errorf("a client that cannot run: %+v", rep)
	}
}

// serve refuses to start on a model the provider does not serve, with the
// instruction; a served model and a probe that shows neither let it start,
// logged.
func TestServeModel(t *testing.T) {
	self, _ := os.Executable()
	home := t.TempDir()
	ctx := context.Background()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	env, _ := modelProbeEnv(t, streamInit, streamNoModel, streamNoModelResult)
	err := serveModel(ctx, self, home, env, "claude-3-haiku", time.Minute, log)
	if !errors.Is(err, ErrModelUnavailable) || !strings.Contains(err.Error(), "turns.model") {
		t.Fatalf("serve on a model not served: %v", err)
	}
	env, _ = modelProbeEnv(t, `{"type":"assistant","message":{"model":"claude-haiku-5-5"}}`,
		`{"type":"result","subtype":"success","is_error":false}`)
	if err := serveModel(ctx, self, home, env, "haiku", time.Minute, log); err != nil ||
		!strings.Contains(logs.String(), "resolved=claude-haiku-5-5") {
		t.Errorf("serve on a served model: %v, %s", err, logs.String())
	}
	env, _ = modelProbeEnv(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"]}`)
	if err := serveModel(ctx, self, home, env, "haiku", time.Minute, log); err != nil ||
		!strings.Contains(logs.String(), "could not be verified") {
		t.Errorf("serve on a probe that shows neither: %v, %s", err, logs.String())
	}
}

// A turn whose model the provider does not serve holds the member as a
// refused login does: an hour, doubling, not a failure's minute.
func TestModelUnavailableHolds(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	l := &TurnLoop{Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))}
	var st turnState
	l.afterOutcome(&st, TurnResult{Outcome: TurnModelUnavailable, Error: modelUnavailable("haiku")})
	if st.Backoff != time.Hour || st.Hold != TurnModelUnavailable || !st.HoldUntil.Equal(now.Add(time.Hour)) {
		t.Errorf("the hold: %+v", st)
	}
	if outcomeHold(st, TurnResult{Outcome: TurnModelUnavailable}, "att-1") != nil {
		t.Error("a model not served reported a hold to aicrewd")
	}
}
