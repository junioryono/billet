package guestreport

import (
	"errors"
	"strconv"
)

// The reasons a value is refused. Each is wrapped in an *Error naming where.
var (
	// ErrMalformed is bytes that are not one DEFLATE stream of one JSON value of
	// the type asked for.
	ErrMalformed = errors.New("not one compressed JSON value of the schema")
	// ErrTooLarge is an encoding over its bound, compressed or inflated.
	ErrTooLarge = errors.New("larger than its bound")
	// ErrUnknownField is a JSON key the schema does not have.
	ErrUnknownField = errors.New("a field the schema does not have")
	// ErrInvalidUTF8 is JSON that is not valid UTF-8.
	ErrInvalidUTF8 = errors.New("not valid UTF-8")
	// ErrNotCanonical is JSON that decodes but is not what the encoder writes for
	// the value: a missing field, a repeated or differently cased key, an escape
	// the encoder would not use, an empty list spelled out.
	ErrNotCanonical = errors.New("not the encoding of the value it decodes to")
	// ErrNameTooLong is text over its bound.
	ErrNameTooLong = errors.New("longer than its bound")
	// ErrControlCharacter is text holding a control character, which a terminal
	// showing it would act on.
	ErrControlCharacter = errors.New("holds a control character")
	// ErrEmpty is a value that must be set and is not.
	ErrEmpty = errors.New("not set")
	// ErrTooMany is a list over its bound.
	ErrTooMany = errors.New("more entries than its bound")
	// ErrNotMonotonic is a time or a counter that goes back.
	ErrNotMonotonic = errors.New("goes back")
	// ErrOutOfRange is a number below zero or above its bound.
	ErrOutOfRange = errors.New("out of range")
	// ErrDuplicateName is a process name twice in one sample.
	ErrDuplicateName = errors.New("names a process twice")
	// ErrInconsistent is values that contradict each other.
	ErrInconsistent = errors.New("contradicts another field")
	// ErrNoBatches is a merge with no batch it could keep.
	ErrNoBatches = errors.New("no batch to merge")
	// ErrCannotFit is a report that does not fit its bound at the lowest
	// resolution.
	ErrCannotFit = errors.New("does not fit its bound at any resolution")
)

// Error is a refusal: one of the sentinels above, and where in the value it was
// found. Where is built from the schema's own field names and indices, never from
// anything the guest wrote.
type Error struct {
	Err   error
	Where string
}

func (e *Error) Error() string {
	if e.Where == "" {
		return "guestreport: " + e.Err.Error()
	}

	return "guestreport: " + e.Where + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func refuse(err error, where string) error { return &Error{Err: err, Where: where} }

// at is where[i].
func at(where string, i int) string { return where + "[" + strconv.Itoa(i) + "]" }
