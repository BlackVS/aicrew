package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Client wiring (decision K3): one managed MCP entry per client, in a
// project-level file inside the agent home, so the client started there by
// `aicrew-agent run` finds aimem's MCP server; `AIMEM_TEAM_SESSION`, which
// the launcher sets, switches that same server to team mode. Nothing else in
// those files is touched, no hook is installed, and no user-level client
// configuration is read or written.
//
// The entry follows the managed-file rule (docs/WORKSPACE.md): it is added
// when missing, updated only while it still matches the digest recorded at
// its last managed write, and otherwise left as it is, with the proposed
// document written beside the file as <file>.aicrew-new.

// wiringEntry is a client's managed entry: file, parent key and value.
type wiringEntry struct {
	client string
	file   string // relative to the home
	parent string // the object holding the entry
	name   string
	value  any
	// skeleton is the rest of a new file.
	skeleton map[string]any
}

// managedKey is the entry's record name in agent.json's "managed".
func (w wiringEntry) managedKey() string { return w.file + "#" + w.parent + "." + w.name }

// wiringFor is the entry of a client, running aimem as command.
func wiringFor(client, command string) wiringEntry {
	if client == "opencode" {
		return wiringEntry{client: client, file: "opencode.json", parent: "mcp", name: "aimem",
			value:    map[string]any{"type": "local", "command": []string{command, "mcp"}, "enabled": true},
			skeleton: map[string]any{"$schema": "https://opencode.ai/config.json"}}
	}
	return wiringEntry{client: client, file: ".mcp.json", parent: "mcpServers", name: "aimem",
		value: map[string]any{"command": command, "args": []string{"mcp"}}}
}

// canonical is v as compact JSON with sorted keys, for comparison and digests.
func canonical(v any) []byte {
	raw, _ := json.Marshal(v)
	var g any
	if json.Unmarshal(raw, &g) != nil {
		return raw
	}
	out, _ := json.Marshal(g)
	return out
}

// planWiring decides the entry's change and the document to write.
func planWiring(home string, w wiringEntry, recorded string) (FileChange, []byte, error) {
	path := filepath.Join(home, w.file)
	ch := FileChange{Path: w.file + " (" + w.parent + "." + w.name + ")"}
	want := canonical(w.value)
	raw, err := os.ReadFile(path)
	doc := map[string]json.RawMessage{}
	switch {
	case errors.Is(err, os.ErrNotExist):
		for k, v := range w.skeleton {
			doc[k], _ = json.Marshal(v)
		}
		ch.Action = "create"
	case err != nil:
		return ch, nil, err
	case json.Unmarshal(raw, &doc) != nil || doc == nil:
		// Not an object: never rewritten; the proposed file goes beside it.
		doc = map[string]json.RawMessage{}
		for k, v := range w.skeleton {
			doc[k], _ = json.Marshal(v)
		}
		ch.Action = "conflict"
	}
	parent := map[string]json.RawMessage{}
	if p, ok := doc[w.parent]; ok && json.Unmarshal(p, &parent) != nil {
		parent = map[string]json.RawMessage{} // a non-object parent is the user's: conflict below
		ch.Action = "conflict"
	}
	if ch.Action == "" {
		cur, ok := parent[w.name]
		switch {
		case !ok:
			ch.Action = "create"
		case bytes.Equal(canonical(json.RawMessage(cur)), want):
			ch.Action = "unchanged"
		case recorded != "" && recorded == digestOf(canonical(json.RawMessage(cur))):
			ch.Action = "update"
		default:
			ch.Action = "conflict"
		}
	}
	parent[w.name] = want
	doc[w.parent], _ = json.Marshal(parent)
	out, err := json.MarshalIndent(doc, "", "  ")
	return ch, append(out, '\n'), err
}

// applyWiring writes the planned document: the file itself, or beside it
// on a conflict.
func applyWiring(home string, w wiringEntry, ch FileChange, doc []byte) error {
	path := filepath.Join(home, w.file)
	switch ch.Action {
	case "create", "update":
		return writeAtomic(path, doc)
	case "conflict":
		return writeAtomic(path+".aicrew-new", doc)
	}
	return nil
}
