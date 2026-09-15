package retirement

import (
	"errors"
	"fmt"
	"time"
)

// CompletionSchema and AnswerSchema number the two documents a retirement's
// tail carries between the two controllers.
const (
	CompletionSchema = 1
	AnswerSchema     = 1
)

// MaxDocumentBytes bounds a read of either document.
const MaxDocumentBytes = 16 << 10

// Completion is what the retiring host emits at its journal's `done`: the
// binding fields of its retirement, for the survivor to complete the ledger
// row against when the retiring host cannot write it itself.
// It records a historically proved retirement bound to DoneAt and these fields,
// not fresh retiring-host health. Local done publication and settlement require
// fresh local proof; survivor completion and acknowledgement preserve history.
type Completion struct {
	Schema       int    `json:"schema"`
	Deployment   string `json:"deployment"`
	Retiring     string `json:"retiring"`
	Survivor     string `json:"survivor"`
	TransitionID string `json:"transition_id"`
	Reservation  string `json:"reservation"`
	DoneAt       string `json:"done_at"`
}

// Answer is what the survivor's helper answers, and what the retiring host
// validates against its own journal before it acknowledges the row.
type Answer struct {
	Schema       int    `json:"schema"`
	Deployment   string `json:"deployment"`
	Retiring     string `json:"retiring"`
	Survivor     string `json:"survivor"`
	TransitionID string `json:"transition_id"`
	Reservation  string `json:"reservation"`
	// Row is "done" or "already".
	Row         string `json:"row"`
	CompletedBy string `json:"completed_by"`
	CompletedAt string `json:"completed_at"`
}

// Row words an answer may carry.
const (
	RowDone    = "done"
	RowAlready = "already"
)

// ErrDocument is a completion or an answer that does not hold: another shape,
// a missing member, a repeated one, or a field that contradicts the record it
// is presented against.
var ErrDocument = errors.New("retirement: the document does not hold")

// DecodeCompletion decodes a completion document strictly and requires every
// binding field.
func DecodeCompletion(raw []byte) (Completion, error) {
	if len(raw) > MaxDocumentBytes {
		return Completion{}, fmt.Errorf("%w: %d bytes is longer than any completion billet writes", ErrDocument, len(raw))
	}

	var c Completion
	if err := strictDecode(raw, &c); err != nil {
		return Completion{}, fmt.Errorf("%w: %w", ErrDocument, err)
	}

	if err := c.wellFormed(); err != nil {
		return Completion{}, fmt.Errorf("%w: %w", ErrDocument, err)
	}

	return c, nil
}

func (c Completion) wellFormed() error {
	switch {
	case c.Schema != CompletionSchema:
		return fmt.Errorf("completion schema %d is not %d", c.Schema, CompletionSchema)
	case c.Deployment == "" || c.Retiring == "" || c.Survivor == "" || c.TransitionID == "" || c.Reservation == "":
		return errors.New("completion misses a binding field (deployment, retiring, survivor, transition_id, reservation)")
	case c.Retiring == c.Survivor:
		return errors.New("completion names one host as both retiring and survivor")
	}

	if _, err := time.Parse(time.RFC3339, c.DoneAt); err != nil {
		return fmt.Errorf("completion carries an unparseable done_at: %w", err)
	}

	return nil
}

// CompletionOf is the document a journal at `done` emits.
func CompletionOf(j Journal) Completion {
	return Completion{
		Schema: CompletionSchema, Deployment: j.Deployment, Retiring: j.Retiring,
		Survivor: j.Survivor.Host, TransitionID: j.Provenance.TransitionID,
		Reservation: j.Provenance.Reservation, DoneAt: j.DoneAt,
	}
}

// DecodeAnswer decodes the survivor's answer strictly and requires every field.
func DecodeAnswer(raw []byte) (Answer, error) {
	if len(raw) > MaxDocumentBytes {
		return Answer{}, fmt.Errorf("%w: %d bytes is longer than any answer billet writes", ErrDocument, len(raw))
	}

	var a Answer
	if err := strictDecode(raw, &a); err != nil {
		return Answer{}, fmt.Errorf("%w: %w", ErrDocument, err)
	}

	if err := a.wellFormed(); err != nil {
		return Answer{}, fmt.Errorf("%w: %w", ErrDocument, err)
	}

	return a, nil
}

func (a Answer) wellFormed() error {
	switch {
	case a.Schema != AnswerSchema:
		return fmt.Errorf("answer schema %d is not %d", a.Schema, AnswerSchema)
	case a.Deployment == "" || a.Retiring == "" || a.Survivor == "" || a.TransitionID == "" || a.Reservation == "":
		return errors.New("answer misses a binding field (deployment, retiring, survivor, transition_id, reservation)")
	case a.Row != RowDone && a.Row != RowAlready:
		return fmt.Errorf("answer's row is %q, not done or already", a.Row)
	case a.CompletedBy == "":
		return errors.New("answer names nobody as completed_by")
	}

	if _, err := time.Parse(time.RFC3339, a.CompletedAt); err != nil {
		return fmt.Errorf("answer carries an unparseable completed_at: %w", err)
	}

	return nil
}

// MatchesJournal holds an answer to the local `done` journal's provenance,
// field by field, before the row is acknowledged; the first disagreement is
// named.
func (a Answer) MatchesJournal(j Journal) error {
	mismatch := func(field, have, want string) error {
		return fmt.Errorf("%w: the answer's %s is %q, the journal's %q", ErrDocument, field, have, want)
	}

	switch {
	case a.Deployment != j.Deployment:
		return mismatch("deployment", a.Deployment, j.Deployment)
	case a.Retiring != j.Retiring:
		return mismatch("retiring", a.Retiring, j.Retiring)
	case a.Survivor != j.Survivor.Host:
		return mismatch("survivor", a.Survivor, j.Survivor.Host)
	case a.TransitionID != j.Provenance.TransitionID:
		return mismatch("transition_id", a.TransitionID, j.Provenance.TransitionID)
	case a.Reservation != j.Provenance.Reservation:
		return mismatch("reservation", a.Reservation, j.Provenance.Reservation)
	}

	return nil
}
