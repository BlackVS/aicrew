//go:build realaimem

package realaimem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C1: the operator's control of the team (docs/DESIGN-CONTROL-PLANE.md,
// section 4.2 and D12). The operator reads the team's state, pauses the
// team, and nudges the worker: the note waits in the worker's inbox, and the
// worker's keep-alive loop (the Stop hook's wait-inbox) starts no turn on it
// and says the team is paused. Until the headless turns exist (A3), that is
// what "no turn" means. After the resume the same loop wakes on the note.
// A member's own pause holds that member alone, and neither pause touches
// the members' sessions.
func (h *harness) c1Control(t *testing.T) {
	sc := h.report.scenario(t, "C1")
	coord, worker := h.members["coord"], h.members["worker"]
	aicrew, agentBin := filepath.Join(h.bin, "aicrew"), filepath.Join(h.bin, "aicrew-agent")
	h.pageWith(coord, "nothing-is-named-this")
	h.pageWith(worker, "nothing-is-named-this")
	for _, m := range []*member{coord, worker} {
		h.setWake(m, `{"wait_seconds": 8}`)
	}
	hook := func(m *member) string {
		r := h.run(m.env, []byte(`{"session_id":"e2e-c1-`+m.name+`"}`), agentBin, "wait-inbox", "-home", m.home)
		return r.stdout + r.stderr
	}

	before := h.teamState(sc)
	sc.check("aicrew team state lists both members with their role and session, none paused",
		before["coord"].Role == "coordinator" && before["worker"].Role == "worker" && before["coord"].Session.ID != "" &&
			before["worker"].Session.ID != "" && before["coord"].State != "paused" && before["worker"].State != "paused", before)

	paused := h.must(h.opEnv, nil, aicrew, "team", "pause", "--team-name", "e2e", "--reason", "e2e upgrade")
	sc.check("aicrew team pause pauses the team", strings.Contains(paused, `"changed": true`) &&
		strings.Contains(paused, `"reason": "e2e upgrade"`), paused)
	nudge := h.must(h.opEnv, nil, aicrew, "member", "nudge", "--team-name", "e2e", "--agent", "worker",
		"--note", "C1: the upgraded agent is installed")
	sc.check("aicrew member nudge writes one message to the worker", strings.Contains(nudge, `"message_id"`) &&
		strings.Contains(nudge, worker.agentID), nudge)
	out := hook(worker)
	sc.check("the paused worker's keep-alive loop starts no turn on the new message: it says the team is paused",
		strings.Contains(out, `"decision":"block"`) && strings.Contains(out, "The operator paused your team") &&
			!strings.Contains(out, "new message"), out)
	st := h.teamState(sc)
	sc.check("aicrew team state shows both members paused and the note unacknowledged",
		st["coord"].State == "paused" && st["worker"].State == "paused" && st["worker"].Unacknowledged == 1, st)

	h.must(h.opEnv, nil, aicrew, "team", "resume", "--team-name", "e2e")
	out = hook(worker)
	sc.check("after aicrew team resume the worker's keep-alive loop wakes on the note",
		strings.Contains(out, `"decision":"block"`) && strings.Contains(out, "1 new message"), out)
	page := h.pageWith(worker, "C1: the upgraded agent is installed")
	sc.check("the worker's inbox delivers the operator's note", strings.Contains(page, `"operator"`) &&
		strings.Contains(page, `"note": "C1: the upgraded agent is installed"`), page)
	h.ackAll(sc, worker, page)

	// A member's own pause holds that member alone.
	h.must(h.opEnv, nil, aicrew, "member", "pause", "--team-name", "e2e", "--agent", worker.agentID)
	h.must(h.opEnv, nil, aicrew, "member", "nudge", "--team-name", "e2e", "--agent", "coord", "--note", "C1: for the coordinator")
	h.must(h.opEnv, nil, aicrew, "member", "nudge", "--team-name", "e2e", "--agent", "worker", "--note", "C1: held for the worker")
	out = hook(coord)
	sc.check("with only the worker paused, the coordinator's loop wakes on its note", strings.Contains(out, "1 new message"), out)
	out = hook(worker)
	sc.check("the paused worker's loop does not: it says the operator paused it",
		strings.Contains(out, "The operator paused you") && !strings.Contains(out, "new message"), out)
	h.must(h.opEnv, nil, aicrew, "member", "resume", "--team-name", "e2e", "--agent", "worker")
	out = hook(worker)
	sc.check("after aicrew member resume the worker's loop wakes on its note", strings.Contains(out, "1 new message"), out)
	for _, m := range []*member{coord, worker} {
		h.ackAll(sc, m, h.pageWith(m, "C1: "))
	}

	after := h.teamState(sc)
	same := true
	for _, name := range []string{"coord", "worker"} {
		same = same && after[name].Session.ID == before[name].Session.ID && after[name].Session.Generation == before[name].Session.Generation
	}
	sc.check("the pauses left both members' sessions as they were", same, after)
	for _, m := range []*member{coord, worker} {
		h.setWake(m, `{}`)
	}
	sc.finish()
}

type memberStateLite struct {
	Label   string `json:"label"`
	Role    string `json:"role"`
	State   string `json:"state"`
	Session struct {
		ID         string `json:"id"`
		Generation int64  `json:"generation"`
	} `json:"session"`
	Unacknowledged int `json:"unacknowledged"`
}

// teamState is aicrew team state's members by label.
func (h *harness) teamState(sc *scenario) map[string]memberStateLite {
	raw := h.must(h.opEnv, nil, filepath.Join(h.bin, "aicrew"), "team", "state", "--team-name", "e2e")
	var st struct {
		Members []memberStateLite `json:"members"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		sc.check("aicrew team state prints the team state", false, raw)
	}
	out := map[string]memberStateLite{}
	for _, m := range st.Members {
		out[m.Label] = m
	}
	return out
}

// setWake sets the wake settings of mem's agent.json (a documented field).
func (h *harness) setWake(mem *member, wake string) {
	file := filepath.Join(mem.home, "agent.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		h.t.Fatal(err)
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil {
		h.t.Fatal(err)
	}
	f["wake"] = json.RawMessage(wake)
	out, _ := json.MarshalIndent(f, "", "  ")
	info, _ := os.Stat(file)
	if err := os.WriteFile(file, out, info.Mode().Perm()); err != nil {
		h.t.Fatal(err)
	}
}
