package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/releasesource"
)

// A BINARY TOO LARGE TO BE A MEMBER IS REFUSED BEFORE IT IS HASHED.
//
// The member this is compared against is refused above the same bound, so a
// larger binary cannot possibly be the one inside the archive — and hashing it
// first only spends the time to reach an answer already known. The bound is a
// var precisely so this can be reached without writing half a gigabyte.
func TestReleaseRecordRefusesABinaryTooLargeToBeAMember(t *testing.T) {
	f := buildRecordFixture(t)

	original := maxMemberBytes
	maxMemberBytes = 4

	t.Cleanup(func() { maxMemberBytes = original })

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("a binary larger than any member could be was recorded")
	}

	if !strings.Contains(err.Error(), "cannot be a member") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	if _, statErr := os.Stat(provenance.Path); statErr == nil {
		t.Error("a record was written for a binary the archive cannot contain")
	}
}

// NOTHING MAY FOLLOW THE GZIP STREAM, and that is what makes the digest a fact
// about the whole file.
//
// A gzip.Reader is MULTISTREAM BY DEFAULT and tar stops at the first
// end-of-archive marker, so a file made by concatenating two archives carries a
// second `billet` that the duplicate refusal never sees — while the manifest
// legitimately names the whole thing, because the manifest describes the file. The
// invariant this command holds is that the archive names ONE binary; two, in a
// shape different extractors resolve differently, is exactly the choice a record
// must not silently describe.
//
// THIS ALSO REPLACED A TEST THAT ASSERTED THE OPPOSITE. An earlier version
// appended bytes, rewrote the manifest to describe them, and asserted the archive
// was ACCEPTED — which was how the digest was made to cover the trailing bytes.
// Refusing them is the stronger answer: if nothing may follow the stream, then the
// file IS the stream, and reading all of it is what the size accounting proves.
func TestReleaseRecordRefusesAnythingAfterTheGzipStream(t *testing.T) {
	f := buildRecordFixture(t)

	appendTo(t, f.archive, strings.Repeat("bytes past the end of the gzip stream. ", 4096))
	describeArchive(t, f)

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive with bytes after its gzip stream was accepted, so a second " +
			"member named billet can hide behind the first stream")
	}

	if _, statErr := os.Stat(provenance.Path); statErr == nil {
		t.Error("a record was written for an archive that carries more than one stream")
	}
}

// AND A SECOND WHOLE ARCHIVE CONCATENATED ONTO THE FIRST IS THE REAL SHAPE OF IT.
//
// Trailing junk is refused by anything that looks; a second VALID gzip stream is
// what a reader with multistream left on would decode straight through, and what
// `tar -i` would extract a different billet out of.
func TestReleaseRecordRefusesASecondConcatenatedArchive(t *testing.T) {
	f := buildRecordFixture(t)

	second := filepath.Join(t.TempDir(), "second.tar.gz")
	writeBilletArchive(t, second, "a different billet entirely")

	body, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read the second archive: %v", err)
	}

	appendTo(t, f.archive, string(body))
	describeArchive(t, f)

	err = cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("two concatenated archives were accepted as one, so which billet a host " +
			"installs depends on how it unpacks the file")
	}

	if !strings.Contains(err.Error(), "more than one gzip stream") {
		t.Errorf("the refusal does not name what is wrong: %v", err)
	}

	if _, statErr := os.Stat(provenance.Path); statErr == nil {
		t.Error("a record was written for a file carrying two archives")
	}
}

// describeArchive rewrites the fixture's manifest so it names the archive as it
// now stands, rather than as it was when the fixture built it.
//
// WITHOUT THIS, EVERY TEST ABOVE WOULD REFUSE ON THE SIZE and prove nothing about
// what it is named for: appending changes the length, and the length is checked
// before anything is read.
func describeArchive(t *testing.T, f recordFixture) {
	t.Helper()

	body, err := os.ReadFile(f.manifest)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	var m releasesource.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse the manifest: %v", err)
	}

	sum, err := provenance.HashFile(f.archive)
	if err != nil {
		t.Fatalf("hash the archive: %v", err)
	}

	for i := range m.Artifacts {
		m.Artifacts[i].SHA256 = sum
		m.Artifacts[i].Size = archiveSize(t, f.archive)
	}

	rewritten, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("render the manifest: %v", err)
	}

	if err := os.WriteFile(f.manifest, rewritten, 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}

func appendTo(t *testing.T, path, extra string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the archive to append: %v", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(extra); err != nil {
		t.Fatalf("append to the archive: %v", err)
	}
}

// AN ARCHIVE THAT DECOMPRESSES PAST THE BOUND IS REFUSED, NOT TRUNCATED.
//
// A LimitReader that runs out looks to tar exactly like a clean end of archive,
// so the walk would accept a truncated read of an oversized archive as a complete
// one that happened to contain billet — and write an attestation derived from a
// prefix of a file nobody bounded. Counting the decompressed bytes is what turns
// that into an answer instead.
//
// THE BOUND EXISTS FOR A GZIP BOMB, which is the case a size check on the FILE
// cannot see: a compressed archive can satisfy every size the manifest declares
// and still expand without limit.
func TestReleaseRecordRefusesAnArchiveThatDecompressesPastTheBound(t *testing.T) {
	f := buildRecordFixture(t)

	original := maxArchiveBytes
	maxArchiveBytes = 8

	t.Cleanup(func() { maxArchiveBytes = original })

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive past the decompression bound was accepted, so an oversized " +
			"one is read as however much of it happened to fit")
	}

	if !strings.Contains(err.Error(), "decompresses to more than") {
		t.Errorf("the refusal does not name the bound it hit: %v", err)
	}

	if _, statErr := os.Stat(provenance.Path); statErr == nil {
		t.Error("a record was written from a truncated read of an archive")
	}
}

// AND CONTENT BEHIND THE END-OF-ARCHIVE MARKER IS REFUSED TOO.
//
// This is the same hole as a concatenated stream, one layer in: tar reports EOF at
// two zero blocks, so anything after them inside the SAME gzip stream is never
// walked — a second `billet` there would pass the duplicate refusal while the file
// hashed to exactly what the manifest says. Different extractors disagree about
// this shape, which is precisely the choice a record must not silently make on a
// host's behalf.
//
// A GZIP STREAM'S CRC IS ALSO ONLY CHECKED IF SOMETHING READS TO ITS END, which
// nothing did before this: the walk stopped at the member it wanted and left the
// rest of the stream unverified.
func TestReleaseRecordRefusesContentBehindTheEndOfArchiveMarker(t *testing.T) {
	f := buildRecordFixture(t)

	writeBilletArchiveWithTrailer(t, f.archive, recordFixtureBody,
		"a second tar entry hiding behind the end-of-archive marker")
	describeArchive(t, f)

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive with content behind its end-of-archive marker was accepted, " +
			"so a member the walk cannot see can travel inside a release billet attests to")
	}

	if !strings.Contains(err.Error(), "after its end-of-archive marker") {
		t.Errorf("the refusal does not name what is wrong: %v", err)
	}

	if _, statErr := os.Stat(provenance.Path); statErr == nil {
		t.Error("a record was written for an archive carrying content nothing walked")
	}
}

// writeBilletArchiveWithTrailer writes a valid archive and then appends bytes
// INSIDE the same gzip stream, after tar's end-of-archive marker.
func writeBilletArchiveWithTrailer(t *testing.T, path, contents, trailer string) {
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

	// CLOSING THE TAR WRITER IS WHAT EMITS THE END MARKER, so everything written to
	// the gzip stream after this point is decoded content no walk of members reaches.
	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	if _, err := gz.Write([]byte(trailer)); err != nil {
		t.Fatalf("write past the end marker: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close the gzip: %v", err)
	}
}

// THE ENTRY LIMIT MEANS WHAT IT SAYS AT ITS BOUNDARY.
//
// The counter used to be incremented BEFORE tar.Next(), so the pass that ended the
// walk counted as an entry: an archive with exactly maxArchiveEntries members was
// refused for carrying "more than" that many. An off-by-one in a refusal is not a
// cosmetic bug — it rejects a correct release and reports a reason that is false,
// which is the failure ADR-005 names, because the next thing anybody does is
// delete the check.
func TestReleaseRecordAcceptsExactlyTheEntryLimit(t *testing.T) {
	f := buildRecordFixture(t)

	original := maxArchiveEntries
	maxArchiveEntries = 3

	t.Cleanup(func() { maxArchiveEntries = original })

	// THREE MEMBERS AGAINST A LIMIT OF THREE, one of them the billet being recorded.
	writeArchiveWithMembers(t, f.archive, map[string]string{
		"LICENSE": "the licence",
		"README":  "the readme",
		"billet":  recordFixtureBody,
	})
	describeArchive(t, f)

	if err := cmdReleaseRecord(t.Context(), f.args()); err != nil {
		t.Fatalf("an archive with exactly the entry limit was refused: %v", err)
	}
}

// AND ONE PAST IT IS REFUSED.
func TestReleaseRecordRefusesOnePastTheEntryLimit(t *testing.T) {
	f := buildRecordFixture(t)

	original := maxArchiveEntries
	maxArchiveEntries = 3

	t.Cleanup(func() { maxArchiveEntries = original })

	writeArchiveWithMembers(t, f.archive, map[string]string{
		"LICENSE":   "the licence",
		"README":    "the readme",
		"CHANGELOG": "the changelog",
		"billet":    recordFixtureBody,
	})
	describeArchive(t, f)

	err := cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive past the entry limit was accepted")
	}

	if !strings.Contains(err.Error(), "more than 3 entries") {
		t.Errorf("the refusal does not name the bound it hit: %v", err)
	}
}

// writeArchiveWithMembers writes an archive carrying each named member.
//
// THE ORDER IS FIXED rather than a map's, because `billet` must be reachable in
// every run: a walk that refuses on the entry count before reaching it would make
// the accepting test pass for the wrong reason.
func writeArchiveWithMembers(t *testing.T, path string, members map[string]string) {
	t.Helper()

	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}

	sort.Strings(names)

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the archive: %v", err)
	}

	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	for _, name := range names {
		body := members[name]

		mode := int64(0o644)
		if name == "billet" {
			mode = 0o755
		}

		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write the header for %s: %v", name, err)
		}

		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close the gzip: %v", err)
	}
}

// THE BOUND STOPS THE DECOMPRESSION, IT DOES NOT MERELY NOTICE IT AFTERWARDS.
//
// EVERY OTHER TEST HERE PROVES EVENTUAL REFUSAL AND NOT THAT, which is the hole
// this closes. With the bound set low, tar reads a 512-byte header and the check
// after Next() refuses — whether or not anything limited the reader. So a
// regression to counting without limiting would keep them all green while a
// crafted archive expanded a huge member that is NOT called billet before anything
// looked, which is the work the bound exists to refuse.
//
// THE ERROR CANNOT BE THE OBSERVABLE, AND THE FIRST VERSION OF THIS TEST ASSUMED
// IT COULD. Reporting the bound in preference to the error it caused is deliberate
// — a truncated stream makes tar and gzip cry corruption about a file that is
// intact — and it means a counting-only walk ALSO ends up reporting the bound,
// just after doing all the work. Measured: that version passed against a build
// with the limit removed, which is the defect it was written to catch wearing the
// test's own clothes.
//
// SO WHAT IS ASSERTED IS HOW MANY BYTES THE WALK ASKED FOR. The file arrives
// through a reader that counts and then refuses, with a budget far above what a
// bounded walk needs and far below the archive's size. A hard limit stops asking
// almost immediately; a counting-only walk reads on through the oversized member
// until the budget is gone. That difference is the property, and it is visible
// whatever error comes back.
func TestReleaseRecordStopsDecompressingAtTheBound(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "big.tar.gz")

	// INCOMPRESSIBLE AND FIRST, so the member the walk must not expand is large on
	// disk as well as decompressed, and is reached before `billet`.
	writeArchiveWithMembers(t, archive, map[string]string{
		"aaa-big": incompressible(t, 512<<10),
		"billet":  recordFixtureBody,
	})

	originalBytes := maxArchiveBytes
	maxArchiveBytes = 8 << 10

	t.Cleanup(func() { maxArchiveBytes = originalBytes })

	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open the archive: %v", err)
	}

	defer func() { _ = f.Close() }()

	size := archiveSize(t, archive)
	sentinel := &failAfter{r: f, budget: 128 << 10}

	_, err = archiveContains(sentinel, archive, size, "irrelevant")
	if err == nil {
		t.Fatal("an archive past the decompression bound was accepted")
	}

	if !strings.Contains(err.Error(), "decompresses to more than") {
		t.Errorf("the refusal does not name the bound: %v", err)
	}

	// THE WHOLE POINT: a bounded walk needs the bound plus gzip's own read-ahead and
	// nothing like the budget. Spending all of it means it read through the
	// oversized member first.
	if sentinel.read >= sentinel.budget {
		t.Errorf("the walk consumed its whole %d-byte budget before refusing, so it read "+
			"through the oversized member rather than stopping at the %d-byte bound: "+
			"decompression is counted and not limited", sentinel.budget, maxArchiveBytes)
	}
}

// AND AN ARCHIVE AT EXACTLY THE BOUND IS ACCEPTED, which pins the comparison
// rather than its neighbourhood. An off-by-one here refuses a correct release.
func TestReleaseRecordAcceptsAnArchiveExactlyAtTheBound(t *testing.T) {
	f := buildRecordFixture(t)

	exact := decompressedSize(t, f.archive)

	original := maxArchiveBytes
	maxArchiveBytes = exact

	t.Cleanup(func() { maxArchiveBytes = original })

	if err := cmdReleaseRecord(t.Context(), f.args()); err != nil {
		t.Fatalf("an archive decompressing to exactly the bound (%d bytes) was refused: %v",
			exact, err)
	}
}

// AND ONE BYTE OVER IT IS NOT.
func TestReleaseRecordRefusesAnArchiveOneByteOverTheBound(t *testing.T) {
	f := buildRecordFixture(t)

	exact := decompressedSize(t, f.archive)

	original := maxArchiveBytes
	maxArchiveBytes = exact - 1

	t.Cleanup(func() { maxArchiveBytes = original })

	if err := cmdReleaseRecord(t.Context(), f.args()); err == nil {
		t.Fatalf("an archive decompressing to %d bytes was accepted under a bound of %d",
			exact, exact-1)
	}
}

// decompressedSize reports how many bytes an archive expands to.
func decompressedSize(t *testing.T, path string) int64 {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the archive: %v", err)
	}

	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}

	defer func() { _ = gz.Close() }()

	n, err := io.Copy(io.Discard, gz)
	if err != nil {
		t.Fatalf("decompress the archive: %v", err)
	}

	return n
}

// incompressible returns n bytes gzip cannot shrink, so a member is large on disk
// as well as after decompression.
func incompressible(t *testing.T, n int) string {
	t.Helper()

	body := make([]byte, n)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("generate incompressible bytes: %v", err)
	}

	return string(body)
}

// failAfter stops supplying bytes once a budget is spent.
type failAfter struct {
	r      io.Reader
	budget int64
	read   int64
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.read >= f.budget {
		return 0, errors.New("the sentinel refused to keep feeding this walk")
	}

	if int64(len(p)) > f.budget-f.read {
		p = p[:f.budget-f.read]
	}

	n, err := f.r.Read(p)
	f.read += int64(n)

	return n, err
}

// A STREAM WHOSE OWN CHECKSUM DOES NOT MATCH IS REFUSED.
//
// gzip only verifies its CRC when something reads to the END of the stream, and
// until the padding drain went in nothing did: the walk stopped at the member it
// wanted and left the rest unchecked. So this is the test that keeps the drain
// honest about its second job — and it had none for a while, because the fixture
// that used to damage the footer was rewritten to patch MTIME instead when the
// drain made gzip catch it first. Losing the last observation of a property while
// fixing a different test is how a check quietly stops being one.
//
// THE MANIFEST IS REWRITTEN TO NAME THE DAMAGED FILE, so its size and sha256 both
// agree and the ONLY thing left wrong is the stream's internal checksum. Without
// that the size check refuses first and this proves nothing.
func TestReleaseRecordRefusesAStreamWhoseChecksumDoesNotMatch(t *testing.T) {
	f := buildRecordFixture(t)

	body, err := os.ReadFile(f.archive)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}

	// THE CRC32 IS THE FOUR BYTES BEFORE THE FINAL FOUR (ISIZE), so flipping them
	// leaves the length, the header and every compressed byte alone.
	damaged := make([]byte, len(body))
	copy(damaged, body)

	for i := len(damaged) - 8; i < len(damaged)-4; i++ {
		damaged[i] ^= 0xff
	}

	if err := os.WriteFile(f.archive, damaged, 0o600); err != nil {
		t.Fatalf("damage the checksum: %v", err)
	}

	describeArchive(t, f)

	err = cmdReleaseRecord(t.Context(), f.args())
	if err == nil {
		t.Fatal("an archive whose gzip checksum does not match its contents was recorded, " +
			"so nothing reads the stream to its end and the checksum is never verified")
	}

	if !strings.Contains(err.Error(), "invalid checksum") {
		t.Errorf("the refusal does not name the checksum: %v", err)
	}

	if _, statErr := os.Stat(provenance.Path); statErr == nil {
		t.Error("a record was written for an archive that fails its own checksum")
	}
}
