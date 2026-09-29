package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// The test binary also plays three processes for the launcher's tests, when
// AICREW_PROBE is set:
//   - probe-client DIR: an agent client. It records its environment and
//     arguments, starts a grandchild the way a client starts its MCP server,
//     then exits with AICREW_PROBE_EXIT or runs until SIGTERM or DIR/stop,
//     writing a heartbeat; with AICREW_PROBE_STEP it first asks the
//     launcher's step channel for its pending steps (DIR/step.json);
//   - probe-grandchild DIR: records whether it sees AIMEM_TEAM_SESSION;
//   - probe-launcher HOME DIR: runs RunClient with the probe client, from the
//     agent home's configuration, so a test can kill the launcher outright.

type probeReport struct {
	PID        int      `json:"pid"`
	HasVar     bool     `json:"has_var"`
	Value      string   `json:"value"`
	Env        []string `json:"env"`
	Args       []string `json:"args"`
	Grandchild string   `json:"grandchild"`
	Dir        string   `json:"dir"`
}

func probeMain(args []string) (int, bool) {
	if os.Getenv("AICREW_PROBE") == "" || len(args) < 2 {
		return 0, false
	}
	switch args[0] {
	case "probe-client":
		return probeClient(args[1]), true
	case "probe-grandchild":
		v, ok := os.LookupEnv(SessionEnv)
		out := "absent"
		if ok {
			out = "present:" + v
		}
		os.WriteFile(filepath.Join(args[1], "grandchild.txt"), []byte(out), 0o600)
		return 0, true
	case "probe-launcher":
		if len(args) < 3 {
			return 2, true
		}
		return probeLauncher(args[1], args[2]), true
	}
	return 0, false
}

func probeClient(dir string) int {
	self, _ := os.Executable()
	if err := exec.Command(self, "probe-grandchild", dir).Run(); err != nil {
		return 9
	}
	if os.Getenv("AICREW_PROBE_STEP") != "" {
		// Ask the launcher for a step, as the model's client would.
		ans, err := CallStep(context.Background(), os.Getenv(HomeEnv), StepCall{Op: "pending"})
		if err != nil {
			ans = StepAnswer{Status: "error: " + err.Error()}
		}
		b, _ := json.Marshal(ans)
		os.WriteFile(filepath.Join(dir, "step.json"), b, 0o600)
	}
	gc, _ := os.ReadFile(filepath.Join(dir, "grandchild.txt"))
	v, ok := os.LookupEnv(SessionEnv)
	wd, _ := os.Getwd()
	b, _ := json.Marshal(probeReport{PID: os.Getpid(), HasVar: ok, Value: v, Env: os.Environ(), Args: os.Args,
		Grandchild: string(gc), Dir: wd})
	tmp := filepath.Join(dir, "report.tmp")
	os.WriteFile(tmp, b, 0o600)
	os.Rename(tmp, filepath.Join(dir, "report.json"))
	if code := os.Getenv("AICREW_PROBE_EXIT"); code != "" {
		n, _ := strconv.Atoi(code)
		return n
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	deadline := time.After(60 * time.Second)
	for beat := 0; ; beat++ {
		os.WriteFile(filepath.Join(dir, "heartbeat"), []byte(strconv.Itoa(beat)), 0o600)
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			return 0
		}
		select {
		case <-term:
			return 0
		case <-deadline:
			return 8
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func probeLauncher(home, dir string) int {
	cfg, err := LoadConfig(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	crew, err := NewCrew(cfg, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	e := NewEngine(cfg, crew, ExecAimem{Command: cfg.AimemCommand}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	self, _ := os.Executable()
	code, err := RunClient(context.Background(), e, Client{Path: self, Args: []string{"probe-client", dir}},
		Stdio{Out: io.Discard, Err: io.Discard}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return code
}
