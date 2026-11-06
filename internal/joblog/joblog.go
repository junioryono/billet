// Package joblog is the words a kept job log is described in: a chunk of a step's
// lines as the agent in a job's VM sends it, as a node stores it and as the control
// plane indexes it, and its one codec.
//
// A LOG IS WHAT THE JOB WROTE. The job is root in its VM and can write anything here,
// so nothing in a chunk may drive a billet decision, it is quoted wherever it is
// shown, and no refusal quotes a byte of it. Unlike a guest report the control plane
// does read it, to index it, which is why it is a package of its own: reading a log
// grants nothing the depguard rule guestview holds back.
//
// Codec 1 is the chunk as JSON, compressed with DEFLATE. It imports nothing of
// billet's.
package joblog

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Codec is the encoding Encode writes and Decode reads.
const Codec = 1

// The bounds of one chunk. A chunk holds one step's lines; MaxLineBytes cuts a line,
// since a job can write one of any length, and MaxLines is what a job's line numbers
// may reach, which the index packs into 26 bits.
const (
	MaxChunkBytes         = 128 << 10
	MaxChunkInflatedBytes = 1 << 20
	MaxChunkLines         = 16 << 10
	MaxLineBytes          = 8 << 10
	MaxStepNameBytes      = 64
	MaxSteps              = 1 << 16
	MaxLines              = 1 << 26
)

// Chunk is some of one step's lines, in the order the runner wrote them. Seq numbers
// the agent's chunks from 1, so the node can drop a retried one. Step counts the job's
// steps from 0, in the order their logs appeared, and StepName is the step's display
// name where the runner's own log gave one, empty for the runner's setup and cleanup.
type Chunk struct {
	Seq      uint64 `json:"seq"`
	Step     int64  `json:"step"`
	StepName string `json:"step_name,omitempty"`
	Lines    []Line `json:"lines"`
}

// Line is one line of a step's log: what the runner stamped it with, in Unix
// milliseconds on the guest's clock and zero where it carried none, and its text.
type Line struct {
	AtMillis int64  `json:"at_ms,omitempty"`
	Text     string `json:"text"`
}

// ErrRefused is a chunk this package will not read. It never says what the chunk held.
var ErrRefused = errors.New("joblog: not a chunk of the codec")

// ErrTooLarge is a chunk over its bound, compressed or inflated.
var ErrTooLarge = errors.New("joblog: a chunk over its bound")

// Encode is c in Codec, refused if it would not decode or is over MaxChunkBytes.
func Encode(c Chunk) ([]byte, error) {
	if err := validate(&c); err != nil {
		return nil, err
	}

	raw, err := json.Marshal(c)
	if err != nil {
		return nil, ErrRefused
	}

	if len(raw) > MaxChunkInflatedBytes {
		return nil, ErrTooLarge
	}

	var out bytes.Buffer

	w, err := flate.NewWriter(&out, flate.BestSpeed)
	if err != nil {
		return nil, err
	}

	if _, err := w.Write(raw); err != nil {
		return nil, err
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	if out.Len() > MaxChunkBytes {
		return nil, ErrTooLarge
	}

	return out.Bytes(), nil
}

// Decode reverses Encode, refusing anything Encode would not have written. Its errors
// are its own sentinels, so they quote nothing the guest wrote.
func Decode(data []byte) (Chunk, error) {
	if len(data) > MaxChunkBytes {
		return Chunk{}, ErrTooLarge
	}

	in := bytes.NewReader(data)

	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(in), MaxChunkInflatedBytes+1))
	if err != nil {
		return Chunk{}, ErrRefused
	}

	if len(raw) > MaxChunkInflatedBytes {
		return Chunk{}, ErrTooLarge
	}

	if in.Len() != 0 || !utf8.Valid(raw) {
		return Chunk{}, ErrRefused
	}

	var c Chunk

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&c); err != nil {
		return Chunk{}, ErrRefused
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Chunk{}, ErrRefused
	}

	if err := validate(&c); err != nil {
		return Chunk{}, err
	}

	return c, nil
}

func validate(c *Chunk) error {
	switch {
	case c.Seq == 0 || c.Seq > 1<<53:
		return fmt.Errorf("%w: its sequence number is out of range", ErrRefused)
	case c.Step < 0 || c.Step >= MaxSteps:
		return fmt.Errorf("%w: its step is out of range", ErrRefused)
	case len(c.StepName) > MaxStepNameBytes || !isText(c.StepName):
		return fmt.Errorf("%w: its step name is not text within its bound", ErrRefused)
	case len(c.Lines) == 0 || len(c.Lines) > MaxChunkLines:
		return fmt.Errorf("%w: it holds a number of lines out of range", ErrRefused)
	}

	for i := range c.Lines {
		l := &c.Lines[i]
		if l.AtMillis < 0 || l.AtMillis > 1<<53 || len(l.Text) > MaxLineBytes || !isText(l.Text) {
			return fmt.Errorf("%w: line %d is not text within its bound", ErrRefused, i)
		}
	}

	return nil
}

// isText says s is UTF-8 with no control character but a tab, which a terminal showing
// it would act on.
func isText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return r != '\t' && unicode.IsControl(r)
	})
}

// Clean is s as a chunk may carry it: a terminal's colour and cursor sequences removed,
// which the runner keeps in what it logs (measured 2026-11-04: `\x1b[36;1m`), every
// other control character and invalid byte replaced by '?', and the whole cut to limit
// bytes on a character boundary.
func Clean(s string, limit int) string {
	var b strings.Builder

	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
				j++
			}

			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
				i = j + 1

				continue
			}
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 || (r != '\t' && unicode.IsControl(r)) {
			r = '?'
		}

		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}

		b.WriteRune(r)

		i += size
	}

	return b.String()
}

// Level is what a line is inferred to be, from its own text and so only as true as the
// job chose to make it. A search filters on it and nothing else reads it.
type Level int

// The levels, in the two bits the index keeps.
const (
	LevelOther Level = iota
	LevelWarning
	LevelError
)

// String is the level as a search names it.
func (l Level) String() string {
	switch l {
	case LevelWarning:
		return "warning"
	case LevelError:
		return "error"
	default:
		return "other"
	}
}

// ParseLevel is a level as a search names it, false for any other word.
func ParseLevel(s string) (Level, bool) {
	switch s {
	case "other":
		return LevelOther, true
	case "warning":
		return LevelWarning, true
	case "error":
		return LevelError, true
	default:
		return 0, false
	}
}

// LevelOf infers a line's level: the runner's own `##[error]` and `##[warning]` marks
// first, then a line beginning as a compiler's or a test tool's would.
func LevelOf(text string) Level {
	switch {
	case strings.HasPrefix(text, "##[error]"):
		return LevelError
	case strings.HasPrefix(text, "##[warning]"):
		return LevelWarning
	}

	head := strings.ToLower(strings.TrimLeft(text[:min(len(text), 32)], " \t"))
	for _, word := range []string{"error:", "error ", "fatal:", "fatal ", "panic:"} {
		if strings.HasPrefix(head, word) {
			return LevelError
		}
	}

	for _, word := range []string{"warning:", "warning ", "warn:", "warn "} {
		if strings.HasPrefix(head, word) {
			return LevelWarning
		}
	}

	return LevelOther
}
