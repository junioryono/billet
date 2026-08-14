package firecracker

import (
	"strconv"
	"strings"
)

// maxDiagnostic bounds how much of another program's output reaches a terminal.
const maxDiagnostic = 300

// bounded renders a value that came from somewhere else, capped and quoted.
//
// THREE SOURCES REACH IT and none of them is billet's own text: the jailer's
// stderr, a VMM's json fault message, and an instance name read back off a socket.
// Parsing something does not make its contents safe to print — the ceph client
// carries the same rule for the same reason — and an unbounded one lands in an
// error string, a log line and eventually an operator's terminal.
//
// QUOTED, so a control byte in another program's output cannot become a live
// terminal control, and the CAP IS ON THE RENDERED LENGTH: quoting expands, so 300
// NUL bytes become 1,200 characters of `\x00` and a cap on the input would leave
// the output four times the bound it was supposed to have.
func bounded(v string) string {
	clean := strings.ToValidUTF8(v, "")
	quoted := strconv.Quote(clean)

	if len(quoted) <= maxDiagnostic {
		return quoted
	}

	// THE LARGEST PREFIX WHOSE QUOTED FORM FITS, assembled escape by escape rather
	// than sliced out of the finished string. Slicing cuts inside an escape — 100
	// NUL bytes truncate to `"\x00\x00…\x0` — and trimming a trailing backslash
	// only repairs the subset where the cut happened to land on one.
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range clean {
		piece := strconv.Quote(string(r))
		piece = piece[1 : len(piece)-1]

		if b.Len()+len(piece)+len("…\"") > maxDiagnostic {
			break
		}

		b.WriteString(piece)
	}

	b.WriteString("…\"")

	return b.String()
}
