package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/BlackVS/aicrew/internal/store"
)

// The fake aimem's reservation side: `aimem reservation …` and `aimem mcp`
// (get_task) against one state file, as aimem's C6c member CLI and MCP
// facade behave. Before it commits a mutation it checks the body against the
// attempt's pending step in aicrewd's store: the operation, the begin
// response's values the member must send (intent, target state, blocker,
// result reference, reason, terminal evidence), the expected revision, the
// hold, and every task field aicrew does not own, unchanged. A body that
// differs is refused as payload_mismatch, and the reason is logged.

type fakeHoldState struct {
	ID      string `json:"id"`
	Fence   int    `json:"fence"`
	WorkRef string `json:"work_ref"`
	Active  bool   `json:"active"`
}

type fakeTaskState struct {
	Project  string                     `json:"project"`
	Revision int64                      `json:"revision"`
	Content  map[string]json.RawMessage `json:"content"`
}

type fakeReservations struct {
	Tasks    map[string]*fakeTaskState     `json:"tasks"`
	Holds    map[string]*fakeHoldState     `json:"holds"`
	Receipts map[string]store.ScopeReceipt `json:"receipts"` // by op|key
	Proofs   map[string]string             `json:"proofs"`   // p1_ digest -> op|key
	// Faults are the next answers to force, by command ("update",
	// "receipt", "mcp", …): "4" retryable, "5" lost before commit, "5c"
	// committed then lost, "2" usage, "3:code" a refusal, "bump" (mcp) the
	// task moved before this read.
	Faults map[string][]string `json:"faults"`
	Seq    int                 `json:"seq"`
}

func fakeStatePath(root string) string { return filepath.Join(root, "reservations.json") }

func loadFakeReservations(root string) *fakeReservations {
	f := &fakeReservations{}
	if raw, err := os.ReadFile(fakeStatePath(root)); err == nil {
		json.Unmarshal(raw, f)
	}
	if f.Tasks == nil {
		f.Tasks = map[string]*fakeTaskState{}
	}
	if f.Holds == nil {
		f.Holds = map[string]*fakeHoldState{}
	}
	if f.Receipts == nil {
		f.Receipts = map[string]store.ScopeReceipt{}
	}
	if f.Proofs == nil {
		f.Proofs = map[string]string{}
	}
	if f.Faults == nil {
		f.Faults = map[string][]string{}
	}
	return f
}

func (f *fakeReservations) save(root string) {
	raw, _ := json.MarshalIndent(f, "", " ")
	os.WriteFile(fakeStatePath(root), raw, 0o600)
}

func (f *fakeReservations) fault(cmd string) string {
	q := f.Faults[cmd]
	if len(q) == 0 {
		return ""
	}
	f.Faults[cmd] = q[1:]
	return q[0]
}

func digest(prefix, s string) string {
	sum := sha256.Sum256([]byte(s))
	return prefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

func envelope(code string, retryable bool) string {
	b, _ := json.Marshal(map[string]any{"code": code, "message": "fake aimem refused: " + code, "retryable": retryable,
		"next_action": "fake", "correlation_id": "corr-fake"})
	return string(b)
}

// fakeReservation plays `aimem reservation ARGS`.
func fakeReservation(root string, args []string) int {
	f := loadFakeReservations(root)
	defer f.save(root)
	if os.Getenv(SessionEnv) == "" {
		fmt.Println(envelope("context_missing", false))
		return 3
	}
	if _, err := os.Stat(os.Getenv(SessionEnv)); err != nil {
		fmt.Println(envelope("context_missing", false))
		return 3
	}
	cmd := args[0]
	if cmd == "receipt" {
		op, task, key := args[1], flagValue(args, "--task"), flagValue(args, "--key")
		record(root, fakeCall{Args: args, Event: "receipt"})
		switch f.fault("receipt") {
		case "5":
			fmt.Println(envelope("receipt_unresolved", true))
			return 5
		case "unresolved":
			fmt.Println(`{"state":"unresolved"}`)
			return 0
		}
		_ = task // aimem's receipt route is per task; the fake keys receipts by operation and key
		if r, ok := f.Receipts[op+"|"+key]; ok {
			b, _ := json.Marshal(map[string]any{"state": "committed", "receipt": r})
			fmt.Println(string(b))
			return 0
		}
		fmt.Println(`{"state":"not_committed"}`)
		return 0
	}
	task, key := flagValue(args, "--task"), flagValue(args, "--key")
	raw, _ := io.ReadAll(os.Stdin)
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		fmt.Fprintln(os.Stderr, "the body must be one JSON object on standard input")
		return 2
	}
	var proof string
	json.Unmarshal(body["coordination_proof"], &proof)
	record(root, fakeCall{Args: args, Event: "mutate", StdinPipe: proof != ""})
	if proof != "" {
		// Every proof seen, for the no-secrets scan.
		if pf, err := os.OpenFile(filepath.Join(root, "proofs.secret"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			pf.WriteString(proof + "\n")
			pf.Close()
		}
	}
	fault := f.fault(cmd)
	switch {
	case fault == "4":
		fmt.Println(envelope("context_unavailable", true))
		return 4
	case fault == "5":
		return 5
	case fault == "2":
		fmt.Fprintln(os.Stderr, "usage")
		return 2
	case strings.HasPrefix(fault, "3:"):
		fmt.Println(envelope(strings.TrimPrefix(fault, "3:"), false))
		return 3
	}
	if r, ok := f.Receipts[cmd+"|"+key]; ok {
		// A replay of a committed step answers its receipt.
		b, _ := json.Marshal(map[string]any{"replayed": true, "receipt": r})
		fmt.Println(string(b))
		return 0
	}
	t := f.Tasks[task]
	if t == nil {
		fmt.Println(envelope("task_unavailable", false))
		return 3
	}
	if why := checkPayload(root, f, cmd, task, key, body); why != "" {
		record(root, fakeCall{Args: args, Event: "mismatch: " + why})
		fmt.Println(envelope("payload_mismatch", false))
		return 3
	}
	var holder struct {
		WorkRef string `json:"work_ref"`
	}
	json.Unmarshal(body["holder"], &holder)
	h := f.Holds[task]
	switch cmd {
	case "claim":
		h = &fakeHoldState{ID: "res-" + task, Fence: 1, WorkRef: holder.WorkRef, Active: true}
		f.Holds[task] = h
	case "transfer":
		h.Fence++
		h.WorkRef = holder.WorkRef
	case "update":
		t.Content = contentOf(body)
	case "release", "finalize":
		h.Fence++
		h.Active = false
		t.Content = contentOf(body)
	}
	t.Revision++
	f.Seq++
	r := store.ScopeReceipt{ID: fmt.Sprintf("rcpt-%d", f.Seq), Operation: cmd, TaskID: task, RequestKeyDigest: digest("k1_", key),
		ReservationID: h.ID, Fence: fmt.Sprint(h.Fence), TaskRevision: t.Revision, MemberUserID: "user-1", VerifiedMode: "team",
		CommittedAt: "2026-09-29T05:00:00Z"}
	f.Receipts[cmd+"|"+key] = r
	if proof != "" {
		f.Proofs[digest("p1_", proof)] = cmd + "|" + key
	}
	if fault == "5c" {
		return 5 // committed, and the reply is lost
	}
	b, _ := json.Marshal(map[string]any{"receipt": r})
	fmt.Println(string(b))
	return 0
}

func contentOf(body map[string]json.RawMessage) map[string]json.RawMessage {
	var c map[string]json.RawMessage
	json.Unmarshal(body["content"], &c)
	return c
}

// checkPayload compares a mutation's body with the attempt's pending step in
// aicrewd's store, and with the task and hold it acts on.
func checkPayload(root string, f *fakeReservations, op, task, key string, body map[string]json.RawMessage) string {
	db, err := sql.Open("sqlite", os.Getenv("AICREW_FAKE_AICREW_DB"))
	if err != nil {
		return "no aicrew store: " + err.Error()
	}
	defer db.Close()
	var id, pendingOp, intent, detail, evidence, origin, stop, worker string
	var revision int64
	if err := db.QueryRow(`SELECT id, pending_op, pending_intent, pending_detail, pending_evidence, origin, stop, task_revision,
		worker_agent_id FROM attempts WHERE pending_key = ?`, key).Scan(&id, &pendingOp, &intent, &detail, &evidence, &origin, &stop,
		&revision, &worker); err != nil {
		return "no pending step with this key"
	}
	if pendingOp != op {
		return "operation " + op + ", pending " + pendingOp
	}
	str := func(field string) string {
		var s string
		json.Unmarshal(body[field], &s)
		return s
	}
	var expected int64
	json.Unmarshal(body["expected_revision"], &expected)
	t := f.Tasks[task]
	if expected != revision || expected != t.Revision {
		return fmt.Sprintf("expected_revision %d; step %d, task %d", expected, revision, t.Revision)
	}
	if _, has := body["coordination_proof"]; has == (op == "update") {
		return "a proof on an update, or none on a coordinated step"
	}
	if op == "claim" || op == "transfer" {
		var holder struct {
			Mode    string `json:"mode"`
			WorkRef string `json:"work_ref"`
		}
		json.Unmarshal(body["holder"], &holder)
		want := "aicrew-attempt-" + id
		if op == "claim" && origin != string(store.OriginClaim) {
			want = "aicrew-offer-" + id
		}
		if holder.Mode != "external" || holder.WorkRef != want {
			return "holder " + holder.Mode + " " + holder.WorkRef + ", want " + want
		}
		if op == "claim" {
			if _, has := body["reservation_id"]; has {
				return "a claim names no reservation"
			}
			return ""
		}
	}
	// Every operation but claim acts on the current hold, at its fence.
	h := f.Holds[task]
	if h == nil || !h.Active || str("reservation_id") != h.ID || str("fence") != fmt.Sprint(h.Fence) {
		return "reservation or fence"
	}
	if op == "transfer" {
		return ""
	}
	content := contentOf(body)
	cstr := func(field string) string {
		var s string
		json.Unmarshal(content[field], &s)
		return s
	}
	var wantState, wantBlocker string
	switch op {
	case "update":
		if str("intent") != intent {
			return "intent " + str("intent") + ", pending " + intent
		}
		wantState = map[string]string{"block": "BLOCKED", "submit": "REVIEW", "resume": "IN_PROGRESS"}[intent]
		if intent == "block" {
			wantBlocker = detail
		}
		if intent == "submit" {
			var refs []map[string]string
			json.Unmarshal(content["candidate_refs"], &refs)
			// The reference names the attempt and its member (1aad G2).
			note := "aicrew attempt " + id + " by member " + worker
			found := false
			for _, r := range refs {
				found = found || (r["ref"] == detail && r["note"] == note)
			}
			if !found {
				return "the result reference, noted " + note + ", is missing"
			}
		}
	case "release":
		if stop == string(store.StopConfirmed) {
			wantState, wantBlocker = intent, detail
			if str("reason") != "stopped" {
				return "reason " + str("reason")
			}
		} else {
			wantState = "READY"
			if str("reason") == "" {
				return "no reason"
			}
		}
	case "finalize":
		wantState = "DONE"
		var ev []store.Evidence
		json.Unmarshal([]byte(evidence), &ev)
		var want []string
		for _, e := range ev {
			want = append(want, e.Ref)
		}
		var got []string
		json.Unmarshal(body["terminal_evidence"], &got)
		if !reflect.DeepEqual(got, want) || len(want) == 0 {
			return fmt.Sprintf("terminal_evidence %v, want %v", got, want)
		}
		if str("reason") == "" {
			return "no reason"
		}
	}
	if cstr("state") != wantState || cstr("blocker") != wantBlocker {
		return fmt.Sprintf("state %q blocker %q, want %q %q", cstr("state"), cstr("blocker"), wantState, wantBlocker)
	}
	for field, v := range t.Content {
		if field == "state" || field == "blocker" || field == "candidate_refs" {
			continue
		}
		if string(content[field]) != string(v) {
			return "unowned field " + field + " changed"
		}
	}
	return ""
}

// fakeMCP plays `aimem mcp` for get_task, over stdin and stdout.
func fakeMCP(root string) int {
	f := loadFakeReservations(root)
	defer f.save(root)
	record(root, fakeCall{Args: []string{"mcp"}, Event: "mcp"})
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					ID string `json:"id"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/call":
			t := f.Tasks[req.Params.Arguments.ID]
			if req.Params.Name != "get_task" || t == nil {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "not found"}}, "isError": true}
				break
			}
			if f.fault("mcp") == "bump" {
				t.Revision++ // the task moved after the step began
			}
			doc := map[string]any{"id": req.Params.Arguments.ID, "revision": t.Revision, "project": t.Project}
			for k, v := range t.Content {
				doc[k] = v
			}
			text, _ := json.Marshal(doc)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		fmt.Println(string(b))
	}
	return 0
}

// fileReader is aimem's read scope over the fake's state file, for aicrewd.
type fileReader struct{ root string }

func (r fileReader) ReceiptByProof(_ context.Context, p1 string) (store.ScopeReceiptLookup, error) {
	f := loadFakeReservations(r.root)
	if k, ok := f.Proofs[p1]; ok {
		rc := f.Receipts[k]
		return store.ScopeReceiptLookup{State: store.ScopeCommitted, Receipt: &rc}, nil
	}
	return store.ScopeReceiptLookup{State: store.ScopeNone}, nil
}

func (r fileReader) ReceiptByKey(_ context.Context, _ store.TaskRef, op store.ReservationOp, k1 string) (store.ScopeReceiptLookup, error) {
	f := loadFakeReservations(r.root)
	for k, rc := range f.Receipts {
		if strings.HasPrefix(k, string(op)+"|") && rc.RequestKeyDigest == k1 {
			return store.ScopeReceiptLookup{State: store.ScopeCommitted, Receipt: &rc}, nil
		}
	}
	return store.ScopeReceiptLookup{State: store.ScopeNone}, nil
}

func (r fileReader) HoldStatus(_ context.Context, task store.TaskRef) (store.ScopeHold, error) {
	f := loadFakeReservations(r.root)
	h, t := f.Holds[task.TaskID], f.Tasks[task.TaskID]
	if h == nil || t == nil {
		return store.ScopeHold{State: store.ScopeNone}, nil
	}
	if !h.Active {
		return store.ScopeHold{State: store.ScopeClosed, ReservationID: h.ID, ClosingFence: fmt.Sprint(h.Fence)}, nil
	}
	return store.ScopeHold{State: store.ScopeHeld, ReservationID: h.ID, Fence: fmt.Sprint(h.Fence), HolderMode: "external",
		OwnWorkRef: h.WorkRef, TaskRevision: t.Revision}, nil
}
