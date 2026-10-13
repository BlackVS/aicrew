package forge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
)

// Each dialect opens a draft pull request, reads it, adds and removes
// labels the repository or its owner define, refuses a label neither
// defines without creating it, and comments on it, all with the member's
// token; a refusal carries the forge's own message.
func TestPulls(t *testing.T) {
	ctx := context.Background()
	for _, k := range []Kind{GitHub, Gitea, GitLab} {
		t.Run(string(k), func(t *testing.T) {
			f := fakeforge.New(t, string(k))
			c := &Client{HTTP: f.Client}
			p, err := c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo,
				NewPull{Title: "Fix the parser", Body: "Fixes empty lines.", Head: "feat/parser", Base: "main", Draft: true})
			if err != nil {
				t.Fatal(err)
			}
			wantTitle := map[Kind]string{GitHub: "Fix the parser", Gitea: "WIP: Fix the parser", GitLab: "Draft: Fix the parser"}[k]
			if p.Number != 1 || !strings.HasPrefix(p.URL, "https://"+f.Host+"/") || p.State != PullOpen || !p.Draft ||
				p.Title != wantTitle || p.Head != "feat/parser" || p.Base != "main" || p.HeadCommit != fakeforge.HeadCommit {
				t.Fatalf("opened %+v", p)
			}
			if got := f.Pull(1); got.Body != "Fixes empty lines." || got.Title != wantTitle || (k == GitHub) != got.Draft {
				t.Fatalf("the forge holds %+v", got)
			}
			// A draft prefix already in the title is not doubled.
			if _, err := c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo,
				NewPull{Title: wantTitle, Head: "feat/other", Base: "main", Draft: true}); err != nil {
				t.Fatal(err)
			}
			if got := f.Pull(2).Title; got != wantTitle {
				t.Errorf("a prefixed draft title became %q", got)
			}
			_, err = c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, NewPull{Title: "Again", Head: "feat/parser", Base: "main"})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "already exists") {
				t.Errorf("a second pull request from one head: %v", err)
			}
			if err := c.AddLabels(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1, []string{"review-this", "hands-reviewed"}); err != nil {
				t.Fatal(err)
			}
			if p, err = c.GetPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1); err != nil ||
				!slices.Equal(p.Labels, []string{"review-this", "hands-reviewed"}) || p.Title != wantTitle || !p.Draft {
				t.Fatalf("after adding labels: %+v, %v", p, err)
			}
			err = c.AddLabels(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1, []string{"review-this:other"})
			if !errors.Is(err, ErrLabelUnknown) || len(f.Created) != 0 {
				t.Errorf("an unknown label: %v, created %v", err, f.Created)
			}
			if err := c.RemoveLabels(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1, []string{"hands-reviewed", "absent"}); err != nil {
				t.Fatal(err)
			}
			if got := f.Pull(1).Labels; !slices.Equal(got, []string{"review-this"}) {
				t.Fatalf("after removing a label: %v", got)
			}
			before := len(f.Requests)
			if err := c.RemoveLabels(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1, []string{"hands-reviewed"}); err != nil {
				t.Fatalf("removing it again: %v", err)
			}
			if len(f.Requests) != before+1 {
				t.Errorf("removing a label the pull request does not carry sent %v", f.Requests[before:])
			}
			cm, err := c.CommentPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1, "Reviewed at the head.")
			if err != nil || cm.ID == 0 || !strings.HasPrefix(cm.URL, p.URL+"#") {
				t.Fatalf("comment: %+v, %v", cm, err)
			}
			if got := f.Pull(1).Comments; !slices.Equal(got, []string{"Reviewed at the head."}) {
				t.Errorf("the forge holds the comments %q", got)
			}
			if _, err := c.GetPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 99); !errors.Is(err, ErrNotFound) {
				t.Errorf("an unknown pull request: %v", err)
			}
			_, err = c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.ReadOnly, NewPull{Title: "X", Head: "a", Base: "main"})
			if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "Must have push access") {
				t.Errorf("a write the forge forbids: %v", err)
			}
			if _, err := c.GetPull(ctx, f.Host, k, "another-token", fakeforge.Repo, 1); !errors.Is(err, ErrRejected) {
				t.Errorf("a token the forge rejects: %v", err)
			}
			if f.Unauthorized != 1 {
				t.Errorf("%d requests went without the token", f.Unauthorized)
			}
		})
	}
}

// Each dialect marks a draft ready: GitHub's flag and every draft title
// prefix go, a ready pull request is not written again, and a closed one
// is refused unchanged.
func TestReadyPull(t *testing.T) {
	ctx := context.Background()
	for _, k := range []Kind{GitHub, Gitea, GitLab} {
		t.Run(string(k), func(t *testing.T) {
			f := fakeforge.New(t, string(k))
			c := &Client{HTTP: f.Client}
			if _, err := c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo,
				NewPull{Title: "WIP: [draft] Fix the parser", Head: "feat/parser", Base: "main", Draft: true}); err != nil {
				t.Fatal(err)
			}
			p, err := c.ReadyPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1)
			if err != nil || p.Draft || p.Title != "Fix the parser" || p.State != PullOpen {
				t.Fatalf("ready: %+v, %v", p, err)
			}
			if got := f.Pull(1); got.Draft || got.Title != "Fix the parser" {
				t.Fatalf("the forge holds %+v", got)
			}
			before := len(f.Requests)
			if p, err = c.ReadyPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1); err != nil || p.Draft {
				t.Fatalf("ready again: %+v, %v", p, err)
			}
			if len(f.Requests) != before+1 {
				t.Errorf("a ready pull request was written: %v", f.Requests[before:])
			}
			f.SetPull(1, func(p *fakeforge.Pull) { p.State, p.Title = "closed", "WIP: Fix the parser" })
			if _, err := c.ReadyPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1); !errors.Is(err, ErrNotOpen) ||
				f.Pull(1).Title != "WIP: Fix the parser" {
				t.Errorf("a closed pull request: %v, %+v", err, f.Pull(1))
			}
			if _, err := c.ReadyPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 99); !errors.Is(err, ErrNotFound) {
				t.Errorf("an unknown pull request: %v", err)
			}
		})
	}
}

// GitHub's GraphQL refusal of the draft flag is reported, as ErrForbidden
// when GraphQL types it FORBIDDEN, and the title is left as it was.
func TestReadyPullGraphQLRefusal(t *testing.T) {
	ctx := context.Background()
	for typ, forbidden := range map[string]bool{"FORBIDDEN": true, "UNPROCESSABLE": false} {
		f := fakeforge.New(t, "github")
		c := &Client{HTTP: f.Client}
		if _, err := c.OpenPull(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo,
			NewPull{Title: "WIP: Fix", Head: "feat/x", Base: "main", Draft: true}); err != nil {
			t.Fatal(err)
		}
		f.GraphQLError = typ
		_, err := c.ReadyPull(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo, 1)
		if err == nil || errors.Is(err, ErrForbidden) != forbidden || !strings.Contains(err.Error(), "Resource not accessible") {
			t.Errorf("%s: %v", typ, err)
		}
		if got := f.Pull(1); !got.Draft || got.Title != "WIP: Fix" {
			t.Errorf("%s: the forge holds %+v", typ, got)
		}
	}
}

// stripDraft drops every leading draft prefix in any case, and nothing else.
func TestStripDraft(t *testing.T) {
	for in, want := range map[string]struct {
		title string
		cut   bool
	}{
		"WIP: Fix":           {"Fix", true},
		"wip:Fix":            {"Fix", true},
		"[WIP] Fix":          {"Fix", true},
		"Draft: (Draft) Fix": {"Fix", true},
		"  [draft]   Fix  ":  {"Fix", true},
		"Fix: WIP: later":    {"Fix: WIP: later", false},
		"Wipe the cache":     {"Wipe the cache", false},
		"Drafting the docs":  {"Drafting the docs", false},
		"WIP:":               {"", true},
		"WIPE: the cache":    {"WIPE: the cache", false},
	} {
		if title, cut := stripDraft(in); title != want.title || cut != want.cut {
			t.Errorf("stripDraft(%q) = %q, %v; want %q, %v", in, title, cut, want.title, want.cut)
		}
	}
}

// A refusal quotes the forge's message on one bounded line, and nothing
// when the answer has none.
func TestRefusal(t *testing.T) {
	for in, want := range map[string]string{
		`{"message":"Validation Failed","errors":[{"message":"A pull request already exists."}]}`: ": Validation Failed; A pull request already exists.",
		`{"message":["Another open merge request already exists"]}`:                               ": Another open merge request already exists",
		`{"message":{"title":["can't be blank"]}}`:                                                ": title [can't be blank]",
		`{"error":"insufficient_scope"}`:                                                          ": insufficient_scope",
		`{"message":"line one\nline two"}`:                                                        ": line one line two",
		`not json`:                                                                                "",
		`{}`:                                                                                      "",
	} {
		if got := refusal(strings.NewReader(in)); got != want {
			t.Errorf("refusal(%s) = %q, want %q", in, got, want)
		}
	}
	long := refusal(strings.NewReader(`{"message":"` + strings.Repeat("x", 1000) + `"}`))
	if len(long) > maxRefusal+len(": ")+len("…") {
		t.Errorf("a long refusal is %d bytes", len(long))
	}
}
