package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/BlackVS/aicrew/internal/store"
)

// A member's inbox (pilot G1): its oldest unacknowledged messages, and their
// acknowledgement, as the token's session. A lifecycle message names the
// attempt it announces, so a worker reads an offer's attempt ID here.

const (
	InboxPath = "/v1/crew/inbox"
	// InboxPendingPath lists the unacknowledged messages without delivering
	// them.
	InboxPendingPath = InboxPath + "/pending"
	InboxAckPath     = InboxPath + "/ack"

	defaultInboxPage = 20
	maxInboxPage     = 100
)

func (s *Server) registerInbox() {
	s.handle(http.MethodGet, InboxPath, s.inbox)
	s.handle(http.MethodPost, InboxAckPath, s.inboxAck)
	s.handle(http.MethodGet, InboxPendingPath, s.inboxPending)
}

// inboxPending is GET /v1/crew/inbox/pending: the member's unacknowledged
// messages by id and kind, without content and without recording a
// delivery. The launcher's wake-up polls it; the inbox read stays the only
// delivery.
func (s *Server) inboxPending(w http.ResponseWriter, r *http.Request) {
	pending, err := s.store.PendingInboxWithToken(r.Context(), sessionToken(r))
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Pending []store.PendingMessage `json:"pending"`
	}{pending})
}

type inboxReply struct {
	Messages []store.InboxItem `json:"messages"`
}

// inbox delivers a page of the member's oldest unacknowledged messages, and
// records their delivery. ?limit= is 1 to 100 (default 20); any other query
// parameter is refused.
func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	if _, err := s.store.AuthenticateSessionToken(r.Context(), token); err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	limit := defaultInboxPage
	q := r.URL.Query()
	for name := range q {
		if name != "limit" {
			s.refuseSession(w, r, "invalid_request", false, 0)
			return
		}
	}
	if v := q["limit"]; len(v) > 0 {
		n, err := strconv.Atoi(v[0])
		if len(v) != 1 || err != nil || n < 1 || n > maxInboxPage {
			s.refuseSession(w, r, "invalid_request", false, 0)
			return
		}
		limit = n
	}
	items, err := s.store.ReadInboxWithToken(r.Context(), token, limit)
	if err != nil {
		s.refuseSession(w, r, inboxRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, inboxReply{Messages: items})
}

// inboxAck acknowledges the named messages, every one of which must have
// been delivered to the member; an Idempotency-Key makes a retry exact.
func (s *Server) inboxAck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	res, err := s.store.AckWithToken(r.Context(), key, token, in.IDs)
	if err != nil {
		s.refuseSession(w, r, inboxRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func inboxRefusal(err error) string {
	if errors.Is(err, store.ErrNotDelivered) {
		return "message_not_delivered"
	}
	return refusalCode(err)
}
