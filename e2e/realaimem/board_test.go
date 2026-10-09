//go:build realaimem

package realaimem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// B1: the board wake (docs/DESIGN-CONTROL-PLANE.md, A1; task 5570).
// aicrewd reads the hub's board feed each reconcile tick with its
// board.read credential, and a task of the granted project that becomes
// READY reaches the coordinator's inbox as a board.changed announcement,
// on which the keep-alive loop (the Stop hook's wait-inbox) wakes it; until
// the headless turns exist (A3), that is what "without a typed prompt"
// means. The coordinator then offers the task and the worker accepts, and
// aicrew's own steps are not announced. A restarted aicrewd announces
// nothing again; a task cancelled on the board while the worker holds it
// is announced once.
func (h *harness) b1BoardWake(t *testing.T) {
	sc := h.report.scenario(t, "B1")
	coord, worker := h.members["coord"], h.members["worker"]
	agentBin := filepath.Join(h.bin, "aicrew-agent")
	// Both members idle: nothing pending in either inbox.
	h.pageWith(coord, "nothing-is-named-this")
	h.pageWith(worker, "nothing-is-named-this")

	task := h.createTaskIn(sc, "B1 board wake", "BACKLOG")
	time.Sleep(boardTick + 5*time.Second)
	sc.check("a task created in BACKLOG wakes no one", h.boardCount(sc, task.ID, "") == 0, h.boardCount(sc, task.ID, ""))

	moved := time.Now()
	task = h.setTaskState(sc, task.ID, "READY")
	woke := h.waitBoard(sc, task.ID, "READY", 2*boardTick+10*time.Second)
	sc.check(fmt.Sprintf("the task moved to READY reaches the coordinator's inbox as board.changed within a tick (%s)",
		time.Since(moved).Round(time.Second)), woke, task.ID)
	hook := h.run(coord.env, []byte(`{"session_id":"e2e-b1"}`), agentBin, "wait-inbox", "-home", coord.home)
	sc.check("the coordinator's keep-alive loop wakes on it: the Stop hook blocks the stop and names it",
		strings.Contains(hook.stdout, `"decision":"block"`) && strings.Contains(hook.stdout, "lifecycle"), hook.stdout+hook.stderr)
	page := h.pageWith(coord, task.ID)
	sc.check("the announcement names the task and the change, and carries the board change",
		strings.Contains(page, `"board"`) && strings.Contains(page, `"to": "READY"`) && strings.Contains(page, `"from": "BACKLOG"`) &&
			strings.Contains(page, "moved from BACKLOG to READY on the board"), page)
	h.ackAll(sc, coord, page)

	// The coordinator acts on it: it offers the task, and the worker
	// accepts. aicrew's own steps are not announced back.
	id := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	h.committed(sc, worker, "accept", id, task.ID, map[string]string{"instruction_digest": instructionHash})
	time.Sleep(boardTick + 5*time.Second)
	sc.check("aicrew's own offer and accept are not announced", h.boardCount(sc, task.ID, "") == 1, h.boardCount(sc, task.ID, ""))

	// Restarted, aicrewd reads on from the cursor it stored with the
	// announcement: nothing is announced again.
	h.restartAicrewd()
	time.Sleep(boardTick + 5*time.Second)
	sc.check("after aicrewd restarts, the task already announced is not announced again", h.boardCount(sc, task.ID, "READY") == 1,
		h.boardCount(sc, task.ID, "READY"))

	// Cancelled on the board while the worker holds it: one announcement
	// naming the change.
	h.recoverHold(sc, task.ID, "cancel")
	cancelled := h.waitBoard(sc, task.ID, "CANCELLED", 2*boardTick+10*time.Second)
	time.Sleep(boardTick + 5*time.Second)
	sc.check("the task cancelled on the board while the worker held it is announced once, naming the change",
		cancelled && h.boardCount(sc, task.ID, "CANCELLED") == 1, h.boardCount(sc, task.ID, "CANCELLED"))
	page = h.pageWith(coord, `"to": "CANCELLED"`)
	sc.check("the coordinator reads it: the task, CANCELLED, and that the team has an attempt for it",
		strings.Contains(page, task.ID) && strings.Contains(page, "The team has an attempt for it."), page)
	h.ackAll(sc, coord, page)
	closed := h.waitAttempt(sc, id, 60*time.Second, func(a attemptRow) bool { return a.State == "closed" })
	sc.check("aicrewd closes the cancelled task's attempt as recovered, freeing the worker", closed, h.attempt(sc, id))
	h.pageWith(worker, "nothing-is-named-this")
	sc.finish()
}

// boardTick is aicrewd's board read interval: the reconcile tick.
const boardTick = 15 * time.Second

// createTaskIn creates a task in the project in state.
func (h *harness) createTaskIn(sc *scenario, title, state string) taskRef {
	sc.t.Helper()
	content := taskContent(title, nil)
	content["state"] = state
	var task aimemTask
	status := h.hubJSON(http.MethodPost, "/v1/projects/"+projectID+"/tasks", map[string]string{"Idempotency-Key": newKey()},
		content, &task)
	sc.require("aimem creates task "+title+" in "+state, status == http.StatusCreated && task.ID != "", status)
	return taskRef{ID: task.ID, Revision: task.Revision}
}

// boardCount counts the board.changed announcements of task in aicrew's
// store, of the changes to state ("" for any).
func (h *harness) boardCount(sc *scenario, task, state string) int {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	rows, err := db.Query(`SELECT board FROM messages WHERE task_id = ? AND board != ''`, task)
	if err != nil {
		sc.t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var raw string
		var d struct {
			To string `json:"to"`
		}
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &d) != nil {
			sc.t.Fatalf("a board change that does not read: %s", raw)
		}
		if state == "" || d.To == state {
			n++
		}
	}
	return n
}

// waitBoard waits until task's change to state is announced.
func (h *harness) waitBoard(sc *scenario, task, state string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for h.boardCount(sc, task, state) == 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
	return true
}
