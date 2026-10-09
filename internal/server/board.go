package server

import (
	"context"
	"errors"
	"time"

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/reconcile"
	"github.com/BlackVS/aicrew/internal/store"
)

// The board wake (docs/DESIGN-CONTROL-PLANE.md, A1; docs/CREW-CONTRACT.md,
// "Board changes"): every BoardTick, aicrewd reads each hub's board feed
// with its board.read credential, from the cursor it stored, and records
// each page with the board.changed announcements it leads to, in one
// transaction (store.RecordBoardPage).

// BoardTick is how often the board feed is read: the reconcile tick.
const BoardTick = reconcile.Tick

// boardPagesPerTick bounds a tick's reads of one hub, so that a long
// backlog is read over several ticks and the reads stay well within the
// credential's 60 a minute.
const boardPagesPerTick = 4

// boardReader is what the service asks of a hub's board feed;
// *hubteams.Client is the real one.
type boardReader interface {
	CanReadBoard() bool
	ReadBoard(ctx context.Context, cursor string, limit int) (hubteams.BoardPage, error)
}

// boardOf is the hub's board reader, or nil when the hub has no board.read
// credential or no hub ID.
func boardOf(b hubBinding) boardReader {
	r, ok := b.teams.(boardReader)
	if !ok || b.id == "" || !r.CanReadBoard() {
		return nil
	}
	return r
}

// readsBoard reports whether any hub's board can be read.
func (s *Server) readsBoard() bool {
	for _, b := range s.hubs {
		if boardOf(b) != nil {
			return true
		}
	}
	return false
}

// readBoards reads the board of each hub that has a team, up to
// boardPagesPerTick pages each.
func (s *Server) readBoards(ctx context.Context) {
	teams, err := s.store.ListTeams(ctx)
	if err != nil {
		s.log.Error("read the board: list teams", "err", err)
		return
	}
	used := map[string]bool{}
	for _, t := range teams {
		used[t.Hub] = true
	}
	for alias, b := range s.hubs {
		if r := boardOf(b); r != nil && used[alias] {
			s.readBoard(ctx, alias, b.id, r)
		}
	}
}

// readBoard reads one hub's board on from its stored cursor and records
// each page. A cursor the hub no longer knows (its state was restored) is
// dropped, and the feed read again from its start; a hub that asks to wait
// is not read again before the time it named.
func (s *Server) readBoard(ctx context.Context, alias, hubID string, r boardReader) {
	if s.boardPaused == nil {
		s.boardPaused = map[string]time.Time{}
	}
	if time.Now().Before(s.boardPaused[alias]) {
		return
	}
	rc := store.ReconcilerCaller()
	for range boardPagesPerTick {
		cur, err := s.store.BoardCursor(ctx, rc, hubID)
		if err != nil {
			s.log.Error("read the board: the cursor", "hub", alias, "err", err)
			return
		}
		page, err := r.ReadBoard(ctx, cur, hubteams.MaxBoardPage)
		if err != nil {
			s.boardRefused(ctx, alias, hubID, err)
			return
		}
		rec := store.BoardPage{From: cur, Cursor: page.Cursor, Changes: make([]store.BoardChange, 0, len(page.Changes))}
		for _, ch := range page.Changes {
			rec.Changes = append(rec.Changes, store.BoardChange{ProjectID: ch.Project, TaskID: ch.TaskID, Revision: ch.Revision,
				From: ch.From, To: ch.To, At: ch.At, RequiredCapability: ch.RequiredCapability})
		}
		n, err := s.store.RecordBoardPage(ctx, rc, hubID, rec)
		if err != nil {
			if !errors.Is(err, store.ErrBoardCursorMoved) {
				s.log.Error("read the board: record", "hub", alias, "err", err)
			}
			return
		}
		if n > 0 {
			s.log.Info("board changes announced", "hub", alias, "announcements", n)
		}
		if !page.More {
			return
		}
	}
}

// boardRefused handles a failed board read.
func (s *Server) boardRefused(ctx context.Context, alias, hubID string, err error) {
	code := hubteams.Code(err)
	switch code {
	case "cursor_ahead", "invalid_cursor":
		s.log.Warn("read the board: the hub refused the stored cursor; reading its feed again from the start", "hub", alias, "code", code)
		if err := s.store.ResetBoardCursor(ctx, store.ReconcilerCaller(), hubID); err != nil {
			s.log.Error("read the board: reset the cursor", "hub", alias, "err", err)
		}
	default:
		s.log.Warn("read the board", "hub", alias, "code", code)
		var e *hubteams.Error
		if errors.As(err, &e) && e.Retryable {
			s.boardPaused[alias] = time.Now().Add(max(e.RetryAfter, BoardTick))
		}
	}
}

// runBoard reads the boards every boardEvery until ctx ends.
func (s *Server) runBoard(ctx context.Context) {
	tick := time.NewTicker(s.boardEvery)
	defer tick.Stop()
	for {
		s.readBoards(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
