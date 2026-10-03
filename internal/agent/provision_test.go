package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/store"
)

const testMemberToken = "aimem_user_0123456789abcdef"

const testCA = "-----BEGIN CERTIFICATE-----\nMIIBtest\n-----END CERTIFICATE-----\n"

// provisionCall is one call the stand-in aimem recorded.
type provisionCall struct {
	Args   []string `json:"args"`
	State  string   `json:"state"`
	Socket string   `json:"socket"`
	Stdin  string   `json:"stdin"`
}

func provisionCalls(t *testing.T, home string) []provisionCall {
	t.Helper()
	f, err := os.Open(filepath.Join(AimemDir(home), "calls.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []provisionCall
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c provisionCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// privateFile writes content to a new owner-only file.
func privateFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := privatefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
}

// provisionDeps runs the real exec aimem (this test binary in its
// provisioning mode) for the join.
func (e *joinEnv) provisionDeps(t *testing.T, crew *recCrew, reads *int, codes ...string) JoinDeps {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(joinFakeEnv, "provision")
	deps := e.deps(crew, nil, reads, codes...)
	prover := activeAimem()
	deps.Aimem = func(_, hub, home string) JoinAimem {
		return execProvisioning{JoinAimem: ExecAimem{Command: self, Hub: hub, Home: home}.JoinAimem(), prover: prover}
	}
	return deps
}

// execProvisioning is the real exec aimem for the credential and the
// provisioning, and the in-process stand-in for the identity proof, whose
// receipts the test verifier vouches for.
type execProvisioning struct {
	JoinAimem
	prover *fakeJoinAimem
}

func (p execProvisioning) Proof(ctx context.Context, serviceID, hubID, challengeID string) (string, error) {
	return p.prover.Proof(ctx, serviceID, hubID, challengeID)
}

// A first join with the provisioning flags gives the home's installation
// the hub and the member's credential through aimem itself: the token only
// on aimem's standard input, the CA copied owner-only into the home's creds/,
// every call against the home's installation. The join then goes on as
// usual, and the token appears nowhere else.
func TestJoinProvisionsTheHome(t *testing.T) {
	for _, viaPrompt := range []bool{false, true} {
		name := "token file"
		if viaPrompt {
			name = "prompt"
		}
		t.Run(name, func(t *testing.T) {
			e := setupJoin(t)
			dir := t.TempDir()
			ca := filepath.Join(dir, "hub-ca.pem")
			os.WriteFile(ca, []byte(testCA), 0o644)
			o := e.opts()
			o.AimemURL, o.AimemCAFile = "https://hub.example:8443/", ca
			reads := 0
			deps := e.provisionDeps(t, &recCrew{}, &reads, e.invite(t, "inv-1", store.RoleWorker))
			if viaPrompt {
				o.AimemTokenFile = "-"
				deps.ReadToken = func() (string, error) { return testMemberToken + "\n", nil }
			} else {
				o.AimemTokenFile = filepath.Join(dir, "member.token")
				privateFile(t, o.AimemTokenFile, testMemberToken+"\n")
			}
			rep, err := Join(context.Background(), o, deps)
			if err != nil || rep.Status == JoinBlocked {
				t.Fatalf("%+v, %v\n%s", rep, err, e.out.String())
			}
			copied := filepath.Join(e.home, "creds", "aimem.main.ca.pem")
			if b, err := os.ReadFile(copied); err != nil || string(b) != testCA {
				t.Fatalf("CA copy: %q, %v", b, err)
			}
			if err := privatefile.Check(copied); err != nil {
				t.Fatalf("the CA copy is not owner-only: %v", err)
			}
			calls := provisionCalls(t, e.home)
			var add, task bool
			for _, c := range calls {
				if c.State != AimemDir(e.home) || c.Socket != AimemSocket(e.home) {
					t.Fatalf("a call ran against another installation: %+v", c)
				}
				if strings.Contains(strings.Join(c.Args, " "), testMemberToken) {
					t.Fatalf("the token is in aimem's arguments: %q", c.Args)
				}
				switch strings.Join(c.Args[:2], " ") {
				case "hub add":
					add = true
					want := []string{"hub", "add", "main", "https://hub.example:8443", "--token-file", "-", "--ca-file", copied}
					if strings.Join(c.Args, " ") != strings.Join(want, " ") || strings.TrimSpace(c.Stdin) != testMemberToken {
						t.Fatalf("hub add: %+v", c)
					}
				case "hub task-token":
					task = true
					if strings.Join(c.Args, " ") != "hub task-token main --token-file -" || strings.TrimSpace(c.Stdin) != testMemberToken {
						t.Fatalf("task-token: %+v", c)
					}
				}
			}
			if !add || !task {
				t.Fatalf("calls %+v", calls)
			}
			// The token appears in no report, output or home file but the
			// installation's own state, which aimem owns.
			raw, _ := json.Marshal(rep)
			if strings.Contains(string(raw)+e.out.String(), testMemberToken) {
				t.Fatal("the token is in the report or the output")
			}
			filepath.WalkDir(e.home, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || strings.HasPrefix(p, AimemDir(e.home)) {
					return nil
				}
				if b, _ := os.ReadFile(p); strings.Contains(string(b), testMemberToken) {
					t.Fatalf("%s holds the token", p)
				}
				return nil
			})
		})
	}
}

// A home whose credential is active is not provisioned again; a rerun after
// a failed step completes it; a failure stops the run with aimem's message.
func TestJoinProvisioningReruns(t *testing.T) {
	e := setupJoin(t)
	dir := t.TempDir()
	o := e.opts()
	o.AimemURL = "https://hub.example"
	o.AimemTokenFile = filepath.Join(dir, "member.token")
	privateFile(t, o.AimemTokenFile, testMemberToken)

	// task-token fails: the run stops, before any invitation attempt.
	t.Setenv("AICREW_JOIN_FAKE_FAIL", "task-token")
	reads := 0
	rep, err := Join(context.Background(), o, e.provisionDeps(t, &recCrew{}, &reads, e.invite(t, "inv-1", store.RoleWorker)))
	if err != nil || rep.Status != JoinBlocked || rep.Reason != "aimem_provision_failed" || reads != 0 ||
		!strings.Contains(rep.Instruction, "aimem refused it") || strings.Contains(rep.Instruction, testMemberToken) {
		t.Fatalf("%+v, %v", rep, err)
	}
	// The rerun repeats both steps and goes on to the join.
	t.Setenv("AICREW_JOIN_FAKE_FAIL", "")
	reads = 0
	rep, err = Join(context.Background(), o, e.provisionDeps(t, &recCrew{}, &reads, e.invite(t, "inv-2", store.RoleWorker)))
	if err != nil || rep.Status == JoinBlocked {
		t.Fatalf("rerun: %+v, %v", rep, err)
	}
	before := len(provisionCalls(t, e.home))

	// A new unlinked home whose installation already holds an active
	// credential: the flags provision nothing.
	e2 := setupJoin(t)
	o2 := e2.opts()
	o2.AimemURL, o2.AimemTokenFile = o.AimemURL, o.AimemTokenFile
	am := activeAimem()
	rep, _ = e2.join(t, o2, &recCrew{}, am, e2.invite(t, "inv-3", store.RoleWorker))
	if rep.Status == JoinBlocked || am.provisions != 0 || !strings.Contains(e2.out.String(), "not provisioning it again") {
		t.Fatalf("an active credential was provisioned again: %+v, %d", rep, am.provisions)
	}
	if after := len(provisionCalls(t, e.home)); after != before {
		t.Fatal("the first home was touched")
	}
}

// The provisioning flags are checked before anything runs: a token is never
// an argument, the flags go together and only on a new home, the token
// file must be owner-only and hold a user-scoped token.
func TestJoinProvisioningRefusals(t *testing.T) {
	e := setupJoin(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "member.token")
	privateFile(t, good, testMemberToken)
	checkpoint := filepath.Join(dir, "checkpoint.token")
	privateFile(t, checkpoint, "aimem_admin_secret")
	for _, c := range []struct {
		name, url, token, ca string
		usage                bool
		reason               string
	}{
		{"token as the flag value", "https://hub.example", testMemberToken, "", true, ""},
		{"url without token", "https://hub.example", "", "", true, ""},
		{"token without url", "", good, "", true, ""},
		{"ca alone", "", "", good, true, ""},
		{"plain http", "http://hub.example", good, "", true, ""},
		{"url with a path", "https://hub.example/api", good, "", true, ""},
		{"not a user token", "https://hub.example", checkpoint, "", false, "aimem_token"},
		{"missing token file", "https://hub.example", filepath.Join(dir, "none"), "", false, "aimem_token"},
		{"not a CA", "https://hub.example", good, good, false, "aimem_provision_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := e.opts()
			o.AimemURL, o.AimemTokenFile, o.AimemCAFile = c.url, c.token, c.ca
			reads := 0
			rep, err := Join(context.Background(), o, e.provisionDeps(t, &recCrew{}, &reads))
			if c.usage {
				if !errors.Is(err, ErrJoinUsage) {
					t.Fatalf("%+v, %v", rep, err)
				}
			} else if err != nil || rep.Reason != c.reason {
				t.Fatalf("%+v, %v", rep, err)
			}
			if strings.Contains(rep.Instruction, testMemberToken) || strings.Contains(rep.Instruction, "aimem_admin_secret") {
				t.Fatal("a refusal quotes the token")
			}
			if calls := provisionCalls(t, e.home); len(calls) != 0 {
				t.Fatalf("aimem ran: %+v", calls)
			}
		})
	}
	// A linked home refuses the flags: provisioning is for a new home.
	o := e.opts()
	reads := 0
	if rep, err := Join(context.Background(), o, e.deps(&recCrew{}, activeAimem(), &reads, e.invite(t, "inv-l", store.RoleWorker))); err != nil || rep.Status == JoinBlocked {
		t.Fatalf("link: %+v, %v", rep, err)
	}
	o.AimemURL, o.AimemTokenFile = "https://hub.example", good
	if _, err := Join(context.Background(), o, e.deps(&recCrew{}, activeAimem(), &reads)); !errors.Is(err, ErrJoinUsage) {
		t.Fatalf("a linked home accepted the provisioning flags: %v", err)
	}
}
