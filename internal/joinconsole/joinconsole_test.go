package joinconsole

import (
	"slices"
	"strings"
	"testing"
)

// fp is a fingerprint of the shape wirecert writes, distinct per seed.
func fp(seed string) string { return "SHA256:" + (seed + strings.Repeat("A", 43))[:43] }

// THE CONSOLE LINE A HOST WRITES IS THE ONE THE PLANE READS: wherever it sits in a
// boot's output, between terminal escapes and carriage returns, each fingerprint is
// read once, in the order written, and only for the order asked about. A line that
// names an order only as a prefix of another's is not that order's.
func TestAPoolConsoleLineIsReadBackForItsOrderOnly(t *testing.T) {
	t.Parallel()
	a, b, c := fp("aB+/c9"), fp("second"), fp("other")
	console := "\x1b[32m[  OK  ]\x1b[0m Started cloud-final.\r\n" +
		Line("p1", a) + "\r\n" +
		"[  12.0] " + Line("p12", c) + "\n" +
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
