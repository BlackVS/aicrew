package server

import (
	"context"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// The event core's delivery (docs/DESIGN-TURNS.md, 12.4; store.DeliverEvents):
// every EventTick, aicrewd runs the handler of each event queue that is due,
// so a burst of board or forge events about one subject wakes its members
// once. The events are journaled by their sources; a queue left pending by a
// crash or a stop runs on the next tick after a start.

// EventTick is how often the event queues are looked at. A queue runs once
// its events have been quiet for store.EventQuiet, so a member is woken
// between that and that plus a tick after the last event.
const EventTick = 5 * time.Second

// deliverEvents runs the due event queues once.
func (s *Server) deliverEvents(ctx context.Context) {
	runs, err := s.store.DeliverEvents(ctx, store.ReconcilerCaller())
	if err != nil {
		s.log.Error("deliver events", "err", err)
	}
	for _, r := range runs {
		switch r.Outcome {
		case "handled":
		case "parked":
			s.log.Error("deliver events: a handler failed three times in a row; its events are parked",
				"subscription", r.Subscription, "team", r.TeamID, "subject", r.Subject, "events", r.Events, "err", r.Error)
		default:
			s.log.Warn("deliver events: a handler failed", "subscription", r.Subscription, "team", r.TeamID,
				"subject", r.Subject, "events", r.Events, "err", r.Error)
		}
	}
}

// WithEventTick sets how often the event queues are looked at, for tests
// that need a delivery sooner than EventTick.
func WithEventTick(d time.Duration) Option {
	return func(s *Server) { s.eventsEvery = d }
}

// runEvents delivers events every eventsEvery until ctx ends.
func (s *Server) runEvents(ctx context.Context) {
	every := s.eventsEvery
	if every <= 0 {
		every = EventTick
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		s.deliverEvents(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
