package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
)

// listRoute is a stand-in for aicrewd's escalation list route as the store
// answers it: the open ones only with open=1, one team's with team=, newest
// first and at most waitListBound. The route's own test is the server's.
type listRoute struct {
	mu      sync.Mutex
	all     []opapi.Escalation // oldest first
	queries []string
	answers int // answers posted
}

func (lr *listRoute) answered() bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.answers > 0
}

func (lr *listRoute) raise(es ...opapi.Escalation) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.all = append(lr.all, es...)
}

func (lr *listRoute) answer(ids ...string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	for i := range lr.all {
		if slices.Contains(ids, lr.all[i].ID) {
			lr.all[i].Answer = &opapi.EscalationAnswer{Decision: "d"}
		}
	}
}

func serveListRoute(t *testing.T) *listRoute {
	t.Helper()
	lr := &listRoute{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+opapi.EscalationsPath, func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		q := r.URL.Query()
		lr.queries = append(lr.queries, q.Encode())
		out := []opapi.Escalation{}
		for i := len(lr.all) - 1; i >= 0 && len(out) < waitListBound; i-- {
			e := lr.all[i]
			if (q.Get("open") == "1" && e.Answer != nil) || (q.Get("team") != "" && e.TeamID != q.Get("team")) {
				continue
			}
			out = append(out, e)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET "+opapi.EscalationPath, func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		id := r.URL.Query().Get("id")
		for _, e := range lr.all {
			if len(id) >= 8 && strings.HasPrefix(e.ID, id) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(e)
				return
			}
		}
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("POST "+opapi.EscalationAnswerPath, func(w http.ResponseWriter, r *http.Request) {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		lr.answers++
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	tok, _ := optoken.Generate()
	tokenFile := filepath.Join(t.TempDir(), "architect.token")
	if err := optoken.Write(tokenFile, tok); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AICREW_URL", srv.URL)
	t.Setenv("AICREW_TLS_TRUST_MODE", "spki_sha256")
	t.Setenv("AICREW_TLS_TRUST_VALUE", "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", tokenFile)
	t.Setenv("AICREW_ESCALATIONS_SEEN", "")
	waitUnit = time.Millisecond
	t.Cleanup(func() { waitUnit = time.Second })
	return lr
}

// waitCmd runs aicrew escalations wait with args and returns the IDs it
// told, in order; it fails the test unless the wait returned with exit 0.
func waitCmd(t *testing.T, args ...string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	if code := run(ctx, append([]string{"escalations", "wait", "--interval", "15"}, args...), &out, &errb); code != 0 {
		t.Fatalf("wait %v: exit %d: %s", args, code, errb.String())
	}
	var ids []string
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		ids = append(ids, strings.Fields(line)[1])
	}
	return ids
}

func teamEsc(team, id string) opapi.Escalation {
	e := esc(id, "q")
	e.TeamID = team
	return e
}

// --team narrows the read to one team; the seen set survives a restart of
// the wait; two directories, each with its own seen set, are each told once.
func TestWaitOverTheRoute(t *testing.T) {
	lr := serveListRoute(t)
	lr.raise(teamEsc("team-1", "esc-one-1"), teamEsc("team-2", "esc-two-1"))
	dirA, dirB := t.TempDir(), t.TempDir()
	seenA, seenB := filepath.Join(dirA, "state", "seen.json"), filepath.Join(dirB, "state", "seen.json")

	if got := waitCmd(t, "--team", "team-2", "--seen", seenA); !slices.Equal(got, []string{"esc-two-1"}) {
		t.Fatalf("--team team-2 told %v", got)
	}
	if q := lr.queries[len(lr.queries)-1]; q != "open=1&team=team-2" {
		t.Fatalf("the read asked %q", q)
	}
	// Every team: directory A is told only what it was not told before.
	if got := waitCmd(t, "--seen", seenA); !slices.Equal(got, []string{"esc-one-1"}) {
		t.Fatalf("directory A after a restart was told %v", got)
	}
	// Directory B, through the settings' variable, is told both, once.
	t.Setenv("AICREW_ESCALATIONS_SEEN", seenB)
	if got := waitCmd(t); !slices.Equal(got, []string{"esc-one-1", "esc-two-1"}) {
		t.Fatalf("directory B was told %v", got)
	}
	// A new one is told once in each directory, and nothing else again.
	lr.raise(teamEsc("team-1", "esc-one-2"))
	if got := waitCmd(t); !slices.Equal(got, []string{"esc-one-2"}) {
		t.Fatalf("directory B then was told %v", got)
	}
	if got := waitCmd(t, "--seen", seenA); !slices.Equal(got, []string{"esc-one-2"}) {
		t.Fatalf("directory A then was told %v", got)
	}
}

// Past the bound: with 201 open escalations the route answers the newest
// 200, so the wait tells those; the oldest is told when an answer brings it
// into the list, and no seen entry is dropped on the truncated answers.
func TestWaitPastTheBound(t *testing.T) {
	lr := serveListRoute(t)
	seen := filepath.Join(t.TempDir(), "state", "seen.json")
	for i := range waitListBound + 1 {
		lr.raise(esc(fmt.Sprintf("esc-%03d", i), "q"))
	}
	got := waitCmd(t, "--seen", seen)
	if len(got) != waitListBound || got[0] != "esc-001" || got[len(got)-1] != "esc-200" || slices.Contains(got, "esc-000") {
		t.Fatalf("the first wait told %d, from %s to %s", len(got), got[0], got[len(got)-1])
	}

	// Two are answered: the answer then holds 199 rows and the oldest.
	lr.answer("esc-200", "esc-199")
	if got := waitCmd(t, "--seen", seen); !slices.Equal(got, []string{"esc-000"}) {
		t.Fatalf("after the answers the wait told %v", got)
	}
	ids, err := readSeenIDs(seen)
	if err != nil || len(ids) != waitListBound-1 || slices.Contains(ids, "esc-200") || !slices.Contains(ids, "esc-000") {
		t.Fatalf("the seen set after a complete answer: %d entries, %v", len(ids), err)
	}

	// With the list full again, a seen ID missing from it stays in the set.
	lr.raise(esc("esc-201", "q"), esc("esc-202", "q"))
	if got := waitCmd(t, "--seen", seen); !slices.Equal(got, []string{"esc-201", "esc-202"}) {
		t.Fatalf("the full list told %v", got)
	}
	if ids, _ := readSeenIDs(seen); !slices.Contains(ids, "esc-000") || len(ids) != waitListBound+1 {
		t.Fatalf("a truncated answer dropped seen entries: %d left", len(ids))
	}
}

// A wait narrowed with --team reads only that team's escalations, so it
// drops nothing from the seen set: the other teams' escalations are not
// answered, only out of its view. Every team, then one, then every team
// again tells each escalation once.
func TestWaitTeamFilterKeepsOtherTeams(t *testing.T) {
	lr := serveListRoute(t)
	seen := filepath.Join(t.TempDir(), "state", "seen.json")
	lr.raise(teamEsc("team-1", "esc-a"), teamEsc("team-2", "esc-b"))
	if got := waitCmd(t, "--seen", seen); !slices.Equal(got, []string{"esc-a", "esc-b"}) {
		t.Fatalf("every team: %v", got)
	}
	lr.raise(teamEsc("team-2", "esc-c"))
	if got := waitCmd(t, "--team", "team-2", "--seen", seen); !slices.Equal(got, []string{"esc-c"}) {
		t.Fatalf("--team team-2: %v", got)
	}
	if ids, _ := readSeenIDs(seen); !slices.Contains(ids, "esc-a") {
		t.Fatalf("the filtered wait dropped team-1's open escalation: %v", ids)
	}
	lr.raise(teamEsc("team-1", "esc-d"))
	if got := waitCmd(t, "--seen", seen); !slices.Equal(got, []string{"esc-d"}) {
		t.Fatalf("every team again: %v", got)
	}
	// An unfiltered, complete answer still drops the answered ones.
	lr.answer("esc-a", "esc-b")
	lr.raise(teamEsc("team-1", "esc-e"))
	if got := waitCmd(t, "--seen", seen); !slices.Equal(got, []string{"esc-e"}) {
		t.Fatalf("after the answers: %v", got)
	}
	if ids, _ := readSeenIDs(seen); slices.Contains(ids, "esc-a") || slices.Contains(ids, "esc-b") || len(ids) != 3 {
		t.Fatalf("seen set after the answers: %v", ids)
	}
}

// Writers that overlap each use a temporary file of their own, so the set
// stays valid JSON whatever the interleaving.
func TestSeenWritersDoNotMix(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "state", "seen.json")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := make([]string, 1+i*20)
			for j := range ids {
				ids[j] = fmt.Sprintf("w%02d-%03d", i, j)
			}
			for range 20 {
				_ = writeSeen(seen, seenFile{Owner: fmt.Sprint(i), Seen: ids})
			}
		}()
	}
	wg.Wait()
	f, err := readSeen(seen)
	if err != nil || f.Owner == "" || len(f.Seen) == 0 {
		t.Fatalf("the seen set after overlapping writes: %+v %v", f.Owner, err)
	}
	left, _ := filepath.Glob(seen + ".*.tmp")
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
