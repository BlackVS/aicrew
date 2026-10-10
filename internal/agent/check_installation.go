package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// The check's view of the home's aimem installation (D-STORE): the
// member's credential in it, the carriers that point every process started
// in the home at it, and what outside the home could point a process
// elsewhere.

// provisionInstruction tells the operator how to provision the home's
// installation once.
func provisionInstruction(home, hub string) string {
	return fmt.Sprintf("the home's aimem installation (%s) holds no individual credential for hub %q. "+
		"Provision it once with the home's variables, AIMEM_STATE_DIR=%s and AIMEM_SOCKET=%s: run "+
		"`aimem hub add %s <url> --token-file -` and `aimem hub task-token %s --token-file -`, each reading the "+
		"user-scoped token your aimem operator issues for this member on standard input, then rerun", AimemDir(home), hub, AimemDir(home), AimemSocket(home), hub, hub)
}

// checkCredential: the verdict uses the home's installation, which must
// hold an individual credential for agent.json's hub.
func (c *checker) checkCredential(ctx context.Context, aimem string) {
	hub := str(c.doc.aicrew, "aimem_hub")
	if hub == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, known, err := ExecAimem{Command: aimem, Hub: hub, Home: c.o.Home}.JoinAimem().Credential(ctx)
	switch {
	case err != nil && strings.Contains(err.Error(), "is not configured"):
		c.block("credential_missing", provisionInstruction(c.o.Home, hub))
	case err != nil:
		c.block("aimem_failed", tail(err.Error(), 300)+": check the home's aimem installation ("+AimemDir(c.o.Home)+
			"), then rerun `aicrew-agent check`")
	case !known:
		c.notice("this aimem cannot report its credential: the identity proof will check it")
	case st.Credential != "set":
		c.block("credential_missing", provisionInstruction(c.o.Home, hub))
	case st.State == "refused":
		c.block("credential_refused", fmt.Sprintf("the aimem hub %q does not accept the credential in the home's "+
			"installation (revoked, expired or unknown): ask your aimem operator to reissue it, install it with "+
			"`aimem hub task-token %s --token-file -` under the home's AIMEM_STATE_DIR=%s and AIMEM_SOCKET=%s, then rerun",
			hub, hub, AimemDir(c.o.Home), AimemSocket(c.o.Home)))
	case st.State != "active":
		c.notice(fmt.Sprintf("the aimem hub %q could not confirm the home's credential (%s): it is installed; "+
			"the hub decides when it is reachable", hub, st.State))
	}
}

// checkInstallation reports the carriers that disagree with the home, and
// what outside the home names another installation.
func (c *checker) checkInstallation(sel []string) {
	home := c.o.Home
	if n := len(AimemSocket(home)); n > unixSocketMax {
		c.notice(fmt.Sprintf("the home's aimem socket path is %d bytes, over the %d-byte Unix socket limit: aimem's "+
			"local service cannot listen there (a home runs none); a shorter --home avoids it", n, unixSocketMax))
	}
	if slices.Contains(sel, "claude") {
		c.checkCarrier(".claude/settings.json", func(doc map[string]json.RawMessage) json.RawMessage { return doc["env"] })
		c.checkWakeHook()
		c.checkDenyRules()
		c.checkCarrier(".mcp.json (mcpServers.aimem)", func(doc map[string]json.RawMessage) json.RawMessage {
			var servers map[string]struct {
				Env json.RawMessage `json:"env"`
			}
			_ = json.Unmarshal(doc["mcpServers"], &servers)
			return servers["aimem"].Env
		})
		c.checkUserServers()
	}
	c.checkForeignVars()
}

// checkCarrier blocks when a carrier file does not name the home's
// installation.
func (c *checker) checkCarrier(label string, env func(map[string]json.RawMessage) json.RawMessage) {
	file, _, _ := strings.Cut(label, " ")
	raw, err := os.ReadFile(filepath.Join(c.o.Home, filepath.FromSlash(file)))
	var doc map[string]json.RawMessage
	var vars map[string]string
	if err == nil && json.Unmarshal(raw, &doc) == nil {
		_ = json.Unmarshal(env(doc), &vars)
	}
	for _, v := range aimemVars(c.o.Home) {
		if !samePath(vars[v[0]], v[1]) {
			c.block("carrier_mismatch", fmt.Sprintf("%s does not set %s=%s, so a client started in this home "+
				"would not use the home's aimem installation: rerun `aicrew-agent join --home %s`, and merge any "+
				"%s.aicrew-new it writes", label, v[0], v[1], quoteArg(c.o.Home), file))
			return
		}
	}
}

// checkWakeHook notes a home whose managed Claude Code settings do not hold
// the wake-up's Stop hook as join writes it: the member then waits for a
// human message after each turn. A notice, never a blocker.
func (c *checker) checkWakeHook() {
	raw, err := os.ReadFile(filepath.Join(c.o.Home, ".claude", "settings.json"))
	var doc map[string]json.RawMessage
	if err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	want, _ := json.Marshal(wakeHooks(c.o.Home))
	var got, exp any
	_ = json.Unmarshal(doc["hooks"], &got)
	_ = json.Unmarshal(want, &exp)
	if !reflect.DeepEqual(got, exp) {
		c.notice(fmt.Sprintf(".claude/settings.json does not hold the wake-up's Stop hook for this aicrew-agent, so "+
			"the member waits for a human message after each turn: rerun `aicrew-agent join --home %s`, and merge "+
			"any .claude/settings.json.aicrew-new it writes", quoteArg(c.o.Home)))
	}
}

// checkDenyRules notes a home whose managed Claude Code settings lack any
// of the deny rules join writes: the client would then let the member read
// the home's credentials or weaken git's TLS through the tools those rules
// name. A notice, as for the Stop hook.
func (c *checker) checkDenyRules() {
	raw, err := os.ReadFile(filepath.Join(c.o.Home, ".claude", "settings.json"))
	var doc struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	var missing []string
	for _, r := range denyRules() {
		if !slices.Contains(doc.Permissions.Deny, r) {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		c.notice(fmt.Sprintf(".claude/settings.json lacks %d of the %d managed deny rules (first: %s), so Claude Code "+
			"would let the member read the home's credentials or weaken git's TLS through its tools: rerun "+
			"`aicrew-agent join --home %s`, and merge any .claude/settings.json.aicrew-new it writes",
			len(missing), len(denyRules()), missing[0], quoteArg(c.o.Home)))
	}
}

// aimemInstallerKnobs are the AIMEM_* variables only aimem's installers read
// (boot.sh, boot.ps1, install.sh, install.ps1 and install-hub.sh at
// v0.10.0): the aimem binary never reads them, so a member process that
// inherits one acts on nothing, and the environment check ignores them.
var aimemInstallerKnobs = []string{
	"AIMEM_BIN_DIR", "AIMEM_CLAUDE_SETTINGS", "AIMEM_CODEX_HOME", "AIMEM_NO_SYSTEMD", "AIMEM_OC_PLUGIN_DIR",
	"AIMEM_PREBUILT", "AIMEM_REINSTALL", "AIMEM_REPO", "AIMEM_TARGET_VERSION", "AIMEM_UNIT_DIR",
	"AIMEM_UPGRADE_WAIT", "AIMEM_USER_ONLY", "AIMEM_VERSION",
}

// checkForeignVars notes AIMEM_* variables, in the environment or in
// aimem's ~/.config/aimem/env, that do not name the home's installation,
// other than aimem's installer-only knobs. Only their names are reported: a
// value may be a secret.
func (c *checker) checkForeignVars() {
	own := map[string]string{}
	for _, v := range aimemVars(c.o.Home) {
		own[v[0]] = v[1]
	}
	foreign := func(kvs []string) []string {
		var out []string
		for _, kv := range kvs {
			k, v, _ := strings.Cut(kv, "=")
			k = strings.ToUpper(k)
			if !strings.HasPrefix(k, "AIMEM_") || (own[k] != "" && samePath(v, own[k])) || slices.Contains(out, k) ||
				slices.Contains(aimemInstallerKnobs, k) {
				continue
			}
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if names := foreign(os.Environ()); len(names) > 0 {
		c.notice(fmt.Sprintf("the environment sets %s: aicrew-agent replaces AIMEM_STATE_DIR and AIMEM_SOCKET "+
			"with the home's for what it starts, but every other process inherits them; a member needs none of them "+
			"(aimem's installer-only knobs, such as AIMEM_USER_ONLY and AIMEM_VERSION, are not listed: aimem itself "+
			"never reads them)", strings.Join(names, ", ")))
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(uh, ".config", "aimem", "env")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			k, v, _ := strings.Cut(l, "=")
			lines = append(lines, strings.TrimSpace(k)+"="+strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	if names := foreign(lines); len(names) > 0 {
		c.notice(fmt.Sprintf("%s sets %s: aimem adds them to any aimem process that lacks its own value; it must "+
			"not set AIMEM_STATE_DIR or AIMEM_SOCKET, which belong to the user's own installation", path,
			strings.Join(names, ", ")))
	}
}

// checkUserServers notes an aimem MCP server in Claude Code's user file
// (.claude.json, under CLAUDE_CONFIG_DIR when set): at user scope it runs in
// every session, and at local scope for this home it replaces the managed
// entry.
func (c *checker) checkUserServers() {
	uh, _ := os.UserHomeDir()
	path := filepath.Join(uh, ".claude.json")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		path = filepath.Join(d, ".claude.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	type servers map[string]struct {
		Command string `json:"command"`
	}
	var doc struct {
		MCPServers servers `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers servers `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	isAimem := func(name, command string) bool {
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(filepath.FromSlash(command))), ".exe")
		return name == "aimem" || base == "aimem" || (command != "" && command == c.aimemCommand())
	}
	var found []string
	for name, s := range doc.MCPServers {
		if isAimem(name, s.Command) {
			found = append(found, fmt.Sprintf("%q at user scope", name))
		}
	}
	for dir, p := range doc.Projects {
		if !samePath(dir, c.o.Home) {
			continue
		}
		for name, s := range p.MCPServers {
			if isAimem(name, s.Command) {
				found = append(found, fmt.Sprintf("%q at local scope for this home", name))
			}
		}
	}
	if len(found) > 0 {
		sort.Strings(found)
		c.notice(fmt.Sprintf("%s has an aimem MCP server (%s) that the home does not manage: one at user scope "+
			"runs in every session of this account, and one at local scope replaces the managed entry; remove it "+
			"with `claude mcp remove` unless it is meant to be there", path, strings.Join(found, ", ")))
	}
}

// samePath compares two paths as the file system would: cleaned, with
// either separator, and without case on Windows.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	a, b = filepath.Clean(filepath.FromSlash(a)), filepath.Clean(filepath.FromSlash(b))
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
