package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// The offer route takes the client's dependency evidence (1aad G1), refuses
// it malformed or naming a dependency that is not DONE, and records it in the
// offer's audit. An offer without it is unchanged.
func TestOfferRecordsDependencyEvidence(t *testing.T) {
	e := setupCoordination(t)
	expires := time.Now().Add(time.Hour)
	withEvidence := func(ev any) map[string]any {
		b := e.offerBody("task-1", expires)
		b["dependency_evidence"] = ev
		return b
	}
	tooMany := make([]map[string]any, 65)
	for i := range tooMany {
		tooMany[i] = map[string]any{"task_id": fmt.Sprintf("dep-%d", i), "state": "DONE", "revision": 1}
	}
	for name, ev := range map[string]any{
		"not DONE":      []map[string]any{{"task_id": "dep-a", "state": "IN_PROGRESS", "revision": 2}},
		"no task":       []map[string]any{{"task_id": "", "state": "DONE", "revision": 2}},
		"no revision":   []map[string]any{{"task_id": "dep-a", "state": "DONE", "revision": 0}},
		"a duplicate":   []map[string]any{{"task_id": "dep-a", "state": "DONE", "revision": 2}, {"task_id": "dep-a", "state": "DONE", "revision": 2}},
		"too many":      tooMany,
		"not a list":    "dep-a",
		"unknown field": []map[string]any{{"task_id": "dep-a", "state": "DONE", "revision": 2, "note": "x"}},
	} {
		// Each body differs from the accepted offer below only in its evidence.
		refused(t, e.call(t, e.lead.token, AttemptsPath, "offer-"+strings.ReplaceAll(name, " ", "-"), withEvidence(ev)),
			http.StatusBadRequest, "invalid_request")
	}

	want := []store.DependencyEvidence{{TaskID: "dep-a", State: "DONE", Revision: 4}, {TaskID: "dep-b", State: "DONE", Revision: 9}}
	stepOf(t, e.call(t, e.lead.token, AttemptsPath, "offer-1", withEvidence(want)))
	if got := offerEvidence(t, e); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("the offer's audit evidence: %+v", got)
	}

	// An offer without the field, as the one-shot path sends, is unchanged.
	e2 := setupCoordination(t)
	stepOf(t, e2.call(t, e2.lead.token, AttemptsPath, "offer-1", e2.offerBody("task-1", expires)))
	if got := offerEvidence(t, e2); len(got) != 1 || got[0] != nil {
		t.Fatalf("an offer without evidence: %+v", got)
	}
}

// offerEvidence is the dependency evidence of each offer in e's audit.
func offerEvidence(t *testing.T, e *coordEnv) [][]store.DependencyEvidence {
	t.Helper()
	db, err := sql.Open("sqlite", e.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT input FROM audit WHERE operation = 'attempt.offer' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][]store.DependencyEvidence
	for rows.Next() {
		var input string
		var in store.OfferRequest
		if rows.Scan(&input) != nil || json.Unmarshal([]byte(input), &in) != nil {
			t.Fatalf("audit input %q", input)
		}
		got = append(got, in.Dependencies)
	}
	return got
}
