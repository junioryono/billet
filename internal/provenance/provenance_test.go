package provenance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withRecordPath points the record at a directory this test owns, and clears the
// per-process cache so each test computes its own answer.
func withRecordPath(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	originalPath, originalExe := Path, executable

	Path = filepath.Join(dir, "installed.json")

	// THE REAL COMPARISON, AGAINST A FILE THIS TEST OWNS. Without this seam every
	// case here would hash the test binary, which has no record, and they would
	// all look the same however readInstalled behaved.
	executable = func() (string, error) { return filepath.Join(dir, "billet"), nil }

	t.Cleanup(func() { Path, executable = originalPath, originalExe })

	return dir
}

// fakeBinary writes a file to stand in for an installed billet and returns its
// hash. The path is fixed by the directory, which is what the executable seam is
// pointed at, so returning it as well was flexibility no caller wanted.
func fakeBinary(t *testing.T, dir, contents string) string {
	t.Helper()

	path := filepath.Join(dir, "billet")
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write the fake binary: %v", err)
	}

	sum, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	return sum
}

// A RECORD THAT DESCRIBES THE RUNNING BINARY REPORTS ITS MANIFEST.
//
// This is the whole point: a rollout can compare what a host installed against
// the manifest its own decision named, instead of comparing two version strings
// that a moved tag makes identical.
func TestARecordThatMatchesTheBinaryReportsItsManifest(t *testing.T) {
	dir := withRecordPath(t)

	sum := fakeBinary(t, dir, "the installed bytes")

	want := strings.Repeat("a", 64)

	if err := Write(Record{
		Version: "v0.4.0", ManifestDigest: want, BinarySHA256: sum,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := readInstalled()
	if err != nil {
		t.Fatalf("reading a record that matches: %v", err)
	}

	if got != want {
		t.Errorf("the record reported %q, want %q", got, want)
	}
}

// A BINARY REPLACED BY HAND REPORTS NOTHING, RATHER THAN THE LAST UPGRADE'S
// PROVENANCE.
//
// THIS IS THE CASE THE HASH EXISTS FOR. Two builds can carry one version string
// — that is exactly what a moved tag produces — so a record bound only to a
// version would let a rollout read somebody else's bytes as proof of its own
// decision. Reporting nothing is strictly better: the rollout already knows how
// to read "cannot tell", and it reads a wrong digest as certainty.
func TestABinaryReplacedAfterTheRecordReportsNothing(t *testing.T) {
	dir := withRecordPath(t)

	sum := fakeBinary(t, dir, "the installed bytes")

	if err := Write(Record{
		Version: "v0.4.0", ManifestDigest: strings.Repeat("a", 64), BinarySHA256: sum,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Somebody drops a different build in place, leaving the record behind.
	fakeBinary(t, dir, "different bytes entirely")

	got, err := readInstalled()
	if !errors.Is(err, ErrNotThisBinary) {
		t.Fatalf("a replaced binary returned %v, want ErrNotThisBinary", err)
	}

	if got != "" {
		t.Errorf("a replaced binary still reported the digest %q", got)
	}

	// AND IT SAYS SO SPECIFICALLY. An operator reading this needs to know the
	// record is stale rather than missing; the two lead to different machines
	// being looked at.
	if !strings.Contains(err.Error(), "different bytes") {
		t.Errorf("the failure does not say the record is stale: %v", err)
	}
}

// A MACHINE WITH NO RECORD IS THE ORDINARY CASE, AND IT IS NOT A FAULT.
//
// Every host installed from a package, built from source, or upgraded before
// this existed has none — which on the day this ships is the entire fleet,
// including the hosts that would deliver the build that can report one.
func TestAMachineWithNoRecordSaysSoWithoutFailing(t *testing.T) {
	dir := withRecordPath(t)

	fakeBinary(t, dir, "some binary")

	got, err := readInstalled()
	if !errors.Is(err, ErrNoRecord) {
		t.Fatalf("a machine with no record returned %v, want ErrNoRecord", err)
	}

	if got != "" {
		t.Errorf("a machine with no record reported the digest %q", got)
	}
}

// A RECORD WITHOUT THE HASH IS REFUSED AT THE POINT OF WRITING.
//
// Writing one would put a file on the machine that every reader has to
// distrust, which quietly turns "no record" into "a record that means nothing".
func TestARecordWithoutTheBinaryHashIsRefused(t *testing.T) {
	withRecordPath(t)

	err := Write(Record{Version: "v0.4.0", ManifestDigest: strings.Repeat("a", 64)})
	if err == nil {
		t.Fatal("a record with no binary hash was written")
	}

	if _, statErr := os.Stat(Path); !os.IsNotExist(statErr) {
		t.Errorf("the refused record was written anyway: %v", statErr)
	}
}

// A RECORD BILLET CANNOT PARSE IS AN ERROR, NOT AN ABSENCE.
//
// "Nothing produced this installation" and "something is wrong with the file
// that says what did" are different facts, and answering the second with the
// first hides a machine whose provenance has been damaged.
func TestAnUnreadableRecordIsNotReportedAsAbsent(t *testing.T) {
	withRecordPath(t)

	if err := os.WriteFile(Path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write a damaged record: %v", err)
	}

	_, err := Read()
	if err == nil {
		t.Fatal("a damaged record was read as valid")
	}

	if errors.Is(err, ErrNoRecord) {
		t.Error("a damaged record was reported as no record at all, which hides it")
	}
}

// A RECORD MISSING EITHER HALF CANNOT SHOW ANYTHING, and is reported rather than
// half-believed.
func TestARecordMissingEitherHalfIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no manifest digest", `{"version":"v0.4.0","binary_sha256":"abc"}`},
		{"no binary hash", `{"version":"v0.4.0","manifest_digest":"abc"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRecordPath(t)

			if err := os.WriteFile(Path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write the record: %v", err)
			}

			if _, err := Read(); err == nil {
				t.Errorf("a record with %s was accepted", tc.name)
			}
		})
	}
}
