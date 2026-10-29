// Command aicrew-simclient stands in for Claude Code in the pilot
// simulation's member containers (docs/DESIGN-PILOT-SIMULATION.md, section
// 3), installed there as `claude` on PATH. It answers only what
// `aicrew-agent check` and serve's model probe ask a client:
//   - `--version`: the Claude Code version aicrew is tested with;
//   - `mcp list` and the init event of `-p hi --output-format stream-json`:
//     what the home wires, found the way Claude Code finds it (the home's
//     .mcp.json, the skill directories) and its aimem server reached with a
//     real MCP initialize, so a green check means the wiring works;
//   - the model probe (`-p … --strict-mcp-config`): one finished turn, with
//     no model call and no cost;
//   - a turn (`-p … --session-id ID`, the brief on standard input): the
//     role's script (turn.go).
//
// Anything else exits 2 with "not simulated".
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/agent"
)

// simModel is the model the probe's turn reports when none is configured.
const simModel = "aicrew-sim"

// mcpTimeout bounds the MCP initialize with the home's aimem.
const mcpTimeout = 30 * time.Second

func main() {
	wd, _ := os.Getwd()
	os.Exit(run(context.Background(), os.Args[1:], wd, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, dir string, stdout, stderr io.Writer) int {
	switch {
	case slices.Equal(args, []string{"--version"}):
		v, err := testedClaude()
		if err != nil {
			fmt.Fprintln(stderr, "aicrew-simclient:", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s (Claude Code)\n", v)
		return 0
	case slices.Equal(args, []string{"mcp", "list"}):
		return mcpList(ctx, dir, stdout)
	case turnMode(args):
		return turnRun(ctx, dir, os.Stdin, stdout, newMember(dir))
	case printMode(args) && slices.Contains(args, "--strict-mcp-config"):
		return probe(args, stdout)
	case printMode(args) && flag(args, "-p") == "hi":
		return initEvent(ctx, dir, stdout)
	}
	fmt.Fprintf(stderr, "aicrew-simclient: not simulated: %s\n", strings.Join(args, " "))
	return 2
}

// printMode reports a print-mode run with the stream-json output.
func printMode(args []string) bool {
	return slices.Contains(args, "-p") && flag(args, "--output-format") == "stream-json"
}

// flag is the value after name in args, or "".
func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// testedClaude is the Claude Code release aicrew's supported set names as
// tested.
func testedClaude() (string, error) {
	set, err := agent.LoadSupported()
	if err != nil {
		return "", err
	}
	r := set.Components["claude"].Ranges
	if len(r) == 0 || r[len(r)-1].Tested == "" {
		return "", errors.New("the supported set names no tested claude")
	}
	return r[len(r)-1].Tested, nil
}

// server is one entry of the home's .mcp.json.
type server struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// aimemServer reads the aimem entry of dir/.mcp.json; ok is false when the
// file or the entry is absent.
func aimemServer(dir string) (server, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if errors.Is(err, os.ErrNotExist) {
		return server{}, false, nil
	}
	if err != nil {
		return server{}, false, err
	}
	var doc struct {
		MCPServers map[string]server `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return server{}, false, fmt.Errorf(".mcp.json: %w", err)
	}
	s, ok := doc.MCPServers["aimem"]
	return s, ok, nil
}

// connect starts the server and completes an MCP initialize with it.
func connect(ctx context.Context, s server) error {
	_, err := exchange(ctx, s, nil)
	return err
}

// callTool calls one tool after the initialize, as the client does, and
// returns the text of its result.
func callTool(ctx context.Context, s server, tool string, args map[string]any) (string, error) {
	raw, err := exchange(ctx, s, map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Content) == 0 {
		return "", fmt.Errorf("%s: a result without text", tool)
	}
	if res.IsError {
		return "", fmt.Errorf("%s: %s", tool, res.Content[0].Text)
	}
	return res.Content[0].Text, nil
}

// exchange starts the server, sends the initialize and, when call is not
// nil, the initialized notice and one tools/call with call as its params;
// it returns the result of the last request.
func exchange(ctx context.Context, s server, call map[string]any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Command, s.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05",` +
		`"capabilities":{},"clientInfo":{"name":"aicrew-simclient","version":"1"}}}` + "\n"
	last := "1"
	if call != nil {
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": call})
		if err != nil {
			return nil, err
		}
		req += `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + string(raw) + "\n"
		last = "2"
	}
	if _, err := io.WriteString(in, req); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &resp) != nil || string(resp.ID) != last {
			continue
		}
		if resp.Error != nil {
			return nil, errors.New(resp.Error.Message)
		}
		if len(resp.Result) == 0 {
			return nil, errors.New("a response without a result")
		}
		return resp.Result, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("the server ended without answering")
}

// aimemStatus is the aimem server's state in Claude Code's words, and the
// command line it runs; absent is "".
func aimemStatus(ctx context.Context, dir string) (status, command string) {
	s, ok, err := aimemServer(dir)
	switch {
	case err != nil:
		return "failed", ""
	case !ok:
		return "", ""
	}
	command = strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
	if connect(ctx, s) != nil {
		return "failed", command
	}
	return "connected", command
}

func mcpList(ctx context.Context, dir string, stdout io.Writer) int {
	fmt.Fprintln(stdout, "Checking MCP server health...")
	fmt.Fprintln(stdout)
	switch status, command := aimemStatus(ctx, dir); status {
	case "connected":
		fmt.Fprintf(stdout, "aimem: %s - ✓ Connected\n", command)
	case "failed":
		fmt.Fprintf(stdout, "aimem: %s - ✗ Failed to connect\n", command)
	default:
		fmt.Fprintln(stdout, "No MCP servers configured.")
	}
	return 0
}

// skills are the skills Claude Code would load: each directory holding a
// SKILL.md under the home's .claude/skills and the user's.
func skills(dir string) []string {
	user := os.Getenv("CLAUDE_CONFIG_DIR")
	if user == "" {
		if uh, err := os.UserHomeDir(); err == nil {
			user = filepath.Join(uh, ".claude")
		}
	}
	seen := map[string]bool{}
	for _, root := range []string{filepath.Join(dir, ".claude", "skills"), filepath.Join(user, "skills")} {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(root, e.Name(), "SKILL.md")); err == nil {
				seen[e.Name()] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

type mcpEntry struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func initEvent(ctx context.Context, dir string, stdout io.Writer) int {
	servers := []mcpEntry{}
	if status, _ := aimemStatus(ctx, dir); status != "" {
		servers = append(servers, mcpEntry{"aimem", status})
	}
	emit(stdout, map[string]any{"type": "system", "subtype": "init", "cwd": dir, "model": simModel,
		"mcp_servers": servers, "skills": skills(dir)})
	emit(stdout, map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "",
		"total_cost_usd": 0})
	return 0
}

func probe(args []string, stdout io.Writer) int {
	model := flag(args, "--model")
	if model == "" {
		model = simModel
	}
	usage := map[string]any{"input_tokens": 1, "output_tokens": 1}
	emit(stdout, map[string]any{"type": "system", "subtype": "init", "model": model, "mcp_servers": []mcpEntry{},
		"skills": []string{}})
	emit(stdout, map[string]any{"type": "assistant", "message": map[string]any{"model": model, "usage": usage,
		"content": []map[string]any{{"type": "text", "text": "OK"}}}})
	emit(stdout, map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "OK",
		"total_cost_usd": 0, "usage": usage})
	return 0
}

func emit(w io.Writer, ev map[string]any) {
	raw, _ := json.Marshal(ev)
	fmt.Fprintf(w, "%s\n", raw)
}
