package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A SUMS FILE IS READ AS TEXT, WHATEVER ENCODING THE VENDOR SHIPPED.
//
// PowerShell publishes hashes.sha256 as UTF-16LE with a BOM and CRLF line
// endings — measured with `file`: "Unicode text, UTF-16, little-endian, with CRLF
// line terminators". Bash strips the nulls out of a command substitution and warns
// while doing it, and what survives carries a BOM and a trailing CR on every line,
// so a comparison against a filename matches nothing. A real build refused a
// checksum the vendor had published.
//
// THE LOCAL PROBE SAID IT WAS FINE. The development shell's grep is ugrep, which
// reads UTF-16 transparently, so `curl | grep` printed the lines exactly as
// expected while the builder's GNU tools could not. That is why this asserts on
// bytes rather than on a pipeline that happened to work somewhere.
func TestASumsFileIsReadWhateverItsEncoding(t *testing.T) {
	t.Parallel()

	const digest = "b34ab3b19acac1d3d4d0d3cfdb02acf62f457b0b6a962ff008132033f7566844"
	const name = "powershell-7.6.5-linux-x64.tar.gz"

	line := digest + " *" + name

	for _, tc := range []struct {
		name  string
		bytes []byte
	}{
		{
			// WHAT POWERSHELL ACTUALLY SHIPS. The BOM and the CRLF are both part of
			// the failure: the nulls defeat the read, and the carriage return
			// defeats the comparison even after they are gone.
			name:  "utf-16le with a bom and crlf",
			bytes: utf16LE("\ufeff" + line + "\r\n"),
		},
		{
			// AND THE ORDINARY CASE MUST STILL WORK. The first version of the fix
			// sent every file through iconv, because bash cannot hold a NUL byte
			// and so a grep for one matched everything -- iconv then refused a
			// plain ASCII file with "incomplete character or shift sequence".
			name:  "plain ascii with unix endings",
			bytes: []byte(line + "\n"),
		},
		{
			name:  "plain ascii with crlf",
			bytes: []byte(line + "\r\n"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			sums := filepath.Join(dir, "sums")

			if err := os.WriteFile(sums, tc.bytes, 0o600); err != nil {
				t.Fatalf("write the sums fixture: %v", err)
			}

			body := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				guestImageFunction(t, "billet_tc_text") + "\n" +
				guestImageFunction(t, "billet_tc_sum") + "\n" +
				"billet_tc_sum \"$(billet_tc_text \"$1\")\" \"$2\"\n"

			script := filepath.Join(dir, "run.sh")
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatalf("write the harness: %v", err)
			}

			out, err := exec.CommandContext(t.Context(), "bash", script, sums, name).
				CombinedOutput()
			if err != nil {
				t.Fatalf("reading the sums file: %v\n%s", err, out)
			}

			if got := strings.TrimSpace(string(out)); got != digest {
				t.Errorf("the digest read back is %q, want %q — a build refuses a checksum "+
					"the vendor published and installs nothing", got, digest)
			}
		})
	}
}

// utf16LE encodes ASCII text the way a Windows tool writes it.
func utf16LE(s string) []byte {
	out := make([]byte, 0, len(s)*2)

	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}

	return out
}
