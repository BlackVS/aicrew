//go:build realaimem

package realaimem

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// testForge is the run's forge: a Gitea-dialect API on 127.0.0.1, under the
// run's CA, that knows one repository (e2e/pilot) and a token per member.
// It serves what a member's home reads: the dialect, who a token is, the
// repository's permissions for it, and its default branch's head. It holds
// no Git data: nothing is ever cloned from it.
type testForge struct {
	host string // 127.0.0.1:port
	mu   sync.Mutex
	// accounts maps a token to its account; push names the accounts that
	// may push to e2e/pilot.
	accounts map[string]string
	push     map[string]bool
}

// startForge starts the run's forge on its own leaf of the run's CA.
func (h *harness) startForge() *testForge {
	t := h.t
	t.Helper()
	certFile, keyFile, _ := h.leaf("forge")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &testForge{host: ln.Addr().String(), accounts: map[string]string{}, push: map[string]bool{}}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return f
}

// repoURL is the clone URL of the forge's one repository.
func (f *testForge) repoURL() string { return "https://" + f.host + "/e2e/pilot.git" }

// member gives an account a token and push rights, and returns the token.
func (f *testForge) member(account string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := fmt.Sprintf("forge-%s-%d", account, time.Now().UnixNano())
	f.accounts[tok], f.push[account] = account, true
	return tok
}

// setPush grants or withdraws an account's push right.
func (f *testForge) setPush(account string, push bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.push[account] = push
}

func (f *testForge) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/v1/version" {
		w.Write([]byte(`{"version":"1.22.0"}`))
		return
	}
	f.mu.Lock()
	account := f.accounts[strings.TrimPrefix(r.Header.Get("Authorization"), "token ")]
	push := f.push[account]
	f.mu.Unlock()
	if account == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var out any
	switch r.URL.Path {
	case "/api/v1/user":
		out = map[string]any{"id": 1, "login": account, "full_name": account, "email": account + "@forge.example.test"}
	case "/api/v1/repos/e2e/pilot":
		out = map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": push, "pull": true}}
	case "/api/v1/repos/e2e/pilot/branches/main":
		out = map[string]any{"commit": map[string]string{"id": processCommit}}
	default:
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(out)
}

// capabilityOf is the access aicrew holds as an agent's verified capability
// on the forge's repository, or "".
func (h *harness) capabilityOf(sc *scenario, agentID string) string {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	var report string
	if err := db.QueryRow(`SELECT report FROM agent_capabilities WHERE agent_id = ?`, agentID).Scan(&report); err != nil {
		return ""
	}
	var caps []struct {
		Repositories []struct {
			URL    string `json:"url"`
			Access string `json:"access"`
		} `json:"repositories"`
	}
	_ = json.Unmarshal([]byte(report), &caps)
	for _, c := range caps {
		for _, r := range c.Repositories {
			if r.URL == h.forge.repoURL() {
				return r.Access
			}
		}
	}
	return ""
}

// g2Capabilities: each member's launcher reported, at its session's start,
// that its home verified the project's repository with write access; an
// offer goes only to a worker whose report verified the repository at the
// offer's access (354c-2, 3.6). The forge withdraws the worker's push right,
// the worker's check reports read access, and the next offer is refused
// capability_missing before it begins; restored and checked again, the
// offer commits.
func (h *harness) g2Capabilities(t *testing.T) {
	sc := h.report.scenario(t, "G2")
	coord, worker := h.members["coord"], h.members["worker"]
	for _, name := range []string{"coord", "worker", "indep"} {
		m := h.members[name]
		sc.check(name+"'s launcher reported write access to the project's repository", h.capabilityOf(sc, m.agentID) == "write")
	}

	h.forge.setPush(worker.name, false)
	ans, _ := h.step(sc, worker, "capabilities", "", "", nil)
	sc.check("the worker's check verifies read access only", ans.OK && strings.Contains(string(ans.Result), `"insufficient"`), errorOf(ans))
	sc.check("aicrew holds the worker's read access", h.capabilityOf(sc, worker.agentID) == "read")
	task := h.createTask(sc, "G2 capability")
	ans, _ = h.step(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour)))
	sc.check("aicrewd refuses the offer capability_missing, naming the host and the access",
		refusedAtBegin(ans, "capability_missing") && strings.Contains(ans.Error.Message, h.forge.host) &&
			strings.Contains(ans.Error.Message, "write access"), errorOf(ans))
	sc.check("aicrew: nothing began", len(h.attemptsOfTask(sc, task.ID)) == 0)

	h.forge.setPush(worker.name, true)
	ans, _ = h.step(sc, worker, "capabilities", "", "", nil)
	sc.check("the worker's check verifies write access again", ans.OK && h.capabilityOf(sc, worker.agentID) == "write", errorOf(ans))
	id := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	sc.check("the offer commits", id != "")
	h.committed(sc, coord, "withdraw", id, task.ID, map[string]any{})
	sc.finish()
}
