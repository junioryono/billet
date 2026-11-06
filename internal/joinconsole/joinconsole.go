// Package joinconsole is the line a pool host writes to its system console before it
// asks to join, naming its order and its key's fingerprint, so a source that reads the
// console of the exact host it placed can say which key is that host's (ADR-030). It is
// the one spelling, in a package of its own because both sides use it: the host writes
// it (`billet join --pool-order`) and the control plane's host source reads it, and
// neither may import the other's layer.
//
// A leaf: the standard library only.
package joinconsole

import (
	"bytes"
	"io"
	"regexp"
	"strings"
)

// prefix begins the line.
const prefix = "billet-pool-join "

// Line is the line a pool host writes for its order and its key's fingerprint.
func Line(order, fingerprint string) string {
	return prefix + "order=" + order + " fingerprint=" + fingerprint
}

// wholeLine is one whole line, a carriage return before its newline allowed: a
// fingerprint is SHA-256 in unpadded base64 (43 characters, wirecert.Fingerprint). A
// line the console cut short, or that another write ran into, matches nothing, so it
// is never read as a key.
//
// ONLY A LINE THAT IS NOTHING ELSE: text quoted inside another line (an error a
// network answer wrote, a status) is boot output on the same console, and read wherever
// it sat it would name a key the host never wrote. A carriage return does not start a
// line, for the same reason.
var wholeLine = regexp.MustCompile(
	`^billet-pool-join order=([a-z0-9][a-z0-9-]*) fingerprint=(SHA256:[A-Za-z0-9+/]{43})\r?$`)

// Fingerprints is every distinct fingerprint a console names for order, in the order
// first written. A console carries a boot's whole output, terminal escapes and carriage
// returns included, and is read as text: only a whole line is read.
//
// A LINE IS WHAT TWO NEWLINES BOUND, never the edge of what was read: a console is read
// as its newest bytes, which can begin part way through a line or end before its
// newline, and either edge would make a fragment of another line read as a whole one.
func Fingerprints(console, order string) []string {
	first, last := strings.IndexByte(console, '\n'), strings.LastIndexByte(console, '\n')
	if first < 0 || first == last {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(console[first+1:last], "\n") {
		m := wholeLine.FindStringSubmatch(line)
		if len(m) != 3 || m[1] != order {
			continue
		}
		fp := strings.TrimSpace(m[2])
		if !seen[fp] {
			seen[fp] = true
			out = append(out, fp)
		}
	}

	return out
}

// Guard is what a pool host's join prints through, so that none of it can stand for
// the line the host writes to its console: a boot's output reaches the same console,
// and text a network answer carried would otherwise be read as naming a key. Each line
// is written whole, its carriage returns as the two characters `\r` and every spelling
// of the line's start without its hyphens; a line longer than a console holds is ended
// where it is cut, so the part after starts a line that is guarded on its own.
type Guard struct {
	w   io.Writer
	buf []byte
}

// maxGuardedLine is longer than any console line a source reads (its newest 64 KiB).
const maxGuardedLine = 64 << 10

// NewGuard guards w.
func NewGuard(w io.Writer) *Guard { return &Guard{w: w} }

// Write writes every whole line p completes.
func (g *Guard) Write(p []byte) (int, error) {
	g.buf = append(g.buf, p...)
	for {
		i := bytes.IndexByte(g.buf, '\n')
		switch {
		case i >= 0:
			if err := g.emit(g.buf[:i], true); err != nil {
				return len(p), err
			}
			g.buf = g.buf[i+1:]

			continue
		case len(g.buf) > maxGuardedLine:
			if err := g.emit(g.buf, true); err != nil {
				return len(p), err
			}
			g.buf = nil
		}

		return len(p), nil
	}
}

// Flush writes what is left, a line no newline ended.
func (g *Guard) Flush() error {
	if len(g.buf) == 0 {
		return nil
	}
	err := g.emit(g.buf, false)
	g.buf = nil

	return err
}

// Defang is s as a guard writes it: its carriage returns as `\r`, and every spelling
// of the line's start without its hyphens.
func Defang(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)

	return strings.ReplaceAll(s, strings.TrimSpace(prefix), "billet pool join")
}

// GuardError is err as a guard would print it, for an error printed past the guard;
// errors.Is and errors.As still see err.
func GuardError(err error) error {
	if err == nil {
		return nil
	}

	return guardedError{err}
}

type guardedError struct{ err error }

func (g guardedError) Error() string { return Defang(g.err.Error()) }
func (g guardedError) Unwrap() error { return g.err }

func (g *Guard) emit(line []byte, newline bool) error {
	s := Defang(string(line))
	if newline {
		s += "\n"
	}
	_, err := io.WriteString(g.w, s)

	return err
}
