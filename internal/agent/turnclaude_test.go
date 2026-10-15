package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeTurnEnv makes the test binary a fake Claude Code turn: it records its
// arguments, working directory and standard input in the file the variable
// names, prints the file named by fakeTurnStreamEnv as its stream, and then
// sleeps for fakeTurnSleepEnv and exits with fakeTurnExitEnv.
const (
	fakeTurnEnv       = "AICREW_FAKE_TURN_LOG"
	fakeTurnStreamEnv = "AICREW_FAKE_TURN_STREAM"
	fakeTurnSleepEnv  = "AICREW_FAKE_TURN_SLEEP"
	fakeTurnExitEnv   = "AICREW_FAKE_TURN_EXIT"
)

func fakeTurnMain() int {
	wd, _ := os.Getwd()
	in := make([]byte, 16)
	n, _ := os.Stdin.Read(in)
	rec, _ := json.Marshal(map[string]any{"args": os.Args[1:], "dir": wd, "stdin": n})
	_ = os.WriteFile(os.Getenv(fakeTurnEnv), rec, 0o600)
	if b, err := os.ReadFile(os.Getenv(fakeTurnStreamEnv)); err == nil {
		os.Stdout.Write(b)
	}
	if d, err := time.ParseDuration(os.Getenv(fakeTurnSleepEnv)); err == nil {
		time.Sleep(d)
	}
	if os.Getenv(fakeTurnExitEnv) == "1" {
		return 1
	}
	return 0
}

const (
	streamInit    = `{"type":"system","subtype":"init","session_id":"s","permissionMode":"dontAsk"}`
	streamCall    = `{"type":"assistant","message":{"model":"m","usage":{"input_tokens":10,"cache_read_input_tokens":690,"cache_creation_input_tokens":300}}}`
	streamAllowed = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1800000000,"rateLimitType":"five_hour"}}`
	streamSuccess = `{"type":"result","subtype":"success","is_error":false,"result":"Read the inbox and acknowledged one message.",` +
		`"total_cost_usd":0.25,"usage":{"input_tokens":12,"output_tokens":40,"cache_read_input_tokens":700,"cache_creation_input_tokens":300},` +
		`"modelUsage":{"m":{"contextWindow":10000,"costBasis":"list"}},"permission_denials":[{"tool_name":"Bash"},{"tool_name":"Write"}]}`
)

func stream(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// A turn's stream gives its outcome: a normal end, a spent budget, a
// refused key, a provider limit with its reset time, any other error; and
// the usage, cost, denials and the share of the context window its last
// model call used. The first authentication retry ends the turn at once.
func TestReadTurnStream(t *testing.T) {
	read := func(s string) (TurnResult, int) {
		aborts := 0
		return ReadTurnStream(strings.NewReader(s), func() { aborts++ }), aborts
	}
	res, _ := read(stream(streamInit, "not json", streamCall, streamAllowed, streamSuccess))
	if res.Outcome != TurnFinished || res.ClientTotalUSD != 0.25 || res.CostUSD != 0 || res.CostSource != CostClientTotal ||
		!slices.Equal(res.Models, []string{"m"}) || res.Usage.Output != 40 || res.Usage.CacheRead != 700 ||
		!slices.Equal(res.Denials, []string{"Bash", "Write"}) || res.Context != 0.1 || res.CostBasis != "list" ||
		res.Summary != "Read the inbox and acknowledged one message." {
		t.Fatalf("a finished turn: %+v", res)
	}
	res, _ = read(stream(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"terminal_reason":"budget_exhausted","total_cost_usd":2.1}`))
	if res.Outcome != TurnLimitReached || res.LimitKind != "budget" || res.ClientTotalUSD != 2.1 || res.CostSource != CostClientTotal {
		t.Errorf("a spent budget: %+v", res)
	}
	res, _ = read(stream(`{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Invalid API key"}`))
	if res.Outcome != TurnNeedsLogin {
		t.Errorf("a refused key: %+v", res)
	}
	res, aborts := read(stream(streamInit,
		`{"type":"system","subtype":"api_retry","error_status":401,"error":"authentication_failed","attempt":1,"max_retries":10}`,
		`{"type":"system","subtype":"api_retry","error_status":401,"error":"authentication_failed","attempt":2,"max_retries":10}`))
	if res.Outcome != TurnNeedsLogin || aborts != 1 {
		t.Errorf("an authentication retry: %+v, %d aborts", res, aborts)
	}
	res, _ = read(stream(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1900000000}}`,
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":429}`))
	if res.Outcome != TurnLimitReached || res.LimitKind != "provider" || !res.ResetAt.Equal(time.Unix(1900000000, 0)) {
		t.Errorf("a provider limit: %+v", res)
	}
	res, _ = read(stream(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"]}`))
	if res.Outcome != TurnFailed || !strings.Contains(res.Error, "boom") {
		t.Errorf("an error: %+v", res)
	}
	if res, _ = read(stream(streamInit)); res.Outcome != "" {
		t.Errorf("a stream without a result: %+v", res)
	}
}

// A turn runs Claude Code headless in the home: -p with the prompt,
// stream-json, dontAsk without prompts, the managed per-turn settings, the
// conversation started or resumed, the model and the turn budget; standard
// input closed; never a dangerous flag. Its time limit stops it, and a
// client that exits without a result fails.
func TestRunClaudeTurn(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	logf, streamf := filepath.Join(dir, "args.json"), filepath.Join(dir, "stream.jsonl")
	os.WriteFile(streamf, []byte(stream(streamInit, streamCall, streamSuccess)), 0o600)
	self, _ := os.Executable()
	env := append(os.Environ(), fakeTurnEnv+"="+logf, fakeTurnStreamEnv+"="+streamf)
	spec := TurnSpec{Client: self, Home: home, Env: env, Prompt: "/crew-turn 1 new message(s)",
		Conversation: "11111111-2222-4333-8444-555555555555", Model: "haiku", BudgetUSD: 2, Timeout: time.Minute}
	var transcript strings.Builder
	spec.Transcript = &transcript
	res := RunClaudeTurn(context.Background(), spec)
	spec.Transcript = nil
	if !strings.Contains(transcript.String(), `"subtype":"success"`) || !strings.Contains(transcript.String(), `"type":"assistant"`) {
		t.Errorf("the transcript holds %q", transcript.String())
	}
	var got struct {
		Args  []string `json:"args"`
		Dir   string   `json:"dir"`
		Stdin int      `json:"stdin"`
	}
	raw, _ := os.ReadFile(logf)
	if err := json.Unmarshal(raw, &got); err != nil || res.Outcome != TurnFinished {
		t.Fatalf("the turn: %+v %s", res, raw)
	}
	want := []string{"-p", "/crew-turn 1 new message(s)", "--output-format", "stream-json", "--verbose", "--permission-mode",
		"dontAsk", "--permission-prompts", "none", "--settings", TurnSettingsFile, "--session-id", spec.Conversation,
		"--model", "haiku", "--max-budget-usd", "2.00"}
	if !slices.Equal(got.Args, want) || got.Stdin != 0 {
		t.Fatalf("args %q, stdin %d bytes", got.Args, got.Stdin)
	}
	if a, _ := filepath.EvalSymlinks(got.Dir); a != mustEval(t, home) {
		t.Errorf("the turn ran in %s, not the home", got.Dir)
	}
	spec.Resume = true
	if args := ClaudeTurnArgs(spec); !slices.Contains(args, "--resume") || slices.Contains(args, "--session-id") {
		t.Errorf("a resume: %q", args)
	}
	for _, a := range ClaudeTurnArgs(spec) {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") || a == "--fallback-model" {
			t.Errorf("a dangerous argument: %s", a)
		}
	}
	spec.Env = append(env, fakeTurnSleepEnv+"=30s")
	spec.Timeout = 300 * time.Millisecond
	start := time.Now()
	if res := RunClaudeTurn(context.Background(), spec); res.Outcome != TurnStopped || time.Since(start) > 20*time.Second {
		t.Errorf("a turn past its limit: %+v after %s", res, time.Since(start))
	}
	os.WriteFile(streamf, []byte(stream(streamInit)), 0o600)
	spec.Env, spec.Timeout = append(env, fakeTurnExitEnv+"=1"), time.Minute
	if res := RunClaudeTurn(context.Background(), spec); res.Outcome != TurnFailed || !strings.Contains(res.Error, "without a result") {
		t.Errorf("a client that exits without a result: %+v", res)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A turn's own cost comes from its usage at the price list's prices when it
// used one priced model, whatever the running totals say. The figures are
// the last worker turn of the #154 cycle: its usage is its own, while
// total_cost_usd and modelUsage are the conversation's running totals, and
// the price list reproduces the totals' difference, $7.6582612 - $7.5250528.
func TestReadTurnStreamPricesTheTurn(t *testing.T) {
	result := `{"type":"result","subtype":"success","is_error":false,"result":"Nothing changed.","total_cost_usd":7.6582612,` +
		`"usage":{"input_tokens":6,"output_tokens":554,"cache_read_input_tokens":511322,"cache_creation_input_tokens":2480,` +
		`"cache_creation":{"ephemeral_1h_input_tokens":2480,"ephemeral_5m_input_tokens":0}},` +
		`"modelUsage":{"claude-opus-5-5":{"inputTokens":234,"outputTokens":41388,"cacheReadInputTokens":13447146,` +
		`"cacheCreationInputTokens":517517,"costUSD":7.6582612,"contextWindow":1000000,"costBasis":"list"}}}`
	call := `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":3}}}`
	res := ReadTurnStream(strings.NewReader(stream(call, call, result)), func() {})
	if res.CostSource != CostPriceList || math.Abs(res.CostUSD-(7.6582612-7.5250528)) > 1e-6 ||
		res.ClientTotalUSD != 7.6582612 || res.Usage.CacheWrite1h != 2480 || res.Usage.CacheWrite5m != 0 ||
		!slices.Equal(res.Models, []string{"claude-opus-5-5"}) {
		t.Fatalf("the turn's own cost: %+v", res)
	}

	// The models are the turn's own calls', not modelUsage's, which names
	// every model of the conversation so far: a resumed turn on one priced
	// model is priced even after an earlier turn used another.
	resumed := `{"type":"result","subtype":"success","total_cost_usd":9,"usage":{"input_tokens":1000},` +
		`"modelUsage":{"claude-opus-5-5":{},"claude-earlier":{}}}`
	res = ReadTurnStream(strings.NewReader(stream(call, resumed)), func() {})
	if !slices.Equal(res.Models, []string{"claude-opus-5-5"}) || res.CostSource != CostPriceList || res.CostUSD != 0.004 {
		t.Fatalf("a resumed turn on one model after another: %+v", res)
	}

	// Without the cache-write split, or with a model the list does not
	// price, or with two models in the turn's own calls, or with no call
	// naming a model, the list does not price the turn.
	other := `{"type":"assistant","message":{"model":"other"}}`
	for name, lines := range map[string][]string{
		"no split": {call, `{"type":"result","subtype":"success","total_cost_usd":1,` +
			`"usage":{"input_tokens":1,"cache_creation_input_tokens":5},"modelUsage":{"claude-opus-5-5":{}}}`},
		"unpriced model": {other, `{"type":"result","subtype":"success","total_cost_usd":1,"usage":{"input_tokens":1},"modelUsage":{"other":{}}}`},
		"two models": {call, other, `{"type":"result","subtype":"success","total_cost_usd":1,"usage":{"input_tokens":1},` +
			`"modelUsage":{"claude-opus-5-5":{},"other":{}}}`},
		"a 5-minute write": {call, `{"type":"result","subtype":"success","total_cost_usd":1,"usage":{"input_tokens":1,` +
			`"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":5}},"modelUsage":{"claude-opus-5-5":{}}}`},
		"no call": {`{"type":"result","subtype":"success","total_cost_usd":1,"usage":{"input_tokens":1},"modelUsage":{"claude-opus-5-5":{}}}`},
	} {
		res := ReadTurnStream(strings.NewReader(stream(lines...)), func() {})
		if res.CostSource != CostClientTotal || res.CostUSD != 0 || res.ClientTotalUSD != 1 {
			t.Errorf("%s: %+v", name, res)
		}
	}
}
