package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client discovery (decision K6): ask the selected client itself what it
// sees in the agent home, without a model call. Every command runs with the
// model endpoint pointed at a local port that closes each connection and a
// dummy key, so nothing reaches a provider and nothing is billed; the
// client's stored login and settings are never read or changed. Every step
// is bounded by a timeout, and print mode never shows Claude Code's
// workspace-trust dialog, so no step waits for a person.
//
// Measured on Claude Code 2.1.286 and OpenCode 1.18.32 and 2.0.19:
//   - claude -p … --output-format stream-json: the first "init" event lists
//     mcp_servers (name, status) and skills.
//   - claude mcp list: a project server not yet approved for interactive use
//     shows "Pending approval".
//   - opencode 1.x: `mcp list` (status per server) and `debug skill` (JSON).
//   - opencode 2.x: `serve` prints its password; /api/skill and /api/mcp
//     answer per directory, the catalogs loading in the background.

// Discovery is what a client reports for the agent home.
type Discovery struct {
	// AimemMCP is the aimem server's status: connected, failed, pending,
	// needs-auth, disabled, or absent when the client does not list it.
	AimemMCP  string
	MCPDetail string
	Skills    map[string]bool
	// ApprovalPending: Claude Code will ask, at the first interactive start
	// in this home, to trust the folder and approve the project server.
	ApprovalPending bool
	// Notes are facts worth reporting that do not block.
	Notes []string
}

// discoveryEnv is the environment discovery runs clients in.
type discoveryEnv struct {
	env     []string
	timeout time.Duration
}

// modelSink listens on a local port and closes every connection at once: a
// model request from a client under discovery fails there.
func modelSink() (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return "http://" + ln.Addr().String(), func() { ln.Close() }, nil
}

// clientEnv is base without the calling session's client and provider
// variables, plus the closed model endpoint and a dummy key. It keeps
// CLAUDE_CONFIG_DIR, the member's own Claude Code configuration directory
// when members share an account, so the probes read that member's client.
func clientEnv(base []string, sink string) []string {
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		k := strings.ToUpper(kv[:max(strings.IndexByte(kv, '='), 0)])
		if k == "CLAUDE_CONFIG_DIR" {
			out = append(out, kv)
			continue
		}
		if strings.HasPrefix(k, "CLAUDE") || strings.HasPrefix(k, "ANTHROPIC_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "ANTHROPIC_BASE_URL="+sink, "ANTHROPIC_API_KEY=dummy-not-a-key")
}

// capture runs one command in dir and returns its output.
func (p discoveryEnv) capture(ctx context.Context, dir, path string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir, cmd.Env = dir, p.env
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%s %s did not finish within %s", path, strings.Join(args, " "), p.timeout)
	}
	return out.String(), errb.String(), err
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

// ---- Claude Code

func claudeDiscover(ctx context.Context, p discoveryEnv, path, home string) (Discovery, error) {
	d, err := claudeInit(ctx, p, path, home)
	if err != nil {
		return d, err
	}
	out, errOut, err := p.capture(ctx, home, path, "mcp", "list")
	if err != nil {
		d.Notes = append(d.Notes, "`claude mcp list` failed, so the approval state is unknown: "+tail(errOut+out+err.Error(), 200))
		return d, nil
	}
	if st, _ := parseClaudeMCPList(out); st == "pending" {
		d.ApprovalPending = true
	}
	return d, nil
}

// claudeInit reads the init event of a print-mode run, then lets the run end
// on its own (it closes its MCP servers) or stops it at the timeout.
func claudeInit(ctx context.Context, p discoveryEnv, path, home string) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-p", "hi", "--output-format", "stream-json", "--verbose", "--max-turns", "1",
		"--no-session-persistence")
	cmd.Dir, cmd.Env = home, p.env
	cmd.WaitDelay = 2 * time.Second
	var errb bytes.Buffer
	cmd.Stderr = &errb
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		return Discovery{}, fmt.Errorf("claude could not be run: %w", err)
	}
	waitc := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		waitc <- err
	}()
	var d Discovery
	found := false
	r := bufio.NewReader(pr)
	for !found {
		line, rerr := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"subtype":"init"`)) {
			var perr error
			d, perr = parseClaudeInit(line)
			found = perr == nil
		}
		if rerr != nil {
			break
		}
	}
	go io.Copy(io.Discard, r) // let the run finish writing
	werr := <-waitc
	if !found {
		if ctx.Err() == context.DeadlineExceeded {
			return Discovery{}, fmt.Errorf("claude gave no init event within %s", p.timeout)
		}
		return Discovery{}, fmt.Errorf("claude gave no init event (%v): %s", werr, tail(errb.String(), 300))
	}
	return d, nil
}

func parseClaudeInit(line []byte) (Discovery, error) {
	var ev struct {
		MCPServers []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"mcp_servers"`
		Skills []string `json:"skills"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &ev); err != nil {
		return Discovery{}, err
	}
	d := Discovery{AimemMCP: "absent", Skills: map[string]bool{}}
	for _, s := range ev.MCPServers {
		if s.Name == "aimem" {
			d.AimemMCP = mcpState(s.Status)
		}
	}
	for _, s := range ev.Skills {
		d.Skills[s] = true
	}
	return d, nil
}

// mcpState normalizes a client's status word.
func mcpState(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.Contains(s, "pending"):
		return "pending"
	case strings.Contains(s, "auth"):
		return "needs-auth"
	case strings.Contains(s, "disabled"):
		return "disabled"
	case strings.Contains(s, "connected") && !strings.Contains(s, "dis"):
		return "connected"
	}
	return "failed"
}

// parseClaudeMCPList finds the aimem line: "aimem: <command> - <status>".
func parseClaudeMCPList(out string) (state, detail string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "aimem: ") {
			continue
		}
		i := strings.LastIndex(line, " - ")
		if i < 0 {
			return "failed", line
		}
		status := line[i+3:]
		return mcpState(status), status
	}
	return "absent", ""
}

// ---- OpenCode

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func opencodeDiscover(ctx context.Context, p discoveryEnv, path, home string, major int, skills []string) (Discovery, error) {
	if major >= 2 {
		return opencodeServe(ctx, p, path, home, skills)
	}
	out, errOut, err := p.capture(ctx, home, path, "mcp", "list")
	if err != nil {
		return Discovery{}, fmt.Errorf("opencode mcp list: %s", tail(errOut+out+err.Error(), 300))
	}
	d := Discovery{Skills: map[string]bool{}}
	d.AimemMCP, d.MCPDetail = parseOpencodeMCPList(out)
	out, errOut, err = p.capture(ctx, home, path, "debug", "skill")
	if err != nil {
		return d, fmt.Errorf("opencode debug skill: %s", tail(errOut+err.Error(), 300))
	}
	names, err := parseOpencodeSkills(out)
	if err != nil {
		return d, err
	}
	for _, n := range names {
		d.Skills[n] = true
	}
	return d, nil
}

var opencodeMCPLine = regexp.MustCompile(`(?m)[✓✗•○]\s+aimem\s+(\S[^\n]*?)\s*$`)

// parseOpencodeMCPList reads "✓ aimem connected" or "✗ aimem failed" and,
// for a failure, the error on the next line.
func parseOpencodeMCPList(out string) (state, detail string) {
	out = ansi.ReplaceAllString(out, "")
	loc := opencodeMCPLine.FindStringSubmatchIndex(out)
	if loc == nil {
		return "absent", ""
	}
	state = mcpState(out[loc[2]:loc[3]])
	if state == "failed" {
		rest := strings.SplitN(out[loc[1]:], "\n", 3)
		if len(rest) > 1 {
			detail = strings.TrimSpace(strings.Trim(strings.TrimSpace(rest[1]), "│|"))
		}
	}
	return state, detail
}

func parseOpencodeSkills(out string) ([]string, error) {
	i := strings.IndexByte(out, '[')
	if i < 0 {
		return nil, errors.New("opencode debug skill gave no skill list")
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&list); err != nil {
		return nil, fmt.Errorf("opencode debug skill: %w", err)
	}
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	return names, nil
}

var servePassword = regexp.MustCompile(`server password (\S+)`)

// opencodeServe asks OpenCode 2's own server, started for the home, and
// polls its catalogs until the required skills and aimem's status are in or
// the timeout passes. The server's password is read from its output and
// kept in memory only.
func opencodeServe(ctx context.Context, p discoveryEnv, path, home string, skills []string) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	port, err := freePort()
	if err != nil {
		return Discovery{}, err
	}
	cmd := exec.CommandContext(ctx, path, "serve", "--port", strconv.Itoa(port))
	cmd.Dir, cmd.Env = home, p.env
	cmd.WaitDelay = 2 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return Discovery{}, fmt.Errorf("opencode could not be run: %w", err)
	}
	defer func() { cancel(); cmd.Wait(); pw.Close() }()
	pwc := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sent := false
		for sc.Scan() {
			if m := servePassword.FindStringSubmatch(ansi.ReplaceAllString(sc.Text(), "")); m != nil && !sent {
				pwc <- m[1]
				sent = true
			}
		}
	}()
	var password string
	select {
	case password = <-pwc:
	case <-ctx.Done():
		return Discovery{}, fmt.Errorf("opencode serve printed no server password within %s", p.timeout)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	get := func(route string, out any) error {
		u := fmt.Sprintf("http://127.0.0.1:%d%s?%s", port, route, url.Values{"location[directory]": {home}}.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:"+password)))
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("opencode answered %d for %s", resp.StatusCode, route)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
	}
	d := Discovery{AimemMCP: "absent", Skills: map[string]bool{}}
	for {
		var sk struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		}
		var mcp struct {
			Data []struct {
				Name   string `json:"name"`
				Status struct {
					Status string `json:"status"`
					Error  string `json:"error"`
				} `json:"status"`
			} `json:"data"`
		}
		errS, errM := get("/api/skill", &sk), get("/api/mcp", &mcp)
		if errS == nil && errM == nil {
			d = Discovery{AimemMCP: "absent", Skills: map[string]bool{}}
			for _, s := range sk.Data {
				d.Skills[s.ID] = true
				if s.Name != "" {
					d.Skills[s.Name] = true
				}
			}
			for _, m := range mcp.Data {
				if m.Name == "aimem" {
					d.AimemMCP, d.MCPDetail = mcpState(m.Status.Status), m.Status.Error
				}
			}
			complete := d.AimemMCP != "absent" && d.AimemMCP != "pending"
			for _, s := range skills {
				complete = complete && d.Skills[s]
			}
			if complete {
				return d, nil
			}
		}
		select {
		case <-ctx.Done():
			if errS != nil || errM != nil {
				return d, fmt.Errorf("opencode's server did not answer: %v", errors.Join(errS, errM))
			}
			return d, nil // report what loaded within the bound
		case <-time.After(time.Second):
		}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// lookClient finds a client's executable: the home's client_command when it
// names this one client, else PATH.
func lookClient(name, clientCommand string, single bool) (string, error) {
	if clientCommand != "" && single {
		if _, err := os.Stat(clientCommand); err == nil {
			return clientCommand, nil
		}
		return exec.LookPath(clientCommand)
	}
	return exec.LookPath(name)
}
