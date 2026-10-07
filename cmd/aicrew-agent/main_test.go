package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

func noEngine(agent.Config, *slog.Logger) (*agent.Engine, error) {
	panic("no engine should be built")
}

// noEnv is an environment with no variable set.
func noEnv(string) string { return "" }

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"session"}, {"run"}, {"session", "start"}, {"session", "start", "-home"},
		{"session", "start", "-home", "x", "extra"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb, noEngine, noEnv); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestBadConfigAndStatus(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"session", "start", "-home", t.TempDir()}, &out, &errb, noEngine, noEnv); code != exitFailed {
		t.Fatalf("a home without agent.json: exit %d", code)
	}
	home := t.TempDir()
	cfg := `{"aicrew": {"url": "https://aicrew.example", "tls_trust_mode": "ca_dns", "tls_trust_value": "aicrew.example",
		"agent_id": "agent-1", "team_id": "team-1", "aimem_command": "aimem-not-installed-here"}}`
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(context.Background(), []string{"session", "status", "-home", home}, &out, &errb, noEngine, noEnv); code != exitOK ||
		!strings.Contains(out.String(), `"session": null`) {
		t.Fatalf("status without a session: exit %d, %s", code, out.String())
	}
	if code := run(context.Background(), []string{"session", "fly", "-home", home}, &out, &errb, noEngine, noEnv); code != exitUsage {
		t.Fatalf("an unknown verb: exit %d", code)
	}
}

func TestFinishCodes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	if finish(log, nil) != exitOK || finish(log, &agent.WorkOutstanding{NextAction: "x"}) != exitWorkKept ||
		finish(log, agent.ErrSessionEnded) != exitFailed {
		t.Fatal("exit codes")
	}
}

// launcherEnv is the environment a client started by `run` sees.
func launcherEnv(home string) func(string) string {
	return func(k string) string {
		switch k {
		case agent.SessionEnv:
			return filepath.Join(home, "aimem", "aicrew-sessions", "s.json")
		case agent.HomeEnv:
			return home
		}
		return ""
	}
}

// snapshot reads every file under dir, by relative path.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		files[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestRefusedInsideLauncher: session start, session leave and run, invoked
// from inside a client that run started, exit 2 with the next action before
// reading the home or building an engine, so the launcher's session is
// untouched (pilot P6). Status stays allowed, and an environment with only
// one of the two variables is not a launched client.
func TestRefusedInsideLauncher(t *testing.T) {
	home := t.TempDir()
	cfg := `{"aicrew": {"url": "https://aicrew.example", "tls_trust_mode": "ca_dns", "tls_trust_value": "aicrew.example",
		"agent_id": "agent-1", "team_id": "team-1", "aimem_command": "aimem-not-installed-here"}}`
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"agent.json": cfg, filepath.Join("state", "marker"): "the launcher's"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot(t, home)
	env := launcherEnv(home)
	check := func(name string, code int, stderr string) {
		t.Helper()
		if code != exitUsage || !strings.Contains(stderr, "refused inside a client started by aicrew-agent run") ||
			!strings.Contains(stderr, "next: use aicrew-agent inbox") {
			t.Errorf("%s: exit %d, %q", name, code, stderr)
		}
	}
	for _, verb := range []string{"start", "leave"} {
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"session", verb, "-home", home}, &out, &errb, noEngine, env)
		check("session "+verb, code, errb.String())
		if out.Len() != 0 {
			t.Errorf("session %s wrote %q", verb, out.String())
		}
	}
	var errb bytes.Buffer
	stdio := agent.Stdio{In: strings.NewReader(""), Out: new(bytes.Buffer), Err: &errb}
	check("run", runClient(context.Background(), []string{"-client", "claude", "-home", home}, stdio, nil, noEngine, env),
		errb.String())
	if after := snapshot(t, home); len(after) != len(before) {
		t.Fatalf("the home changed: %v", after)
	} else {
		for k, v := range before {
			if after[k] != v {
				t.Fatalf("%s changed", k)
			}
		}
	}

	var out bytes.Buffer
	errb.Reset()
	if code := run(context.Background(), []string{"session", "status", "-home", home}, &out, &errb, noEngine, env); code != exitOK {
		t.Errorf("status inside a launched client: exit %d, %s", code, errb.String())
	}
	for _, only := range []string{agent.SessionEnv, agent.HomeEnv} {
		one := func(k string) string {
			if k == only {
				return env(k)
			}
			return ""
		}
		errb.Reset()
		code := run(context.Background(), []string{"session", "start", "-home", t.TempDir()}, &out, &errb, noEngine, one)
		if code != exitFailed || strings.Contains(errb.String(), "refused inside") {
			t.Errorf("only %s set: exit %d, %s", only, code, errb.String())
		}
	}
}

// Inside a client run started, `session status` needs no --home: it prints
// the role and the team's projects the launcher recorded (3a4b).
func TestStatusInsideTheClientShowsRoleAndProjects(t *testing.T) {
	home := t.TempDir()
	cfg := `{"aicrew": {"url": "https://aicrew.example", "tls_trust_mode": "ca_dns", "tls_trust_value": "aicrew.example",
		"agent_id": "agent-1", "team_id": "team-1", "aimem_command": "aimem-not-installed-here", "aimem_hub": "main"}}`
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agent.SaveState(home, agent.State{AgentID: "agent-1", TeamID: "team-1", SessionID: "sess-1",
		Role: "coordinator"}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"session", "status"}, &out, &errb, noEngine, launcherEnv(home)); code != exitOK ||
		!strings.Contains(out.String(), `"role": "coordinator"`) || !strings.Contains(out.String(), `"team": null`) ||
		!strings.Contains(out.String(), "not recorded yet") {
		t.Fatalf("status before a team record: exit %d, %s %s", code, out.String(), errb.String())
	}
	var rec agent.TeamRecord
	if err := json.Unmarshal([]byte(`{"hub_alias": "main", "projects": [
		{"hub_id": "hub-1", "project_id": "aicrew", "repository": {"kind": "github", "url": "https://github.com/example/aicrew.git", "access": "write"}},
		{"hub_id": "hub-1", "project_id": "docs", "repository": null}]}`), &rec); err != nil {
		t.Fatal(err)
	}
	if err := agent.SaveTeam(home, rec); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(context.Background(), []string{"session", "status"}, &out, &errb, noEngine, launcherEnv(home)); code != exitOK {
		t.Fatalf("status: exit %d, %s", code, errb.String())
	}
	var view struct {
		Role string            `json:"role"`
		Team *agent.TeamRecord `json:"team"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil || view.Role != "coordinator" || view.Team == nil ||
		view.Team.HubAlias != "main" || len(view.Team.Projects) != 2 || view.Team.Projects[0].ProjectID != "aicrew" ||
		view.Team.Projects[0].Repository == nil || view.Team.Projects[0].Repository.Access != "write" ||
		view.Team.Projects[1].ProjectID != "docs" || view.Team.Projects[1].Repository != nil {
		t.Fatalf("status: %v %s", err, out.String())
	}
	// Outside a client, --home is still required.
	if code := run(context.Background(), []string{"session", "status"}, &out, &errb, noEngine, noEnv); code != exitUsage {
		t.Fatalf("status with no home: exit %d", code)
	}
}
