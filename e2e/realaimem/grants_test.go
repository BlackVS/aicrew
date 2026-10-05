//go:build realaimem

package realaimem

import (
	"strings"
	"testing"
	"time"
)

// G1: the team's projects are the grants its hub holds, read live at every
// offer and claim (354c-1a). A grant the hub's operator revokes refuses the
// next offer and claim before anything reaches aimem's reservation, the
// claim by aicrewd's live read (project_not_granted); granted again, the
// offer commits.
func (h *harness) g1Grants(t *testing.T) {
	sc := h.report.scenario(t, "G1")
	coord, worker, indep := h.members["coord"], h.members["worker"], h.members["indep"]
	host := h.hostEnv()
	task := h.createTask(sc, "G1 revoked grant")

	h.must(host, nil, h.identity("team", "revoke", "--peer", serviceID, "--team-name", "e2e", "--project", projectID)...)
	// The coordinator's client reads the task's dependencies through aimem
	// first, and aimem already refuses the team's read of a revoked
	// project: the client stops there (dependencies_open, the task
	// unreadable). Either refusal comes before anything begins.
	ans, _ := h.step(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour)))
	sc.check("the offer of a revoked project is refused before it begins", refusedAtBegin(ans, "project_not_granted") ||
		(refusedAtBegin(ans, "dependencies_open") && strings.Contains(ans.Error.Message, "unreadable")), errorOf(ans))
	ans, _ = h.step(sc, indep, "claim", "", "", h.claimBody(task))
	sc.check("aicrewd refuses the claim of a revoked project project_not_granted", refusedAtBegin(ans, "project_not_granted"), errorOf(ans))
	sc.check("aicrew: nothing began", len(h.attemptsOfTask(sc, task.ID)) == 0)
	sc.check("aimem: nothing was sent", holdState(h.hold(sc, task.ID)) != "held" && h.readTask(sc, task.ID).Revision == task.Revision)

	h.must(host, nil, h.identity("team", "grant", "--peer", serviceID, "--team-name", "e2e", "--project", projectID)...)
	id := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	sc.check("granted again, the offer commits", id != "")
	h.committed(sc, coord, "withdraw", id, task.ID, map[string]any{})
	sc.finish()
}

// errorOf is a step answer's refusal, for the report.
func errorOf(ans StepAnswerLite) string {
	if ans.Error == nil {
		return ans.Status
	}
	return ans.Status + " " + ans.Error.Code + ": " + ans.Error.Message
}
