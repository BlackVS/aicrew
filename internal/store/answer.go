package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BlackVS/aicrew/internal/events"
)

const opAnswer = "attempt.answer"

// MaxAnswerText bounds an answer as JSON encodes it, so that the event
// (which holds it encoded) and the worker's message (which holds it as it
// is, never longer) carry it whole beside their other fields.
const MaxAnswerText = 12 << 10

// answerSize is the bytes JSON encodes text in.
func answerSize(text string) int {
	b, _ := json.Marshal(text)
	return len(b)
}

// BlockedAnswer is the team's current coordinator's answer to a worker's
// blocked report, from its session.
type BlockedAnswer struct {
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
	Text       string `json:"text"`
}

// answerEvent is an attempt.answered event's payload.
type answerEvent struct {
	AttemptID   string  `json:"attempt_id"`
	Task        TaskRef `json:"task"`
	Worker      string  `json:"worker_agent_id"`
	Coordinator string  `json:"coordinator_agent_id"`
	Text        string  `json:"text"`
}

// AnswerWithToken records the token's session's answer to the blocked
// attempt: the session must be the team's current coordinator, and the
// attempt its team's, running and blocked. The answer is journaled as an
// attempt.answered event, which wakes the worker (answer-wake). It is local.
func (s *Store) AnswerWithToken(ctx context.Context, key, token, attemptID, text string) (Attempt, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Attempt{}, err
	}
	cmd, _ := s.withToken(answerCommand(c, key, attemptID,
		BlockedAnswer{SessionID: t.sessionID, Generation: t.generation, Text: text}), token, t)
	var out Attempt
	return out, s.run(ctx, c, cmd, &out)
}

func answerCommand(c Caller, key, attemptID string, in BlockedAnswer) command {
	return command{
		op: opAnswer, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			BlockedAnswer
		}{attemptID, in},
		authorize: requireAgent,
		validate: func() error {
			if !utf8.ValidString(in.Text) || strings.TrimSpace(in.Text) == "" || answerSize(in.Text) > MaxAnswerText {
				return fmt.Errorf("%w: an answer must be non-blank UTF-8 of at most %d bytes as JSON encodes it",
					ErrInvalid, MaxAnswerText)
			}
			return nil
		},
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentCoordinator(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, in.SessionID)
			if err != nil {
				return nil, err
			}
			a, err := getAttempt(ctx, tx, attemptID)
			if err != nil {
				return nil, err
			}
			if a.TeamID != sess.TeamID {
				return nil, fmt.Errorf("attempt %s: %w", attemptID, ErrNotFound)
			}
			if a.WorkerAgentID == sess.AgentID {
				return nil, fmt.Errorf("%w: the worker of attempt %s cannot answer its own blocked report", ErrForbidden, a.ID)
			}
			if a.State != AttemptRunning || a.Phase != PhaseBlocked {
				return nil, fmt.Errorf("attempt %s is %s (%s), not blocked: %w", a.ID, a.State, a.Phase, ErrAttemptState)
			}
			// The command's key, which is the caller's own, makes the
			// event's with the caller: a replay returns the recorded result
			// before this runs, and the key keeps it once.
			if _, _, err := journal(ctx, tx, sourceAicrewd, "answer/"+a.ID+"/"+sess.AgentID+"/"+key, attemptSubject(a.ID),
				events.KindAttemptAnswered,
				answerEvent{AttemptID: a.ID, Task: a.Task, Worker: a.WorkerAgentID, Coordinator: sess.AgentID, Text: in.Text},
				now, now, matches(events.KindAttemptAnswered, a.TeamID, events.Facts{})); err != nil {
				return nil, err
			}
			return a, nil
		},
	}
}

// wakeAnswer wakes an attempt's worker with its coordinators' answers to its
// blocked report, in order, while the attempt is open.
func wakeAnswer(ctx context.Context, tx *sql.Tx, evs []queuedEvent, roles []string) (*wakeOut, error) {
	var last answerEvent
	var texts []string
	for i, ev := range evs {
		var e answerEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			return nil, fmt.Errorf("read an answer event: %w", err)
		}
		if i > 0 && e.AttemptID != last.AttemptID {
			return nil, errors.New("the queue holds answers to two attempts")
		}
		last = e
		texts = append(texts, e.Text)
	}
	if !slices.Contains(roles, events.RoleWorker) {
		return nil, nil
	}
	a, err := getAttempt(ctx, tx, last.AttemptID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.State == AttemptClosed {
		return nil, nil
	}
	coordinator, err := agentLabel(ctx, tx, last.Coordinator)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("action.needed: %s answered your blocked report on task %s: %s", coordinator, taskName(a.Task),
		strings.Join(texts, "\n\n"))
	if a.Phase != PhaseBlocked {
		text += fmt.Sprintf("\n\nThe attempt has since moved to %s.", a.Phase)
	} else {
		text += "\n\nAct on the answer, then resume the work (`aicrew-agent step work`) or report what still blocks you."
	}
	task := a.Task
	sub := ActionSubject{Kind: "answer", AttemptID: a.ID, Task: &task, Text: boundedText(text)}
	return &wakeOut{recipients: []string{a.WorkerAgentID}, subject: sub}, nil
}
