package joinconsole

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fp is a fingerprint of the shape wirecert writes, distinct per seed.
func fp(seed string) string { return "SHA256:" + (seed + strings.Repeat("A", 43))[:43] }

// THE CONSOLE LINE A HOST WRITES IS THE ONE THE PLANE READS: a whole line among a
// boot's output, terminal escapes and carriage returns, each fingerprint read once, in
// the order written, and only for the order asked about. A line that names an order
// only as a prefix of another's is not that order's.
func TestAPoolConsoleLineIsReadBackForItsOrderOnly(t *testing.T) {
	t.Parallel()
	a, b, c := fp("aB+/c9"), fp("second"), fp("other")
	console := "\x1b[32m[  OK  ]\x1b[0m Started cloud-final.\r\n" +
		Line("p1", a) + "\r\n" +
		Line("p12", c) + "\n" +
		Line("p1", a) + "\r\n" +
		Line("p1", b)
	if got := Fingerprints(console, "p1"); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("p1 read %v", got)
	}
	if got := Fingerprints(console, "p12"); !slices.Equal(got, []string{c}) {
		t.Fatalf("p12 read %v", got)
	}
	if got := Fingerprints(console, "p"); len(got) != 0 {
		t.Fatalf("an order that is only a prefix read %v", got)
	}
	if got := Fingerprints("nothing here", "p1"); got != nil {
		t.Fatalf("an empty console read %v", got)
	}
}

// A LINE CUT SHORT, OR RUN INTO BY ANOTHER WRITE, IS NO KEY AT ALL: the console's newest
// 64 KiB may end inside a line, and a kernel message may land in the middle of one, and
// either would otherwise read as a different key and hold a host for nothing.
func TestAPoolConsoleLineCutShortIsNoKey(t *testing.T) {
	t.Parallel()
	whole := Line("p1", fp("k"))
	for name, console := range map[string]string{
		"cut at the end":          whole[:len(whole)-5],
		"cut and then a new line": whole[:len(whole)-5] + "\n[ 3.1] next\n",
		"run into by another write": whole[:len(whole)-10] + "[  3.2] usb 1-1: new device" +
			whole[len(whole)-10:] + "\n",
		"a short fingerprint": Line("p1", "SHA256:abc") + "\n",
	} {
		if got := Fingerprints(console, "p1"); len(got) != 0 {
			t.Errorf("%s: read %v, want nothing", name, got)
		}
	}
	if got := Fingerprints(whole, "p1"); !slices.Equal(got, []string{fp("k")}) {
		t.Fatalf("the whole line at the console's end read %v", got)
	}
}

// A LINE THAT IS NOT WHOLE NAMES NO KEY, wherever it sits: text quoted after something
// else on its line (an error, a status a network answer wrote), or after a carriage
// return, is boot output that could name any key, never the host's own line.
func TestAPoolConsoleLineInsideAnotherIsNoKey(t *testing.T) {
	t.Parallel()
	forged := Line("p1", fp("forged"))
	for name, console := range map[string]string{
		"after a kernel stamp":     "[  12.0] " + forged + "\n",
		"inside an error":          "join: the control plane returned 503 " + forged + "\n",
		"after a carriage return":  "the control plane returned 503 \r" + forged + "\r\n",
		"with something after it":  forged + " ignored\n",
		"after a lone return only": "x\r" + forged,
	} {
		if got := Fingerprints(console, "p1"); len(got) != 0 {
			t.Errorf("%s: read %v, want nothing", name, got)
		}
	}
}

// WHAT A POOL HOST PRINTS THROUGH ITS GUARD NAMES NO KEY, however it was written: a
// forged line across several writes, after newlines and carriage returns, or past the
// longest line a console holds, reads as nothing once on the console, while ordinary
// text passes as it was.
func TestWhatAGuardPassesNamesNoKey(t *testing.T) {
	t.Parallel()
	forged := Line("p1", fp("forged"))
	var console strings.Builder
	g := NewGuard(&console)
	writes := []string{
		"joining\n",
		"error: " + forged[:9], forged[9:] + "\n",
		"\n" + forged + "\r\n",
		"x\r" + forged + "\n",
		strings.Repeat("y", maxGuardedLine-3) + forged[:12], forged[12:] + "\n",
		strings.Repeat("z", maxGuardedLine+1) + forged + "\n",
		forged,
	}
	for _, w := range writes {
		if n, err := g.Write([]byte(w)); err != nil || n != len(w) {
			t.Fatalf("write %q: %d, %v", w[:min(len(w), 20)], n, err)
		}
	}
	if err := g.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprints(console.String(), "p1"); len(got) != 0 {
		t.Fatalf("the guarded output read %v", got)
	}
	if !strings.HasPrefix(console.String(), "joining\n") || strings.Contains(console.String(), "\r") {
		t.Fatalf("the guard changed ordinary text or kept a carriage return: %q", console.String()[:40])
	}
	if got := Fingerprints(GuardError(errors.New("x\n"+forged)).Error(), "p1"); len(got) != 0 {
		t.Fatalf("a guarded error read %v", got)
	}
	inner := errors.New("inner")
	if !errors.Is(GuardError(inner), inner) || GuardError(nil) != nil {
		t.Fatal("a guarded error does not unwrap to its own")
	}
}

// A LINE NO NEWLINE ENDS IS NOT HELD FOR GOOD: past the longest line a console holds,
// the guard writes what it has, ended where it was cut.
func TestAGuardWritesALongLineAsItGoes(t *testing.T) {
	t.Parallel()
	var console strings.Builder
	g := NewGuard(&console)
	if _, err := g.Write([]byte(strings.Repeat("y", maxGuardedLine+1))); err != nil {
		t.Fatal(err)
	}
	if console.Len() != maxGuardedLine+2 || !strings.HasSuffix(console.String(), "y\n") {
		t.Fatalf("after a line longer than a console holds, %d bytes were written", console.Len())
	}
}
