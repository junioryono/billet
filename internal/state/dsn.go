package state

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// DSN is a PostgreSQL connection string, which carries a password.
//
// ITS OWN TYPE, NOT A string. Two things follow. A container that hands
// collaborators out by type cannot confuse it with any other string, keyed or
// not, so a constructor asking for a DSN gets the DSN and nothing else. And
// every rendering path redacts it: a diagnostic that names the type of a
// dependency that failed to construct names `state.DSN`, and one that formats
// the value prints the sentence below rather than the password.
//
// The five methods each cover a path the others do not: slog's JSON handler
// never consults fmt, %#v never consults String, and an unrecognised verb falls
// back to the underlying string unless Format takes it. They are on VALUE
// receivers so a DSN reached through a field is covered too.
type DSN string

const redactedDSN = "[redacted dsn]"

// String redacts the connection string.
func (DSN) String() string { return redactedDSN }

// GoString covers %#v, which does not consult String.
func (d DSN) GoString() string { return d.String() }

// Format makes EVERY verb safe, not only the ones fmt.Stringer covers.
func (d DSN) Format(s fmt.State, verb rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, d.String())

	_ = verb
}

// MarshalJSON keeps the password out of anything that serializes a DSN.
func (d DSN) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(d.String())
	if err != nil {
		return nil, fmt.Errorf("state: marshal redacted dsn: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (d DSN) LogValue() slog.Value { return slog.StringValue(d.String()) }
