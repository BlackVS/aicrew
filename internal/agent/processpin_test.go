package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProcessPin plays `aimem process pin -p PROJECT --json`: it records its
// arguments, working directory and state root in pin-call.json, then prints
// pin.out, or pin.err on stderr with exit 1.
func fakeProcessPin(root string, args []string) int {
	wd, _ := os.Getwd()
	call, _ := json.Marshal(map[string]any{"args": args, "dir": wd, "state_dir": os.Getenv(StateDirEnv)})
	_ = os.WriteFile(filepath.Join(root, "pin-call.json"), call, 0o600)
	if msg, err := os.ReadFile(filepath.Join(root, "pin.err")); err == nil {
		fmt.Fprint(os.Stderr, string(msg))
		return 1
	}
	out, _ := os.ReadFile(filepath.Join(root, "pin.out"))
	fmt.Print(string(out))
	return 0
}

const (
	pinCommit = "0123456789abcdef0123456789abcdef01234567"
	pinDigest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

func pinJSON(project string) string {
	return `{"project":"` + project + `","repository":"https://forge.example/team/process.git","commit":"` + pinCommit +
		`","manifest":"process/manifest.json","set_digest":"` + strings.Repeat("cd", 32) + `","instruction_digest":"` + pinDigest + "\"}\n"
}

// fakePinAimem is an ExecAimem on the test binary as aimem, for a fresh
// home; the returned root holds the fake's answer files.
func fakePinAimem(t *testing.T) (ExecAimem, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("AICREW_FAKE_AIMEM_ROOT", root)
	return ExecAimem{Command: exe, Home: t.TempDir()}, root
}

// The pin is read with `aimem process pin -p PROJECT --json`, from the
// home's root with the home's aimem installation; aimem's own reason for
// having none, and an answer unfit for an offer, are errors.
func TestExecAimemProcessPin(t *testing.T) {
	a, root := fakePinAimem(t)
	write := func(name, body string) {
		t.Helper()
		os.Remove(filepath.Join(root, "pin.err"))
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pin.out", pinJSON("pilot"))
	pin, err := a.ProcessPin(context.Background(), "pilot")
	if err != nil || pin.Commit != pinCommit || pin.InstructionDigest != pinDigest || pin.Repository != "https://forge.example/team/process.git" {
		t.Fatalf("pin = %+v, %v", pin, err)
	}
	var call struct {
		Args     []string `json:"args"`
		Dir      string   `json:"dir"`
		StateDir string   `json:"state_dir"`
	}
	b, _ := os.ReadFile(filepath.Join(root, "pin-call.json"))
	if err := json.Unmarshal(b, &call); err != nil {
		t.Fatal(err)
	}
	same := func(x, y string) bool {
		fx, _ := os.Stat(x)
		fy, _ := os.Stat(y)
		return fx != nil && fy != nil && os.SameFile(fx, fy)
	}
	if strings.Join(call.Args, " ") != "pin -p pilot --json" || !same(call.Dir, a.Home) || call.StateDir != AimemDir(a.Home) {
		t.Errorf("aimem ran as %+v, want in the home %s with its installation", call, a.Home)
	}

	for name, c := range map[string]struct{ out, want string }{
		"another project": {pinJSON("other"), `not "pilot"`},
		"a short commit":  {strings.Replace(pinJSON("pilot"), pinCommit, "0123456", 1), "not a full commit"},
		"a bare digest":   {strings.Replace(pinJSON("pilot"), "sha256:", "", 1), "not sha256:"},
		"no manifest":     {strings.Replace(pinJSON("pilot"), "process/manifest.json", "", 1), "no repository or manifest"},
		"not a pin":       {"no selection\n", "printed no pin"},
	} {
		write("pin.out", c.out)
		if _, err := a.ProcessPin(context.Background(), "pilot"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	write("pin.err", "aimem: no process is selected for project pilot; an admin selects one on the hub host\n")
	if _, err := a.ProcessPin(context.Background(), "pilot"); err == nil ||
		!strings.Contains(err.Error(), "aimem process pin failed: aimem: no process is selected for project pilot") {
		t.Errorf("no selection: %v", err)
	}
}

// FillProcess fills the offer's process pin and instruction digest from
// the hub for the body's project, keeps a body that names both, refuses a
// body that names one or no project, and sends nothing when the read fails.
func TestFillProcess(t *testing.T) {
	ctx := context.Background()
	var asked []string
	read := func(_ context.Context, project string) (HubPin, error) {
		asked = append(asked, project)
		var pin HubPin
		_ = json.Unmarshal([]byte(pinJSON(project)), &pin)
		return pin, nil
	}
	body := `{"worker_agent_id":"agent-b","task":{"hub_id":"hub-a","project_id":"pilot","task_id":"t-1"},"expected_revision":3}`
	out, res, err := FillProcess(ctx, []byte(body), read)
	if err != nil || res.Kept || res.Project != "pilot" {
		t.Fatalf("fill: %+v %v", res, err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	proc, _ := got["process"].(map[string]any)
	if proc["repo"] != "https://forge.example/team/process.git" || proc["commit"] != pinCommit || proc["manifest"] != "process/manifest.json" ||
		got["instruction_digest"] != pinDigest || got["worker_agent_id"] != "agent-b" || got["expected_revision"] != float64(3) {
		t.Errorf("filled body %s", out)
	}
	if len(asked) != 1 || asked[0] != "pilot" {
		t.Errorf("read for %v", asked)
	}

	own := `{"task":{"project_id":"pilot"},"process":{"repo":"r","commit":"c","manifest":"m"},"instruction_digest":"sha256:x"}`
	if out, res, err := FillProcess(ctx, []byte(own), read); err != nil || !res.Kept || string(out) != own || len(asked) != 1 {
		t.Errorf("own pin: %s %+v %v, read %v", out, res, err, asked)
	}
	for name, c := range map[string]struct{ body, want string }{
		"only the pin":    {`{"task":{"project_id":"pilot"},"process":{"repo":"r"}}`, "only one of process and instruction_digest"},
		"only the digest": {`{"task":{"project_id":"pilot"},"instruction_digest":"sha256:x"}`, "only one of process and instruction_digest"},
		"no project":      {`{"task":{"task_id":"t-1"}}`, "no task.project_id"},
		"not an object":   {`[1]`, "not a JSON object"},
	} {
		if _, _, err := FillProcess(ctx, []byte(c.body), read); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	failed := func(context.Context, string) (HubPin, error) {
		return HubPin{}, errors.New("aimem process pin failed: the selection predates stored sets")
	}
	if out, _, err := FillProcess(ctx, []byte(body), failed); err == nil || out != nil || !strings.Contains(err.Error(), "predates stored sets") {
		t.Errorf("a failed read: %s %v", out, err)
	}
}
