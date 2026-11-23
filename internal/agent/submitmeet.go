package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// A member's own submit and its launcher's gates-green submit of the same
// attempt can meet on aicrewd (01a206f8-24d0): the second begin is refused
// with attempt_state, because the first is in flight or has made the
// attempt submitted. The answers below say so to whichever side came
// second. They match no text of aicrewd: only the refusal's code, the
// launcher's own record of a submit in flight or committed, and the
// session status, read once after the refusal. Anything else is the
// refusal as it is.

// The words the answers below begin their facts with, which the worker's
// guidance quotes (commands.go, submitOrder).
const (
	inFlightWords  = "This launcher is submitting"
	submittedWords = "is submitted already"
)

// submitInFlight is the request key of a work submit of attemptID that
// this home's driver has begun and not settled, or "". A begun step holds
// aicrewd's pending step, so every work begin of the attempt is refused
// while it lasts.
func (d *Driver) submitInFlight(attemptID string) string {
	recs, err := d.Pending()
	if err != nil {
		return ""
	}
	path := attemptsPath + "/" + url.PathEscape(attemptID) + "/work"
	for _, p := range recs {
		// A record holds its step from the begin's answer on (drive).
		if p.Request.Path == path && p.Step != nil && workIntent(p.Request.Body) == "submit" {
			return p.Step.RequestKey
		}
	}
	return ""
}

// submitsDir holds the link of each attempt's latest submit that committed
// through this launcher, one file per attempt. A settled step's record is
// dropped (steps.go), so it cannot say what was submitted.
func submitsDir(home string) string { return filepath.Join(home, "state", "submits") }

// committedSubmit is the record of an attempt's latest committed submit:
// the attempt and the result link, nothing else.
type committedSubmit struct {
	Version   int    `json:"version"`
	AttemptID string `json:"attempt_id"`
	Ref       string `json:"ref"`
}

// noteSubmit records the link of a work submit that settled as committed,
// replacing the attempt's earlier record. A step that is not a submit, an
// attempt ID of another shape, an empty link, or a link that holds a token
// of the home writes nothing; a write that fails leaves no record, which
// reads as none.
func (d *Driver) noteSubmit(p *pendingStep) {
	ref := workDetail(p.Request.Body)
	if workIntent(p.Request.Body) != "submit" || !attemptIDShape(p.AttemptID) || ref == "" ||
		redactText(d.Home, ref) != ref {
		return
	}
	raw, err := json.Marshal(committedSubmit{Version: 1, AttemptID: p.AttemptID, Ref: ref})
	if err != nil {
		return
	}
	dir := submitsDir(d.Home)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".submit-*.json")
	if err != nil {
		return
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), filepath.Join(dir, p.AttemptID+".json")) != nil {
		os.Remove(tmp.Name())
	}
}

// submittedRef is the link of attemptID's latest committed submit as this
// launcher recorded it, or "" for no record, an unreadable or malformed
// one, or one of another attempt.
func (d *Driver) submittedRef(attemptID string) string {
	if !attemptIDShape(attemptID) {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(submitsDir(d.Home), attemptID+".json"))
	if err != nil {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c committedSubmit
	if dec.Decode(&c) != nil || c.Version != 1 || c.AttemptID != attemptID {
		return ""
	}
	return c.Ref
}

// submitMet is what met a submit of an attempt that aicrewd refused:
// another submit of it in flight in this home (inFlight, its request key),
// or the attempt submitted already, with the link of this launcher's
// record of its latest committed submit (ref, "" for none).
type submitMet struct {
	inFlight string
	ref      string
}

// meetSubmit decides what a refused submit of attempt met; ok is false
// when it met nothing, and then the refusal stands as it is. Only an
// attempt_state refusal can have met another submit. A submit in flight
// in this home decides first; otherwise the status, read once, must show
// the same attempt running in the phase submitted. A status that cannot be
// read, or shows anything else, decides nothing: the phase is always the
// live status's, never the record's.
func meetSubmit(err error, inFlight string, status func() (memberStatus, error), recorded func() string,
	attempt string) (submitMet, bool) {
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "attempt_state" {
		return submitMet{}, false
	}
	if inFlight != "" {
		return submitMet{inFlight: inFlight}, true
	}
	st, serr := status()
	if serr != nil || st.Attempt == nil || st.Attempt.ID != attempt || st.Attempt.State != "running" ||
		st.Attempt.Phase != "submitted" {
		return submitMet{}, false
	}
	return submitMet{ref: recorded()}, true
}

// meetSubmitFor is meetSubmit for attempt in this launcher.
func (s *StepServer) meetSubmitFor(ctx context.Context, err error, attempt string) (submitMet, bool) {
	return meetSubmit(err, s.driver.submitInFlight(attempt), func() (memberStatus, error) {
		return s.Status(ctx)
	}, func() string { return s.driver.submittedRef(attempt) }, attempt)
}

// submitMeetAnswer is the channel's answer to a member's submit of attempt
// with the result link detail that met another submit of it (m). A submit
// in flight is pending, never done: it may still settle as not committed.
// A submitted attempt is done only when this launcher's record shows the
// same link; otherwise the refusal stands with the fact added, since a
// member that meant another result must not read "nothing to do".
func submitMeetAnswer(err error, m submitMet, attempt, detail string) StepAnswer {
	if m.inFlight != "" {
		b, _ := json.Marshal(map[string]string{"attempt_id": attempt, "request_key": m.inFlight})
		return StepAnswer{OK: true, Status: StepPending, Result: b,
			NextAction: fmt.Sprintf(inFlightWords+" attempt %s already (request key %s, not settled): do "+
				"not submit it again. The attempt's lifecycle message tells you when it is submitted; until that "+
				"message comes, it is not.", attempt, m.inFlight)}
	}
	if m.ref != "" && m.ref == detail {
		b, _ := json.Marshal(map[string]any{"attempt_id": attempt, "submitted": true, "ref": m.ref})
		return StepAnswer{OK: true, Status: StepDone, Result: b,
			NextAction: fmt.Sprintf("Attempt %s "+submittedWords+", with the result %s: there is nothing to submit. "+
				"Anything further about the attempt, a stop included, comes as its own message.", attempt, m.ref)}
	}
	ans := answerErr(err)
	if m.ref != "" {
		ans.Error.Message += fmt.Sprintf(" Attempt %s "+submittedWords+"; this launcher's record shows the result %s.",
			attempt, m.ref)
	} else {
		ans.Error.Message += fmt.Sprintf(" Attempt %s "+submittedWords+"; this launcher has no record of a submit.",
			attempt)
	}
	return ans
}

// workDetail is a work step's detail, the result link of a submit.
func workDetail(body []byte) string {
	var b struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Detail
}
