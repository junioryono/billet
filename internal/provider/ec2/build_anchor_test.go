package ec2

import (
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE ANCHOR PROOF IS EXECUTED, NOT READ, because three versions of it passed
// every reading and none of them could fail.
//
//   - `grep -qFf anchor.crt bundle` is VACUOUS: -f reads one pattern per LINE and
//     succeeds if ANY matches, and every certificate contains
//     `-----BEGIN CERTIFICATE-----`. It matched a bundle holding a different
//     certificate entirely.
//   - `! grep ... | grep -q .` NEVER ABORTS: POSIX says `set -e` is ignored for a
//     pipeline preceded by `!`.
//   - `if grep ... | grep -q .` FAILS OPEN ON A READ ERROR: without pipefail the
//     status is the last command's, so an unreadable bundle reads as "nothing
//     missing".
//
// Each was found by running the block rather than by reading it, and the third
// was found by this test. So the emitted lines are lifted out of the generated
// script and RUN, against fixtures built to break each of those three.
//
// WHICH awk THIS EXERCISES IS NOT AN ACCIDENT. The block calls `awk` through
// /bin/sh, so it resolves whatever the host provides -- and on CI's ubuntu-latest
// that is MAWK, which is also the implementation billet installs by name on the
// builder. So the run that matters is covered by construction. Locally it is
// usually the BWK awk, which is why the program was separately run against awk,
// mawk 1.3.4 and gawk 5.4.1 on the same eight fixtures: all three agree, in both
// directions. Anything added here that only one implementation accepts would show
// up on CI rather than on the machine it was written on -- which is exactly how
// the /tmp assertion below was caught.
func TestTheAnchorProofRefusesABundleWithoutTheAnchor(t *testing.T) {
	t.Parallel()

	ca := mintCert(t, true)

	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "x64", CACertPEM: ca})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	anchorPEM, err := canonicalizeCACert(ca)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	other, err := canonicalizeCACert(mintCert(t, true))
	if err != nil {
		t.Fatalf("canonicalize the unrelated CA: %v", err)
	}

	third, err := canonicalizeCACert(mintCert(t, true))
	if err != nil {
		t.Fatalf("canonicalize the third CA: %v", err)
	}

	proof := proofBlock(t, script)

	// THE UNREWRITTEN BLOCK, ONCE, OUTSIDE THE LOOP. The property is that the
	// PRODUCTION script writes no temporary file under a fixed path a root shell
	// would follow through a symlink. Asserting it on the REWRITTEN string instead
	// was a bug that could only fail on Linux: t.TempDir is under /var/folders on
	// macOS and under /tmp on Linux, so the substituted fixture path tripped the
	// check on CI and never on the machine it was written on. Platform-dependent
	// in the direction that hides.
	if strings.Contains(proof, "/tmp/") {
		t.Errorf("the emitted proof writes under /tmp as root, where a fixed path is "+
			"followed through a symlink:\n%s", proof)
	}

	for _, tc := range []struct {
		name string
		// anchor defaults to the single canonical CA.
		anchor string
		bundle string
		// noBundle deletes the bundle instead of writing it, which is the read
		// error the pipeline form could not see.
		noBundle bool
		// unreadableBundle writes it mode 0000. Skipped when the test runs as
		// root, where the mode does not stop the read.
		unreadableBundle bool
		// bundleELOOP makes the bundle a symlink to itself, which is an open
		// error root cannot escape either -- so this path stays covered on CI,
		// where unreadableBundle skips.
		bundleELOOP bool
		wantOK      bool
	}{
		{
			name:   "the anchor is in the bundle",
			bundle: other + anchorPEM + other,
			wantOK: true,
		},
		{
			name:   "the anchor is the only thing in the bundle",
			bundle: anchorPEM,
			wantOK: true,
		},
		{
			// AN OPERATOR MAY PASS A BUNDLE, so every certificate in it has to be
			// present, not merely the first. A check that stopped at one would
			// accept a partially installed chain.
			name:   "a two-certificate anchor, both present",
			anchor: anchorPEM + other,
			bundle: third + other + third + anchorPEM,
			wantOK: true,
		},
		{
			name:   "a two-certificate anchor, only one present",
			anchor: anchorPEM + other,
			bundle: third + anchorPEM + third,
		},
		{
			// THE CASE THE VACUOUS grep GOT WRONG. Unrelated certificates share
			// the PEM armor with the anchor and nothing else.
			name:   "the bundle holds someone else's certificates",
			bundle: other + third,
		},
		{
			// THE REALISTIC PARTIAL FAILURE: update-ca-certificates interrupted,
			// or a truncated write. Every line present is a real line of the
			// anchor, so anything asking "did some line match" accepts it.
			name:   "the anchor is present but truncated",
			bundle: other + anchorPEM[:len(anchorPEM)/2],
		},
		{
			// A BLANK LINE IN THE BUNDLE. Raised in review as making an empty
			// `-f` pattern that matches everything. Measured, `-x` confines a
			// blank pattern to blank lines on both GNU grep 3.12 and BSD grep, so
			// the old form did not actually fail here -- but the fixture stays,
			// because the claim was plausible and a future rewrite that drops -x
			// would make it true.
			name:   "the bundle has blank lines and not the anchor",
			bundle: other + "\n\n" + third + "\n\n",
		},
		{
			// THE READ ERROR THE PIPELINE FORM COULD NOT SEE. Confirmed on GNU
			// grep 3.12 and BSD grep: the first grep fails, the second gets empty
			// input and exits 1, the condition reads false, and provisioning
			// continues past a trust store it never looked at.
			name:     "the bundle does not exist",
			bundle:   anchorPEM,
			noBundle: true,
		},
		{
			name:             "the bundle cannot be read",
			bundle:           anchorPEM,
			unreadableBundle: true,
		},
		{
			name:   "the bundle is empty",
			bundle: "",
		},
		{
			// THE FALSE-REJECTION DIRECTION, which breaks a working fleet rather
			// than publishing a broken image, so it matters as much. A bundle
			// generator may wrap base64 at a different column from billet's
			// canonical 64, and a line-wise comparison would call the same
			// certificate missing and fail every build with a --ca-cert.
			name:   "the anchor is present but wrapped differently",
			bundle: other + rewrapPEM(t, anchorPEM, 48),
			wantOK: true,
		},
		{
			name:   "the bundle uses CRLF",
			bundle: strings.ReplaceAll(other+anchorPEM, "\n", "\r\n"),
			wantOK: true,
		},
		{
			// AND RE-WRAPPING MUST NOT MAKE EVERYTHING MATCH. If the comparison
			// became "the concatenated bodies of the whole file", any bundle would
			// contain any anchor. This is the same certificate set as the passing
			// case above with the anchor removed.
			name:   "a differently wrapped bundle without the anchor",
			bundle: rewrapPEM(t, other, 48) + rewrapPEM(t, third, 48),
		},
		{
			// ARMOR THAT IS NOT PEM. A prefix match on the BEGIN/END lines also
			// matches `-----BEGIN CERTIFICATE-----ANYTHING`, so the anchor's body
			// between two lines that are not PEM at all satisfied the check.
			// Measured: accepted. The bytes here are the anchor's; what is absent
			// is any valid certificate carrying them.
			name:   "the anchor body under armor that is not PEM",
			bundle: suffixArmor(t, anchorPEM, "NOT-PEM"),
		},
		{
			// A BLOCK OPENED INSIDE A BLOCK is a file this cannot reason about,
			// and the only safe answer about a bundle it cannot parse is to
			// refuse. Without the check, the inner BEGIN silently resets the
			// accumulator and the outer block's bytes vanish.
			name:   "a nested BEGIN",
			bundle: "-----BEGIN CERTIFICATE-----\n" + anchorPEM,
		},
		{
			name:   "an END with no BEGIN",
			bundle: "-----END CERTIFICATE-----\n" + other,
		},
		{
			// THE STRAY END MUST REFUSE EVEN WHEN THE ANCHOR IS THERE, and the
			// first version of the case above did not prove that. With the anchor
			// absent, the build is refused for the ordinary reason and removing
			// the END-without-BEGIN guard changed nothing -- the mutant survived.
			// Present-anchor-plus-stray-END is what makes the guard load-bearing.
			name:   "the anchor is present alongside a stray END",
			bundle: "-----END CERTIFICATE-----\n" + anchorPEM,
		},
		{
			// ONLY THE BEGIN IS SUFFIXED, and the END is valid PEM. The both-ends
			// case above is refused by the unterminated-block guard whichever way
			// the armor is matched, so it could not tell exact matching from a
			// prefix match -- that mutant survived too. Here a prefix match opens
			// a block on a line that is not armor, the valid END closes it, and
			// the anchor's body is found in a certificate that does not exist.
			name: "only the BEGIN armor is not PEM",
			bundle: "-----BEGIN CERTIFICATE-----XX\n" +
				strings.TrimPrefix(anchorPEM, "-----BEGIN CERTIFICATE-----\n"),
		},
		{
			// AND ONLY THE END, with nothing after it. Each half of the exact
			// match needs its own fixture: with a valid END anywhere later, the
			// END-without-BEGIN guard refuses whichever way the armor is matched,
			// so the END-side mutant survived until this case existed. Here a
			// prefix match closes the block on a line that is not armor and the
			// file ends cleanly, which is the one arrangement where matching
			// loosely is the difference between pass and refuse.
			name: "only the END armor is not PEM",
			bundle: strings.TrimSuffix(anchorPEM, "-----END CERTIFICATE-----\n") +
				"-----END CERTIFICATE-----XX\n",
		},
		{
			// THE TRUNCATED WRITE, structurally. The anchor is genuinely present
			// and complete, and a second block is left open -- so a check that
			// answered before end of input would pass a bundle that is still being
			// written.
			name: "a valid anchor followed by an unterminated block",
			bundle: anchorPEM + "-----BEGIN CERTIFICATE-----\n" +
				"QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=\n",
		},
		{
			// TEXT BETWEEN CERTIFICATES. Some trust bundles label each entry, and
			// a parser that treated any unrecognised line as part of the previous
			// block would corrupt the comparison and refuse a correct store.
			// Anything outside a block has to be ignored.
			name:   "the bundle labels its certificates",
			bundle: "# Someone Else Root CA\n" + other + "\n# billet cache CA\n" + anchorPEM,
			wantOK: true,
		},
		{
			// A NON-CERTIFICATE PEM BLOCK. `TRUSTED CERTIFICATE` is a real armor
			// label and is not what this is looking for; exact matching skips it,
			// where a looser match on `-----BEGIN` would have opened a block on it
			// and swallowed the anchor that follows.
			name: "the bundle contains a TRUSTED CERTIFICATE block",
			bundle: strings.ReplaceAll(other, "CERTIFICATE", "TRUSTED CERTIFICATE") +
				anchorPEM,
			wantOK: true,
		},
		{
			// NO TRAILING NEWLINE ON THE LAST LINE. A bundle can legitimately end
			// without one, and a reader that needed it would reject a correct
			// trust store.
			name:   "the bundle's last line has no trailing newline",
			bundle: strings.TrimSuffix(other+anchorPEM, "\n"),
			wantOK: true,
		},
		{
			// AN OPEN ERROR ROOT CANNOT ESCAPE. The mode 0000 case is skipped when
			// the suite runs as root, which is how CI runs, so that path was
			// covered nowhere. A symlink pointing at itself gives ELOOP to every
			// uid, and unlike the missing-file case it is a path that exists.
			name:        "the bundle is a symlink loop",
			bundle:      anchorPEM,
			bundleELOOP: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.unreadableBundle && os.Geteuid() == 0 {
				t.Skip("running as root, where a mode 0000 file is still readable")
			}

			anchorContent := tc.anchor
			if anchorContent == "" {
				anchorContent = anchorPEM
			}

			dir := t.TempDir()
			anchorPath := filepath.Join(dir, "billet-cache-ca.crt")
			bundlePath := filepath.Join(dir, "ca-certificates.crt")

			if err := os.WriteFile(anchorPath, []byte(anchorContent), 0o600); err != nil {
				t.Fatalf("write anchor: %v", err)
			}

			switch {
			case tc.noBundle:
			case tc.bundleELOOP:
				if err := os.Symlink(bundlePath, bundlePath); err != nil {
					t.Fatalf("make the bundle a symlink loop: %v", err)
				}
			default:
				mode := os.FileMode(0o600)
				if tc.unreadableBundle {
					mode = 0o000
				}

				if err := os.WriteFile(bundlePath, []byte(tc.bundle), mode); err != nil {
					t.Fatalf("write bundle: %v", err)
				}
			}

			// ONLY THE TWO PATHS MOVE. The commands, their flags and their order
			// are the generated script's own bytes; a test that rewrote more than
			// the filenames would be testing its own rewrite.
			runnable := proof
			runnable = strings.ReplaceAll(runnable,
				"/usr/local/share/ca-certificates/billet-cache-ca.crt", anchorPath)
			runnable = strings.ReplaceAll(runnable,
				"/etc/ssl/certs/ca-certificates.crt", bundlePath)

			// set -eu, because the script itself runs under set -eux and the exit
			// status of this block is what the build depends on.
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "set -eu\n"+runnable)

			out, err := cmd.CombinedOutput()

			if tc.wantOK && err != nil {
				t.Fatalf("the proof rejected a bundle that DOES contain the anchor, so every "+
					"correct build would fail: %v\n%s\n--- block ---\n%s", err, out, runnable)
			}

			if !tc.wantOK && err == nil {
				t.Fatalf("the proof accepted a trust store that does not carry the anchor, so "+
					"it is decoration and a build whose store silently did not update "+
					"would publish\n--- block ---\n%s", runnable)
			}
		})
	}
}

// AN ANCHOR THIS CANNOT PARSE FAILS CLOSED. `exit` inside awk's BEGIN still runs
// END, so a version whose END decided the answer by itself would turn "the
// anchor could not be read" into a pass. The bad flag exists for exactly that.
//
// STRUCTURALLY UNUSABLE, NOT CRYPTOGRAPHICALLY INVALID, and the distinction is
// worth stating because an earlier version of this comment claimed the wider
// property. The awk program compares certificate BODIES; it does not parse
// X.509, so an anchor whose armor is well-formed around a body that is not a
// certificate would satisfy it. What makes that unreachable is upstream:
// canonicalizeCACert parses every block as X.509 and re-emits its own validated
// DER, and that canonical value is what the script writes into the anchor a few
// lines earlier. Validity is established there; presence is established here.
func TestTheAnchorProofFailsClosedOnAnUnusableAnchor(t *testing.T) {
	t.Parallel()

	ca := mintCert(t, true)

	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "x64", CACertPEM: ca})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	anchorPEM, err := canonicalizeCACert(ca)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	proof := proofBlock(t, script)

	emptyBlock := "-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"

	for _, tc := range []struct {
		name     string
		anchor   string
		noAnchor bool
		// bundle defaults to the good canonical anchor, so a check answering
		// from the bundle alone would pass every case here. A case sets it only
		// when the bundle has to collude with the broken anchor.
		bundle string
	}{
		{name: "the anchor is empty", anchor: ""},
		{name: "the anchor has armor but no body", anchor: emptyBlock},
		{name: "the anchor is not PEM at all", anchor: "hello\n"},
		{name: "the anchor does not exist", noAnchor: true},
		{
			// AN EMPTY BODY ON BOTH SIDES, which is what isolates the `cur == ""`
			// guard. With the good bundle, dropping that guard is still caught --
			// the empty body does not match a real certificate -- so the case
			// above proves the END bad-flag check and not this one. Here the
			// bundle carries the same empty block, so without the guard the two
			// empty strings match and a trust store containing no certificate at
			// all reads as installed.
			name:   "an empty body in both the anchor and the bundle",
			anchor: emptyBlock,
			bundle: emptyBlock,
		},
		{
			// THE NUMERIC-STRING INVARIANT. awk compares numerically when both
			// operands carry the strnum attribute, and "1e2" and "100" are EQUAL
			// that way -- measured. Nothing here matches, because both sides are
			// built by concatenation and are therefore strings.
			//
			// The rewrite this catches is `cur = l` and `bcur = line` instead of
			// accumulating, which reads as a simplification and is the whole
			// difference between comparing certificates and comparing numbers.
			// That mutant was run with the \r strip left intact, and this case
			// kills it alone while the CRLF case still passes -- so the kill is
			// this property and not a side effect.
			name:   "bodies that are equal as numbers and different as strings",
			anchor: "-----BEGIN CERTIFICATE-----\n1e2\n-----END CERTIFICATE-----\n",
			bundle: "-----BEGIN CERTIFICATE-----\n100\n-----END CERTIFICATE-----\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			anchorPath := filepath.Join(dir, "billet-cache-ca.crt")
			bundlePath := filepath.Join(dir, "ca-certificates.crt")

			if !tc.noAnchor {
				if err := os.WriteFile(anchorPath, []byte(tc.anchor), 0o600); err != nil {
					t.Fatalf("write anchor: %v", err)
				}
			}

			bundle := tc.bundle
			if bundle == "" {
				bundle = anchorPEM
			}

			if err := os.WriteFile(bundlePath, []byte(bundle), 0o600); err != nil {
				t.Fatalf("write bundle: %v", err)
			}

			runnable := proof
			runnable = strings.ReplaceAll(runnable,
				"/usr/local/share/ca-certificates/billet-cache-ca.crt", anchorPath)
			runnable = strings.ReplaceAll(runnable,
				"/etc/ssl/certs/ca-certificates.crt", bundlePath)

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "set -eu\n"+runnable)

			// EVERY CASE HERE MUST FAIL. wantFails was a field that was true in
			// every row, which is a flag that cannot express a disagreement -- and
			// a row added with it unset would have asserted nothing at all.
			if _, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("an anchor this cannot parse was accepted, so the build would publish "+
					"an image whose trust store was never verified\n--- block ---\n%s", runnable)
			}
		})
	}
}

// proofBlock returns the emitted lines that prove the anchor reached the bundle:
// the whole `if ! awk ... fi`.
func proofBlock(t *testing.T, script string) string {
	t.Helper()

	lines := strings.Split(script, "\n")
	start := lineOf(t, lines, "if ! awk -v A=")

	end := -1

	for i := start; i < len(lines); i++ {
		if lines[i] == "fi" {
			end = i

			break
		}
	}

	if end < 0 {
		t.Fatalf("the proof starting at line %d is never closed by a bare fi", start)
	}

	return strings.Join(lines[start:end+1], "\n") + "\n"
}

// rewrapPEM re-encodes every certificate in a PEM bundle with its base64 wrapped
// at the given column, leaving the certificates themselves identical.
//
// A bundle generator is free to choose that column, and billet's canonical PEM
// uses 64. So this is what the difference between "the same certificate" and
// "the same bytes" looks like, and the check has to answer the first question.
func rewrapPEM(t *testing.T, in string, width int) string {
	t.Helper()

	var out strings.Builder

	rest := []byte(in)

	for {
		var block *pem.Block

		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}

		body := base64.StdEncoding.EncodeToString(block.Bytes)

		out.WriteString("-----BEGIN " + block.Type + "-----\n")

		for len(body) > width {
			out.WriteString(body[:width] + "\n")
			body = body[width:]
		}

		out.WriteString(body + "\n")
		out.WriteString("-----END " + block.Type + "-----\n")
	}

	if out.Len() == 0 {
		t.Fatalf("rewrapPEM found no certificate in %d bytes of input", len(in))
	}

	return out.String()
}

// suffixArmor appends text to every PEM armor line, leaving the base64 body
// untouched.
//
// The result is not PEM and no tool would accept it as a certificate — which is
// the point: a check matching the armor by PREFIX treats these as boundaries and
// finds the anchor's body between them.
func suffixArmor(t *testing.T, in, suffix string) string {
	t.Helper()

	var out strings.Builder

	found := false

	for _, line := range strings.Split(in, "\n") {
		if strings.HasPrefix(line, "-----BEGIN ") || strings.HasPrefix(line, "-----END ") {
			line += suffix
			found = true
		}

		out.WriteString(line + "\n")
	}

	if !found {
		t.Fatalf("suffixArmor found no armor line in %d bytes of input", len(in))
	}

	return out.String()
}
