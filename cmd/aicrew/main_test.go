package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

var bearerShape = regexp.MustCompile(`^aicrew_introspect_[0-9a-f]{64}\n$`)

type result struct {
	code           int
	stdout, stderr string
}

func cli(t *testing.T, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

func authenticate(t *testing.T, storePath, bearer string) (string, error) {
	t.Helper()
	st, err := store.Open(context.Background(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return st.AuthenticateIntrospection(context.Background(), strings.TrimSpace(bearer))
}

// Issue writes the bearer only to its new private file and prints metadata;
// the credential then authenticates for its hub.
func TestIssueListRotateRevoke(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "aicrew.db")
	first := filepath.Join(dir, "first.secret")

	// Rotate needs one active credential to replace.
	none := filepath.Join(dir, "none.secret")
	if r := cli(t, "introspection-credential", "rotate", "-store", storePath, "-hub", "hub-a", "-secret-file", none); r.code != 1 {
		t.Fatalf("rotate with no active credential: %d", r.code)
	}
	if _, err := os.Stat(none); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused rotate created its secret file")
	}

	r := cli(t, "introspection-credential", "issue", "-store", storePath, "-hub", "hub-a", "-secret-file", first)
	if r.code != 0 {
		t.Fatalf("issue: %d %s", r.code, r.stderr)
	}
	bearer, err := os.ReadFile(first)
	if err != nil || !bearerShape.Match(bearer) {
		t.Fatalf("secret file = %q, %v", bearer, err)
	}
	if strings.Contains(r.stdout, strings.TrimSpace(string(bearer))) || strings.Contains(r.stderr, strings.TrimSpace(string(bearer))) {
		t.Fatal("the bearer was printed")
	}
	var issued credentialView
	if err := json.Unmarshal([]byte(r.stdout), &issued); err != nil || issued.HubID != "hub-a" || !issued.Active || issued.SecretFile != first {
		t.Fatalf("issue printed %s (%v)", r.stdout, err)
	}
	if hub, err := authenticate(t, storePath, string(bearer)); err != nil || hub != "hub-a" {
		t.Fatalf("authenticate = %q, %v", hub, err)
	}

	// An existing secret file is never reused, and nothing is issued.
	if r := cli(t, "introspection-credential", "issue", "-store", storePath, "-hub", "hub-a", "-secret-file", first); r.code != 1 {
		t.Fatalf("issue into an existing file: %d", r.code)
	}
	if again, _ := os.ReadFile(first); !bytes.Equal(again, bearer) {
		t.Fatal("the existing secret file was changed")
	}

	second := filepath.Join(dir, "second.secret")
	r = cli(t, "introspection-credential", "rotate", "-store", storePath, "-hub", "hub-a", "-secret-file", second)
	var rotated credentialView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &rotated) != nil || rotated.Replaces != issued.ID {
		t.Fatalf("rotate: %d %s %s", r.code, r.stdout, r.stderr)
	}
	third := filepath.Join(dir, "third.secret")
	if r := cli(t, "introspection-credential", "rotate", "-store", storePath, "-hub", "hub-a", "-secret-file", third); r.code != 1 {
		t.Fatalf("rotate with two active credentials: %d", r.code)
	}
	if r := cli(t, "introspection-credential", "issue", "-store", storePath, "-hub", "hub-a", "-secret-file", third); r.code != 1 || !strings.Contains(r.stderr, "credential_limit") {
		t.Fatalf("a third active credential: %d %s", r.code, r.stderr)
	}
	if _, err := os.Stat(third); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused issue left its secret file behind")
	}

	r = cli(t, "introspection-credential", "revoke", "-store", storePath, "-id", issued.ID)
	var revoked credentialView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &revoked) != nil || revoked.Active || revoked.RevokedAt.IsZero() {
		t.Fatalf("revoke: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if _, err := authenticate(t, storePath, string(bearer)); !errors.Is(err, store.ErrUnauthenticated) {
		t.Fatalf("a revoked credential authenticated: %v", err)
	}
	r = cli(t, "introspection-credential", "list", "-store", storePath, "-hub", "hub-a")
	var listed []credentialView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &listed) != nil || len(listed) != 2 {
		t.Fatalf("list: %d %s", r.code, r.stdout)
	}
	for _, c := range listed {
		if (c.ID == issued.ID) == c.Active {
			t.Fatalf("list shows %+v", c)
		}
	}
	if r := cli(t, "introspection-credential", "revoke", "-store", storePath, "-id", "no-such-id"); r.code != 1 {
		t.Fatalf("revoke an unknown credential: %d", r.code)
	}
}

// If the bearer cannot be written, the credential just issued is revoked and
// no secret file is left.
func TestIssueRevokesWhenTheSecretCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "aicrew.db")
	secret := filepath.Join(dir, "x.secret")
	orig := writeSecret
	writeSecret = func(*os.File, string) error { return errors.New("disk full") }
	defer func() { writeSecret = orig }()
	r := cli(t, "introspection-credential", "issue", "-store", storePath, "-hub", "hub-a", "-secret-file", secret)
	if r.code != 1 || !strings.Contains(r.stderr, "was revoked") {
		t.Fatalf("issue with a failed write: %d %s", r.code, r.stderr)
	}
	if _, err := os.Stat(secret); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the secret file was left behind")
	}
	writeSecret = orig
	r = cli(t, "introspection-credential", "list", "-store", storePath)
	var listed []credentialView
	if json.Unmarshal([]byte(r.stdout), &listed) != nil || len(listed) != 1 || listed[0].Active {
		t.Fatalf("after a failed write the credential should be revoked: %s", r.stdout)
	}
}

// Usage errors exit 2; a store held by aicrewd exits 1 and creates nothing.
func TestUsageAndStoreInUse(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "aicrew.db")
	for _, args := range [][]string{
		nil,
		{"introspection-credential"},
		{"other", "issue"},
		{"introspection-credential", "burn", "-store", storePath},
		{"introspection-credential", "issue", "-store", storePath, "-hub", "hub-a"},
		{"introspection-credential", "issue", "-hub", "hub-a", "-secret-file", "x"},
		{"introspection-credential", "revoke", "-store", storePath},
		{"introspection-credential", "list", "-store", storePath, "-id", "x"},
		{"introspection-credential", "issue", "-store", storePath, "-hub", "hub-a", "-secret-file", "x", "-operations", "everything"},
		{"introspection-credential", "issue", "-store", storePath, "-hub", "hub-a", "-secret-file", "x", "-operations", "introspection,"},
		{"introspection-credential", "list", "-store", storePath, "-operations", "coordination"},
	} {
		if r := cli(t, args...); r.code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, r.code)
		}
	}
	held, err := store.Open(context.Background(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	secret := filepath.Join(dir, "held.secret")
	r := cli(t, "introspection-credential", "issue", "-store", storePath, "-hub", "hub-a", "-secret-file", secret)
	if r.code != 1 || !strings.Contains(r.stderr, "stop aicrewd") {
		t.Fatalf("store in use: %d %s", r.code, r.stderr)
	}
	if _, err := os.Stat(secret); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a secret file was created while the store was in use")
	}
}

// -operations sets what a new credential permits: both by default, or only
// the named ones. The listing shows them.
func TestIssueOperations(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "aicrew.db")
	for _, tc := range []struct {
		flag              string
		want              string
		introspect, facts bool
	}{
		{"", store.OpCoordination + "," + store.OpIntrospection, true, true},
		{"introspection", store.OpIntrospection, true, false},
		{"coordination", store.OpCoordination, false, true},
		{"coordination, introspection", store.OpCoordination + "," + store.OpIntrospection, true, true},
	} {
		secret := filepath.Join(dir, "cred-"+strings.NewReplacer(",", "-", " ", "").Replace(tc.flag)+".secret")
		hub := "hub-" + strings.NewReplacer(",", "-", " ", "").Replace(tc.flag)
		args := []string{"introspection-credential", "issue", "-store", storePath, "-hub", hub, "-secret-file", secret}
		if tc.flag != "" {
			args = append(args, "-operations", tc.flag)
		}
		r := cli(t, args...)
		var v credentialView
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil || strings.Join(v.Operations, ",") != tc.want {
			t.Fatalf("-operations %q: %d %s %s", tc.flag, r.code, r.stdout, r.stderr)
		}
		bearer, err := os.ReadFile(secret)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(context.Background(), storePath)
		if err != nil {
			t.Fatal(err)
		}
		_, ierr := st.AuthenticateIntrospection(context.Background(), strings.TrimSpace(string(bearer)))
		_, cerr := st.AuthenticateCoordination(context.Background(), strings.TrimSpace(string(bearer)))
		st.Close()
		if (ierr == nil) != tc.introspect || (cerr == nil) != tc.facts {
			t.Fatalf("-operations %q: introspection %v, coordination %v", tc.flag, ierr, cerr)
		}
	}
}
