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
