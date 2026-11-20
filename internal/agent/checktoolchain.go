package agent

import (
	"fmt"
	"os/exec"
	"slices"

	"github.com/BlackVS/aicrew/internal/toolchain"
)

// ReasonToolchainToolMissing is check's reason when a toolchain the
// operator selected for a worker's project has no first tool on the host.
const ReasonToolchainToolMissing = "toolchain_tool_missing"

// lookTool finds a toolchain's tool on check's PATH; tests replace it.
var lookTool = exec.LookPath

// checkToolchains reads, on a worker's home, the toolchains of its team's
// projects as the last session start recorded them (state/team.json).
//
// A project with a repository and no toolchain is a warning: the operator
// may mean it, and the selection is the operator's on aicrewd, not this
// host's. A selected toolchain whose tool is not found blocks: the operator
// chose it, and every task of the project would end in a failed test
// command, so the worker is not ready for work. Another role's home, or a
// home whose record predates the role, has neither line.
func (c *checker) checkToolchains() {
	rec, ok, err := LoadTeam(c.o.Home)
	switch {
	case err != nil:
		c.notice("the team's projects could not be read for the toolchain lines: " + err.Error())
		return
	case !ok || rec.Role != "worker":
		return
	}
	var missing []string
	for _, p := range rec.Projects {
		switch {
		case p.Toolchain == "" && p.Repository != nil:
			c.warn(fmt.Sprintf("project %s has no toolchain selected: this worker does not run its build and "+
				"test commands. The operator selects one with `aicrew team toolchain --team %s --project %s "+
				"--toolchain %s` (docs/BRING-UP.md); then %s, so that its launcher records the selection",
				p.ProjectID, rec.TeamID, p.ProjectID, toolchain.Names()[0], c.restartMember()))
		case p.Toolchain == "" || !toolchain.Valid(p.Toolchain):
			// A name this build does not know is serve's warning to upgrade.
		default:
			tool := toolchain.Tool(p.Toolchain)
			if _, err := lookTool(tool); err != nil && !slices.Contains(missing, tool) {
				missing = append(missing, tool)
				c.block(ReasonToolchainToolMissing, fmt.Sprintf("install `%s` on this host: project %s's toolchain "+
					"%s runs it, and it is not on the PATH check runs with. Then, from a shell that finds it: if "+
					"join ended blocked, rerun it, `%s`; otherwise %s, so that the member runs with that PATH "+
					"(docs/BRING-UP.md)", tool, p.ProjectID, p.Toolchain, joinRerun(c.o.Home), c.restartMember()))
			}
		}
	}
}

// restartMember is how this home's member starts again: its service in
// turns mode, which runs with the PATH of the shell that last started it;
// its client, through aicrew-agent run, in a stop-hook home.
func (c *checker) restartMember() string {
	home := shellArg(absHome(c.o.Home))
	if c.doc.wakeSettings().Mode == WakeTurns {
		return "restart the worker's service: `aicrew-agent service restart --home " + home + "`"
	}
	return "stop the member's client and start it again: `" + startCommand(firstClient(c.doc.clients()), home) + "`"
}

// firstClient is the first recorded client, or "<client>".
func firstClient(names []string) string {
	if len(names) == 0 {
		return "<client>"
	}
	return names[0]
}

func (c *checker) warn(s string) { c.rep.Warnings = append(c.rep.Warnings, s) }
