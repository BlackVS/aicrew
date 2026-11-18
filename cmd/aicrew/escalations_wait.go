package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/architect"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/opapi"
)

// aicrew escalations wait: the architect session is told of an escalation
// without a command run by hand (docs/DESIGN-CONTROL-PLANE.md, section
// 7.7, D21 to D24). It reads the open escalations on the existing list
// route every interval and returns with those this directory has not been
// told of. The seen set is a local file: answered escalations leave it on
// each complete read of every team, and the only state that ends an
// escalation is aicrewd's recorded answer.

const (
	// waitDefaultInterval and waitMinInterval are D21's 60 and 15 seconds,
	// in units of waitUnit.
	waitDefaultInterval = 60
	waitMinInterval     = 15
	// waitMaxFailures is how many failed reads in a row end the wait.
	waitMaxFailures = 5
	// waitListBound is the list route's bound (ListEscalations, newest
	// first): a list this long may leave older open escalations out.
	waitListBound = 200
)

// waitUnit is the interval's unit; tests shorten it.
var waitUnit = time.Second

// waitSuperseded is the exit code of a wait whose seen set a newer wait in
// the same directory took over.
const waitSuperseded = 3

// seenFile is the seen set as written: the IDs this directory was told of
// and that were still open at the last read, and the wait that owns the
// set. Only the directory's newest wait records anything: a wait left
// running by an ended session finds another owner on its next read and
// ends, so it never takes an escalation from the session that is there.
type seenFile struct {
	Owner string   `json:"owner,omitempty"`
	Seen  []string `json:"seen"`
}

// runEscalationsWait is aicrew escalations wait: 0 when it returns with new
// escalations, 1 when the reads failed waitMaxFailures times in a row or
// the seen set cannot be kept, 2 on a usage error, 3 when a newer wait in
// the same directory took the seen set over.
func runEscalationsWait(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrew escalations wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	team := fs.String("team", "", "the team's ID; every team the credential reads without it")
	interval := fs.Int("interval", waitDefaultInterval, "seconds between two reads, at least 15")
	seenPath := fs.String("seen", "", "the seen set's file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *interval < waitMinInterval {
		fmt.Fprint(stderr, escalationsUsage)
		return 2
	}
	if *seenPath = orEnv(seenPath, "AICREW_ESCALATIONS_SEEN"); *seenPath == "" {
		*seenPath = filepath.FromSlash(architect.SeenFile)
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	q := url.Values{"open": {"1"}}
	if *team != "" {
		q.Set("team", *team)
	}
	list := func(ctx context.Context) ([]opapi.Escalation, error) {
		var open []opapi.Escalation
		err := cl.Get(ctx, opapi.EscalationsPath, q, &open)
		return open, err
	}
	return waitEscalations(ctx, list, *seenPath, *team == "", time.Duration(*interval)*waitUnit, stdout, stderr)
}

// waitEscalations takes the seen set over, then reads the open escalations
// until one is outside the set, records the new ones and prints them,
// oldest first. It ends without recording anything once a newer wait owns
// the set. everyTeam says the list is not narrowed to one team: only such a
// list, when complete, shows which seen escalations were answered.
func waitEscalations(ctx context.Context, list func(context.Context) ([]opapi.Escalation, error),
	seenPath string, everyTeam bool, every time.Duration, stdout, stderr io.Writer) int {
	var mark [16]byte
	if _, err := rand.Read(mark[:]); err != nil {
		return failed(stderr, err)
	}
	owner := hex.EncodeToString(mark[:])
	claim, err := readSeen(seenPath)
	if err != nil {
		return failed(stderr, err)
	}
	claim.Owner = owner
	if err := writeSeen(seenPath, claim); err != nil {
		return failed(stderr, err)
	}
	failures := 0
	for {
		open, err := list(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return failed(stderr, ctx.Err())
			}
			failures++
			if failures >= waitMaxFailures {
				return failed(stderr, fmt.Errorf("the open escalations could not be read %d times in a row: %w", failures, err))
			}
		} else {
			failures = 0
			cur, err := readSeen(seenPath)
			if err != nil {
				return failed(stderr, err)
			}
			if cur.Owner != owner {
				fmt.Fprintln(stderr, "aicrew: a newer escalations wait took this directory's seen set; this one ends and records nothing")
				return waitSuperseded
			}
			kept, fresh := splitSeen(open, cur.Seen, everyTeam && len(open) < waitListBound)
			if len(fresh) > 0 || len(kept) != len(cur.Seen) {
				next := kept
				for _, e := range fresh {
					next = append(next, e.ID)
				}
				if err := writeSeen(seenPath, seenFile{Owner: owner, Seen: next}); err != nil {
					return failed(stderr, err)
				}
			}
			if len(fresh) > 0 {
				for _, e := range fresh {
					fmt.Fprintln(stdout, escalationLine(e))
				}
				return 0
			}
		}
		select {
		case <-ctx.Done():
			return failed(stderr, ctx.Err())
		case <-time.After(every):
		}
	}
}

// splitSeen returns the seen IDs to keep, and the open escalations not seen
// yet, oldest first (the route answers newest first). A seen ID absent from
// the list is dropped only when the list is complete: a list cut at the
// route's bound leaves out older open escalations, so absence there does
// not mean answered.
func splitSeen(open []opapi.Escalation, seen []string, complete bool) (kept []string, fresh []opapi.Escalation) {
	isOpen := map[string]bool{}
	for _, e := range open {
		isOpen[e.ID] = true
	}
	kept = []string{}
	for _, id := range seen {
		if isOpen[id] || !complete {
			kept = append(kept, id)
		}
	}
	for i := len(open) - 1; i >= 0; i-- {
		if !slices.Contains(kept, open[i].ID) {
			fresh = append(fresh, open[i])
		}
	}
	return kept, fresh
}

// escalationLine is one escalation on one line, its text without terminal
// escapes or line breaks: the coordinator wrote it.
func escalationLine(e opapi.Escalation) string {
	id := e.ID
	if len(id) > 13 {
		id = id[:13]
	}
	line := fmt.Sprintf("escalation %s team %s task %s/%s %s %s: %s",
		id, e.TeamID, e.Task.ProjectID, e.Task.TaskID, e.Category, e.Urgency, e.Question)
	return strings.Join(strings.Fields(forge.CleanText(line)), " ")
}

func readSeen(path string) (seenFile, error) {
	f := seenFile{Seen: []string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("the seen set %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("the seen set %s is not readable (remove it to be told of every open escalation again): %w", path, err)
	}
	if f.Seen == nil {
		f.Seen = []string{}
	}
	return f, nil
}

// writeSeen replaces the seen set whole, owner-only, through a temporary
// file of its own, so two waits writing at once never mix their contents.
func writeSeen(path string, f seenFile) error {
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("the seen set %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("the seen set %s: %w", path, err)
	}
	_, werr := tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("the seen set %s: %w", path, werr)
	}
	return nil
}
