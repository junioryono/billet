package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/state"
)

// recordFixture is a manifest, an archive it names, and a binary.
type recordFixture struct {
	manifest string
	archive  string
	binary   string
}

// recordFixtureBody is what every fixture's binary and archive member contain.
// The contents decide nothing these tests assert — what matters is that the
// manifest, the archive and the binary agree — so one value serves all of them.
const recordFixtureBody = "the release archive"

// buildRecordFixture writes a manifest that genuinely names the archive beside it.
func buildRecordFixture(t *testing.T) recordFixture {
	t.Helper()

	archiveBody := recordFixtureBody

	dir := t.TempDir()

	binary := filepath.Join(dir, "billet")
	if err := os.WriteFile(binary, []byte(archiveBody), 0o755); err != nil {
		t.Fatalf("write the binary: %v", err)
	}

	// A REAL ARCHIVE CARRYING THAT BINARY, because the command checks the link
	// rather than assuming it. A fixture that wrote arbitrary bytes and called them
	// an archive would exercise the manifest comparison and nothing else.
	archive := filepath.Join(dir, "billet_0.4.0_"+hostOS+"_"+runtime.GOARCH+".tar.gz")
	writeBilletArchive(t, archive, archiveBody)

	sum, err := provenance.HashFile(archive)
	if err != nil {
		t.Fatalf("hash the archive: %v", err)
	}

	body, err := json.Marshal(releasesource.Manifest{
		Schema:        releasesource.SchemaVersion,
		Version:       "v0.4.0",
		Commit:        strings.Repeat("c", 40),
		BuiltAt:       time.Now().UTC(),
		Wire:          releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
		LedgerSchema:  state.LatestSchemaVersion(),
		GuestContract: firecracker.GuestContract,
		Actions:       "v0.4.0",
		Artifacts: []releasesource.Artifact{{
			Name: filepath.Base(archive), OS: hostOS, Arch: runtime.GOARCH,
			Kind: releasesource.KindArchive, SHA256: sum, Size: archiveSize(t, archive),
		}},
	})
	if err != nil {
		t.Fatalf("render the manifest: %v", err)
	}

	manifest := filepath.Join(dir, "release-manifest.json")
	if err := os.WriteFile(manifest, body, 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}

	original := provenance.Path
	provenance.Path = filepath.Join(dir, "installed.json")

	t.Cleanup(func() { provenance.Path = original })

	return recordFixture{manifest: manifest, archive: archive, binary: binary}
}

func (f recordFixture) args() []string {
	return []string{"--manifest", f.manifest, "--archive", f.archive, "--binary", f.binary}
}

// A RECORD IS WRITTEN ONLY WHEN THE MANIFEST NAMES THESE BYTES.
//
// The record is an ATTESTATION: a rollout treats it as proof and blocks a host
// whose manifest disagrees with the decision. So it may only be written once the
// chain holds — the manifest names this archive and its hash, the archive hashes
// to that, and the binary came out of that archive.
func TestReleaseRecordWritesWhatTheManifestNames(t *testing.T) {
	f := buildRecordFixture(t)

	if err := cmdReleaseRecord(t.Context(), f.args()); err != nil {
		t.Fatalf("recording an install the manifest names: %v", err)
	}

	record, err := provenance.Read()
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	wantDigest, err := provenance.HashFile(f.manifest)
	if err != nil {
		t.Fatalf("hash the manifest: %v", err)
	}

	if record.ManifestDigest != wantDigest {
		t.Errorf("recorded manifest %q, want %q", record.ManifestDigest, wantDigest)
	}

	wantBinary, err := provenance.HashFile(f.binary)
	if err != nil {
		t.Fatalf("hash the binary: %v", err)
	}

	if record.BinarySHA256 != wantBinary {
		t.Errorf("recorded binary %q, want %q", record.BinarySHA256, wantBinary)
	}

	if record.Version != "v0.4.0" {
		t.Errorf("recorded version %q, want v0.4.0", record.Version)
	}
}

// AN ARCHIVE THE MANIFEST DOES NOT NAME IS REFUSED, AND NOTHING IS WRITTEN.
//
// THIS IS THE ATTACK THE COMMAND EXISTS FOR. The installer verifies its download
// against a checksums file fetched from the same place as the manifest, so a
// manifest served beside a DIFFERENT archive would otherwise produce a host that
// converges a rollout as proved on bytes that manifest never named. A record is
// worse than no record when it is wrong: the rollout reads an absence correctly
// and reads a wrong digest as certainty.
func TestReleaseRecordRefusesAnArchiveTheManifestDoesNotName(t *testing.T) {
	f := buildRecordFixture(t)

	// THE SAME NAME, THE SAME LENGTH, AND STILL A VALID ARCHIVE, so only the file's
	// hash can catch it.
	//
	// TWO EARLIER SHAPES OF THIS FIXTURE MEASURED SOMETHING ELSE. A substitute of a
	// different size is refused by the size check before the hash is consulted; and
	// flipping any byte of the compressed body or its footer is now caught by gzip's
	// own CRC, which is verified when the stream is drained — so it too refused for a
	// reason that is not this one. What is left is the gzip header's MTIME: four
	// bytes that change the FILE's sha256 while leaving its length, its checksum and
	// every byte it decompresses to exactly as they were.
	original, err := os.ReadFile(f.archive)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}

	substitute := make([]byte, len(original))
	copy(substitute, original)

	for i := 4; i < 8; i++ {
		substitute[i] ^= 0xff
	}

	if err := os.WriteFile(f.archive, substitute, 0o600); err != nil {
		t.Fatalf("substitute the archive: %v", err)
	}

	err = cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive the manifest does not name was recorded as its product")
	}

	if !strings.Contains(err.Error(), "did not produce this download") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}

	if _, statErr := os.Stat(provenance.Path); !os.IsNotExist(statErr) {
		t.Errorf("a refused attestation was written anyway: %v", statErr)
	}
}

// A DOCUMENT THAT IS NOT A MANIFEST BILLET CAN ACT ON IS REFUSED.
//
// Parsed by the same reader every other path uses, so anything refused elsewhere
// is refused here — a record derived from a document billet cannot read would
// attest to something nothing can check.
func TestReleaseRecordRefusesADocumentItCannotRead(t *testing.T) {
	f := buildRecordFixture(t)

	if err := os.WriteFile(f.manifest, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatalf("damage the manifest: %v", err)
	}

	if err := cmdReleaseRecord(t.Context(), f.args()); err == nil {
		t.Fatal("a document that is not a usable manifest was accepted")
	}

	if _, statErr := os.Stat(provenance.Path); !os.IsNotExist(statErr) {
		t.Errorf("a refused attestation was written anyway: %v", statErr)
	}
}

// AND A MANIFEST THAT PUBLISHES NOTHING FOR THIS MACHINE CANNOT HAVE PRODUCED IT.
func TestReleaseRecordRefusesAManifestForAnotherPlatform(t *testing.T) {
	f := buildRecordFixture(t)

	body, err := os.ReadFile(f.manifest)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	// Rewrite the one artifact so it belongs to a platform this is not.
	swapped := strings.Replace(string(body), `"arch":"`+runtime.GOARCH+`"`,
		`"arch":"somewhere-else"`, 1)

	if err := os.WriteFile(f.manifest, []byte(swapped), 0o600); err != nil {
		t.Fatalf("rewrite the manifest: %v", err)
	}

	if err := cmdReleaseRecord(t.Context(), f.args()); err == nil {
		t.Fatal("a manifest publishing nothing for this machine was accepted as what " +
			"produced its binary")
	}
}

// writeBilletArchive writes a tar.gz carrying one member called billet.
func writeBilletArchive(t *testing.T, path, contents string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the archive: %v", err)
	}

	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: "billet", Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write the archive header: %v", err)
	}

	if _, err := tw.Write([]byte(contents)); err != nil {
		t.Fatalf("write the archive member: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close the gzip: %v", err)
	}
}

func archiveSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the archive: %v", err)
	}

	return info.Size()
}

// A BINARY THAT DID NOT COME OUT OF THE ARCHIVE IS REFUSED.
//
// THE LINK WAS ASSUMED, NOT CHECKED, in the first version — because its one
// caller happened to extract the binary from the archive it passed. An assumption
// is not a chain: the three paths arrive independently, so a caller passing some
// other billet (a stale one already at the destination, a build from elsewhere)
// would have produced a record attesting that those bytes came from a manifest
// which says nothing about them. That is the defect this command exists to
// prevent, one layer down.
func TestReleaseRecordRefusesABinaryTheArchiveDoesNotCarry(t *testing.T) {
	f := buildRecordFixture(t)

	// The archive and the manifest still agree; the binary is somebody else's.
	if err := os.WriteFile(f.binary, []byte("a different build entirely"), 0o755); err != nil {
		t.Fatalf("substitute the binary: %v", err)
	}

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("a binary the archive does not carry was recorded as its product")
	}

	if !strings.Contains(err.Error(), "did not come out of this archive") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}

	if _, statErr := os.Stat(provenance.Path); !os.IsNotExist(statErr) {
		t.Errorf("a refused attestation was written anyway: %v", statErr)
	}
}

// AN ARCHIVE CARRYING NO BILLET CANNOT HAVE PRODUCED ONE.
func TestReleaseRecordRefusesAnArchiveWithNoBillet(t *testing.T) {
	f := buildRecordFixture(t)

	// Rebuild the archive around a differently named member, and re-point the
	// manifest at it so only the missing member is under test.
	empty := filepath.Join(filepath.Dir(f.archive), "empty.tar.gz")
	writeNamedArchive(t, empty, "not-billet", "something else")

	if err := os.Rename(empty, f.archive); err != nil {
		t.Fatalf("replace the archive: %v", err)
	}

	repointManifest(t, f)

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive carrying no billet was accepted as what produced one")
	}

	if !strings.Contains(err.Error(), "carries no billet") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// writeNamedArchive writes a tar.gz whose single member has the given name.
func writeNamedArchive(t *testing.T, path, member, contents string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the archive: %v", err)
	}

	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: member, Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write the archive header: %v", err)
	}

	if _, err := tw.Write([]byte(contents)); err != nil {
		t.Fatalf("write the archive member: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close the gzip: %v", err)
	}
}

// repointManifest rewrites the fixture's manifest so it names the archive as it
// now stands, leaving only the property under test failing.
func repointManifest(t *testing.T, f recordFixture) {
	t.Helper()

	sum, err := provenance.HashFile(f.archive)
	if err != nil {
		t.Fatalf("hash the archive: %v", err)
	}

	body, err := os.ReadFile(f.manifest)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	var m releasesource.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse the manifest: %v", err)
	}

	m.Artifacts[0].SHA256 = sum
	m.Artifacts[0].Size = archiveSize(t, f.archive)

	rewritten, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("render the manifest: %v", err)
	}

	if err := os.WriteFile(f.manifest, rewritten, 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}

// AN ARCHIVE WHOSE BILLET IS A LINK CARRIES NO BILLET.
//
// A link resolves to whatever the extracting side already had, which is not a
// member of this archive at all — so treating one as the member would attest that
// bytes chosen by the machine came from a manifest that says nothing about them.
func TestReleaseRecordRefusesAnArchiveWhoseBilletIsALink(t *testing.T) {
	f := buildRecordFixture(t)

	linked := filepath.Join(filepath.Dir(f.archive), "linked.tar.gz")
	writeLinkArchive(t, linked)

	if err := os.Rename(linked, f.archive); err != nil {
		t.Fatalf("replace the archive: %v", err)
	}

	repointManifest(t, f)

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive whose billet is a link was accepted as carrying one")
	}

	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// writeLinkArchive writes a tar.gz whose only billet is a symlink.
func writeLinkArchive(t *testing.T, path string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the archive: %v", err)
	}

	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: "billet", Linkname: "/usr/bin/billet", Mode: 0o777,
		Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatalf("write the link header: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close the gzip: %v", err)
	}
}

// AN EMPTY BINARY IS NOT SOMETHING TO RECORD.
//
// Two empty files hash alike, so an empty member and an empty binary agree with
// each other — the comparison that catches every other substitution passes, and
// what refuses this is that there is nothing there to describe. Recording it
// would name a manifest for bytes that are not a program.
func TestReleaseRecordRefusesAnEmptyBinary(t *testing.T) {
	f := buildRecordFixture(t)

	empty := filepath.Join(filepath.Dir(f.archive), "empty-member.tar.gz")
	writeNamedArchive(t, empty, "billet", "")

	if err := os.Rename(empty, f.archive); err != nil {
		t.Fatalf("replace the archive: %v", err)
	}

	repointManifest(t, f)

	if err := os.WriteFile(f.binary, []byte{}, 0o755); err != nil {
		t.Fatalf("empty the binary: %v", err)
	}

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an empty billet was recorded as what a manifest produced")
	}

	if _, statErr := os.Stat(provenance.Path); !os.IsNotExist(statErr) {
		t.Errorf("a refused attestation was written anyway: %v", statErr)
	}
}

// AN OVERSIZED MEMBER IS REFUSED RATHER THAN TRUNCATED AND HASHED.
//
// Reading through a limit and hashing whatever came out compares against bytes
// that were never in the archive — a comparison that cannot succeed, reported as
// though the archive simply did not match. The declared size is what says so.
//
// THE HEADER IS WHAT IS CHECKED, so this costs nothing to test: a tar header can
// declare a size without the archive carrying it, which is also exactly how a
// hostile archive would try to spend this command's time.
func TestReleaseRecordRefusesAMemberLargerThanItWillRead(t *testing.T) {
	f := buildRecordFixture(t)

	oversized := filepath.Join(filepath.Dir(f.archive), "oversized.tar.gz")
	writeOversizedArchive(t, oversized)

	if err := os.Rename(oversized, f.archive); err != nil {
		t.Fatalf("replace the archive: %v", err)
	}

	repointManifest(t, f)

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("a member larger than billet will read was accepted")
	}

	if !strings.Contains(err.Error(), "billet will read") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// writeOversizedArchive writes a tar.gz whose billet DECLARES more than the
// command will read. The bytes are not there; the header is what is refused.
func writeOversizedArchive(t *testing.T, path string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the archive: %v", err)
	}

	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: "billet", Mode: 0o755, Size: maxMemberBytes + 1, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write the header: %v", err)
	}

	// The declared size is never written, and the refusal happens before anything
	// tries to read it — which is the point.
	_ = tw.Flush()
	_ = gz.Close()
}

// A MANIFEST WHOSE OWN TWO FACTS DISAGREE IS NOT ONE TO ATTEST FROM.
//
// The size check is not a second opinion about the bytes — the hash settles
// those, and equal bytes have equal lengths. What it catches is a manifest that
// contradicts ITSELF about one artifact, which is a document billet cannot treat
// as self-consistent and therefore cannot derive an attestation from. Removing it
// on the "equal bytes" reasoning was answering a different question.
func TestReleaseRecordRefusesAManifestThatContradictsItself(t *testing.T) {
	f := buildRecordFixture(t)

	body, err := os.ReadFile(f.manifest)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	var m releasesource.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse the manifest: %v", err)
	}

	// The hash still describes the archive; the size no longer does.
	m.Artifacts[0].Size++

	rewritten, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("render the manifest: %v", err)
	}

	if err := os.WriteFile(f.manifest, rewritten, 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}

	err = cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("a manifest whose size and hash describe different files was accepted")
	}

	if !strings.Contains(err.Error(), "does not describe this download") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// NOTHING THIS COMMAND READS MAY MAKE IT WAIT.
//
// Opening a FIFO read-only BLOCKS until somebody writes to it, so a check placed
// after the open never runs and one placed before it leaves the hang in front of
// the guard. An earlier version opened and then stat-ed, and its comment claimed
// a FIFO was refused. This runs at the end of an install, where hanging is
// indistinguishable from working.
func TestReleaseRecordRefusesAPathThatWouldMakeItWait(t *testing.T) {
	f := buildRecordFixture(t)

	fifo := filepath.Join(filepath.Dir(f.archive), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("this platform will not make a fifo: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"as the manifest", []string{"--manifest", fifo, "--archive", f.archive,
			"--binary", f.binary}},
		{"as the archive", []string{"--manifest", f.manifest, "--archive", fifo,
			"--binary", f.binary}},
		{"as the binary", []string{"--manifest", f.manifest, "--archive", f.archive,
			"--binary", fifo}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A BOUNDED WAIT IS THE ASSERTION. If the open blocks, this never returns,
			// and a test that hangs is the failure being described.
			done := make(chan error, 1)

			go func() { done <- cmdReleaseRecord(t.Context(), tc.args) }()

			select {
			case err := <-done:
				if err == nil {
					t.Error("a fifo was read as an ordinary file")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the command blocked on a fifo rather than refusing it")
			}
		})
	}
}
