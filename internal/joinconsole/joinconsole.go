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
	"regexp"
	"strings"
)

// prefix begins the line.
const prefix = "billet-pool-join "

// Line is the line a pool host writes for its order and its key's fingerprint.
func Line(order, fingerprint string) string {
	return prefix + "order=" + order + " fingerprint=" + fingerprint
}

// wholeLine is one whole line: a fingerprint is SHA-256 in unpadded base64 (43
// characters, wirecert.Fingerprint), and the line ends there. A line the console cut
// short, or that another write ran into, matches nothing, so it is never read as a
// key: it says nothing either way.
var wholeLine = regexp.MustCompile(
	`billet-pool-join order=([a-z0-9][a-z0-9-]*) fingerprint=(SHA256:[A-Za-z0-9+/]{43})(?:\r|\n|$)`)

// Fingerprints is every distinct fingerprint a console names for order, in the order
// first written. A console carries a boot's whole output, terminal escapes and carriage
// returns included, and is read as text: a line is matched wherever it sits, and only
// whole.
func Fingerprints(console, order string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range wholeLine.FindAllStringSubmatch(console, -1) {
		if m[1] != order {
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
