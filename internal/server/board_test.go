package server

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/store"
)

// boardHub is a granting hub with a board feed: its cursor is the number of
// changes read so far, so reading on never skips or repeats one, as
// aimem's cursor. refuse, when set, answers every read with that code.
type boardHub struct {
	*grantingHub
	mu      sync.Mutex
	changes []hubteams.BoardChange
	refuse  string
	retry   time.Duration
	reads   int
}

func (h *boardHub) CanReadBoard() bool { return true }

func (h *boardHub) ReadBoard(_ context.Context, cursor string, limit int) (hubteams.BoardPage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	if h.refuse != "" {
		return hubteams.BoardPage{}, &hubteams.Error{Code: h.refuse, Retryable: h.refuse == "rate_limited", RetryAfter: h.retry}
	}
	from := 0
	if cursor != "" {
		from, _ = strconv.Atoi(cursor)
	}
	if from > len(h.changes) {
		return hubteams.BoardPage{}, &hubteams.Error{Code: "cursor_ahead"}
	}
	to := min(from+limit, len(h.changes))
	return hubteams.BoardPage{Changes: append([]hubteams.BoardChange{}, h.changes[from:to]...),
		Cursor: strconv.Itoa(to), More: to < len(h.changes)}, nil
}

func (h *boardHub) add(changes ...hubteams.BoardChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changes = append(h.changes, changes...)
}

func (h *boardHub) set(refuse string, retry time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuse, h.retry = refuse, retry
}

func (h *boardHub) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads
}

// boardSetup is the coordination environment, and a service on its store
// whose hub has a board feed. The running service's own hubs are left as
// they are: it reads them concurrently.
func boardSetup(t *testing.T) (*coordEnv, *Server, *boardHub) {
	t.Helper()
	e := setupCoordination(t)
	hub := &boardHub{grantingHub: e.hub}
	srv := &Server{store: e.store, log: e.srv.log, hubs: map[string]hubBinding{coordHubAlias: {id: coordHub, teams: hub}}}
	return e, srv, hub
}

// leadBoard are the board.changed announcements in the coordinator's
// inbox.
func leadBoard(t *testing.T, e *coordEnv) []store.InboxItem {
	t.Helper()
	items, err := e.store.ReadInboxWithToken(context.Background(), e.lead.token, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.InboxItem
	for _, it := range items {
		if it.Board != nil {
			out = append(out, it)
		}
	}
	return out
}

func toReady(task string, rev int64, at time.Time) hubteams.BoardChange {
	return hubteams.BoardChange{Project: "project-example", TaskID: task, Revision: rev, From: "BACKLOG", To: "READY", At: at}
}

// The first tick reads the board's history and announces none of it; a
// task that becomes READY afterwards wakes the coordinator on the next
// tick, once. A service restarted with the same store reads on from the
// stored cursor and announces nothing again (criteria 1 and 2).
func TestBoardWakesTheCoordinatorOnce(t *testing.T) {
	ctx := context.Background()
	e, srv, hub := boardSetup(t)
	past := time.Now().Add(-time.Hour)
	hub.add(toReady("old-1", 2, past), toReady("old-2", 3, past))
	srv.readBoards(ctx)
	if got := leadBoard(t, e); len(got) != 0 {
		t.Fatalf("the board's history announced: %+v", got)
	}
	hub.add(toReady("task-new", 4, time.Now().Add(time.Second)))
	srv.readBoards(ctx)
	got := leadBoard(t, e)
	if len(got) != 1 || got[0].Task.TaskID != "task-new" || got[0].Board.To != "READY" || got[0].Kind != store.KindLifecycle {
		t.Fatalf("announcements: %+v", got)
	}

	// A restart: a new service on the same store, cursor and all.
	again := &Server{store: e.store, log: e.srv.log, hubs: srv.hubs}
	again.readBoards(ctx)
	srv.readBoards(ctx)
	if got := leadBoard(t, e); len(got) != 1 {
		t.Fatalf("after a restart: %d announcements", len(got))
	}
}

// A long backlog is read over several ticks, boardPagesPerTick pages each.
func TestBoardPagesPerTick(t *testing.T) {
	ctx := context.Background()
	_, srv, hub := boardSetup(t)
	for i := range hubteams.MaxBoardPage*boardPagesPerTick + 1 {
		hub.add(toReady("t"+strconv.Itoa(i), 1, time.Now().Add(-time.Hour)))
	}
	srv.readBoards(ctx)
	if n := hub.readCount(); n != boardPagesPerTick {
		t.Fatalf("one tick read %d pages, want %d", n, boardPagesPerTick)
	}
	srv.readBoards(ctx)
	if n := hub.readCount(); n != boardPagesPerTick+1 {
		t.Fatalf("two ticks read %d pages", n)
	}
}

// A cursor the hub no longer knows is dropped and the feed read again from
// its start, announcing nothing it announced before; a hub that asks to wait
// is not read again before the time it named.
func TestBoardRefusals(t *testing.T) {
	ctx := context.Background()
	e, srv, hub := boardSetup(t)
	hub.add(toReady("old", 1, time.Now().Add(-time.Hour)))
	srv.readBoards(ctx)
	hub.add(toReady("task-a", 2, time.Now().Add(time.Second)))
	srv.readBoards(ctx)
	if got := leadBoard(t, e); len(got) != 1 {
		t.Fatalf("before the restore: %d", len(got))
	}
	// The hub's state is restored to before task-a: the stored cursor is
	// past its feed.
	hub.mu.Lock()
	hub.changes = hub.changes[:1]
	hub.mu.Unlock()
	srv.readBoards(ctx)
	if cur, _ := e.store.BoardCursor(ctx, store.ReconcilerCaller(), coordHub); cur != "" {
		t.Fatalf("the cursor after cursor_ahead: %q", cur)
	}
	hub.add(toReady("task-a", 2, time.Now().Add(-time.Second)))
	srv.readBoards(ctx)
	if got := leadBoard(t, e); len(got) != 1 {
		t.Fatalf("after the restore: %d announcements", len(got))
	}

	hub.set("rate_limited", time.Hour)
	before := hub.readCount()
	srv.readBoards(ctx)
	srv.readBoards(ctx)
	if n := hub.readCount() - before; n != 1 {
		t.Fatalf("read %d times after rate_limited", n)
	}
}
