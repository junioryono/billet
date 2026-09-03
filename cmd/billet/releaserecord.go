package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"

	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/releasesource"
)

// readBounded reads a regular file, refusing one larger than the caller expects.
//
// THE BOUND IS APPLIED WHILE READING, not after. os.ReadFile loads whatever is
// there and hands it over for something else to object to, which is not a bound
// at all on a machine somebody can write a very large file to. A non-regular file
// is refused outright: a FIFO named as a manifest never ends, and this command
// runs at the end of an install where hanging is indistinguishable from working.
func readBounded(path string, limit int) ([]byte, error) {
	f, _, err := openRegular(path)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}

	if len(body) > limit {
		return nil, fmt.Errorf("%s is larger than the %d bytes billet will read", path, limit)
	}

	return body, nil
}

// openRegular opens a file that cannot make this command wait, and reports its
// size.
//
// O_NONBLOCK IS WHAT MAKES THE CHECK A CHECK. Opening a FIFO read-only BLOCKS
// until somebody writes to it, so stat-then-open leaves the hang in front of the
// guard and open-then-stat never reaches it — an earlier version did the latter
// and its comment claimed a FIFO was refused. With O_NONBLOCK the open returns
// immediately whatever the path is, and the fstat below then refuses anything
// that is not an ordinary file. It is harmless on a regular file, which is the
// only thing this goes on to read.
//
// FSTAT ON THE DESCRIPTOR, NOT STAT ON THE PATH, so what is refused and what is
// read are the same object. A stat of the name answers about whatever the name
// meant at that instant.
func openRegular(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, 0, fmt.Errorf("stat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		_ = f.Close()

		return nil, 0, fmt.Errorf("%s is not a regular file, so billet will not read it "+
			"as one", path)
	}

	return f, info.Size(), nil
}

// hashReader hashes what is left in a reader, refusing to hash more than limit.
//
// BOUNDED BECAUSE A SIZE READ FROM A STAT IS NOT A PROMISE. The callers check a
// size before getting here, but the file can grow between the fstat and the read,
// and on a network or FUSE filesystem the two answers need not agree at all. This
// reads one byte past the bound so exceeding it is DETECTED rather than silently
// producing the hash of a prefix — which would be a hash of something that was
// never the file.
func hashReader(r io.Reader, limit int64) (string, error) {
	h := sha256.New()

	n, err := io.Copy(h, io.LimitReader(r, limit+1))
	if err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}

	// NO TEST REACHES THIS, AND IT STAYS. Both callers check a size before getting
	// here, so the only way past those checks is a file that GROWS between the
	// fstat and the read — which no deterministic test can stage, and which is
	// exactly what a network or FUSE filesystem can do on its own. It is not
	// mutation-checked for that reason rather than by oversight.
	if n > limit {
		return "", fmt.Errorf("this is larger than the %d bytes billet will hash", limit)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// countingReader counts what passes through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	return n, err
}

func sha256Of(body []byte) []byte {
	sum := sha256.Sum256(body)

	return sum[:]
}

// archiveMember is the file a billet archive carries.
const archiveMember = "billet"

// maxMemberBytes bounds what will be read out of an archive.
//
// A COMPRESSED FILE CAN CLAIM ANY SIZE, and a gzip stream can expand to far more
// than it occupies. billet is tens of megabytes; this refuses a member that would
// have this command read for as long as somebody likes, and refuses it on the
// declared size rather than by stopping partway through.
// A VAR SO A TEST CAN SHRINK IT. The refusal is reachable only by a file larger
// than the bound, and writing half a gigabyte to prove a comparison is not a test
// anybody will keep running.
var maxMemberBytes int64 = 512 << 20

// maxArchiveBytes bounds what will come out of the decompressor in total, and
// maxArchiveEntries how many members will be walked.
//
// THE BYTE BOUND IS THE REAL ONE. An earlier comment here claimed the entry count
// caught "a stream of empty headers", and it does not: Go's tar reader consumes
// PAX and GNU long-name records INSIDE a single Next(), so a long chain of them
// never increments a counter that only sees returned members. What those records
// do cost is decompressed bytes, which is what maxArchiveBytes counts — so the
// byte bound covers the attack the entry bound was wrongly credited with, and the
// entry bound is kept only for what it actually says: how many MEMBERS will be
// walked. billet's release archive is one binary and a licence.
//
// VARS SO A TEST CAN SHRINK THEM, for the same reason maxMemberBytes is one: the
// refusals are only reachable by an archive that exceeds them, and producing two
// gigabytes to prove a comparison is not a test anybody will keep running.
var (
	maxArchiveBytes   int64 = 2 << 30
	maxArchiveEntries       = 1024
)

// archiveContains refuses unless the archive carries exactly one billet with this
// hash, and returns the sha256 of the whole file.
//
// ONE PASS, AND THE HASH COMES OUT OF THE SAME READ AS THE MEMBER. Hashing the
// archive and then reading its member again is two reads of one file, and holding
// a single descriptor across them only closes the rename: another writer can
// still modify the same inode in between, so archive A supplies the hash the
// manifest accepts while archive B supplies the binary that gets recorded as
// having come from it. That is an attestation stronger than its evidence — the
// exact defect this command exists to prevent, one layer down.
//
// THE SIZE IS PASSED IN BECAUSE A DIGEST OF A PREFIX IS NOT A WEAKER ANSWER, IT
// IS A DIFFERENT FILE'S DIGEST PRESENTED AS THIS ONE'S. The caller has already
// fstatted the file and compared that against the manifest, so exactly how many
// bytes must pass through the hash is known before anything is read — and
// requiring precisely that many is what makes the returned sum a fact about the
// file rather than about however much of it the readers happened to consume. An
// earlier version bounded the final drain by maxArchiveBytes, which bounds
// DECOMPRESSED tar data and has nothing to do with the raw file's length, so a
// large enough remainder was silently truncated and the prefix hash returned.
//
// STREAMS ONLY THE EXPECTED MEMBER, never the whole archive to disk. Unpacking
// everything would let a listed release write links or traversal entries outside
// anything this command chose — the same reason install.sh extracts one member to
// stdout rather than expanding the tarball.
func archiveContains(f io.Reader, path string, size int64, want string) (string, error) {
	// EVERY BYTE READ FROM THE FILE IS HASHED, and counted so the total can be
	// checked against the size the caller established.
	sum := sha256.New()
	raw := &countingReader{r: f}
	tee := io.TeeReader(raw, sum)

	// BUFFERED HERE RATHER THAN INSIDE gzip, BECAUSE Reset REPLACES A BUFFER IT
	// OWNS. gzip.NewReader wraps anything that is not a flate.Reader — an io.Reader
	// that also has ReadByte — in its own bufio, and Reset then builds a FRESH one,
	// discarding whatever the old buffer had already pulled out of the file. The
	// multistream check below would then be asking about a position the file is not
	// at. A bufio.Reader IS a flate.Reader, so passing one keeps a single buffer
	// across both calls, which is exactly why the standard library's own multistream
	// example passes a *bytes.Reader.
	buffered := bufio.NewReader(tee)

	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return "", fmt.Errorf("%s is not a gzip archive: %w", path, err)
	}

	defer func() { _ = gz.Close() }()

	// ONE GZIP STREAM. A gzip.Reader is multistream by DEFAULT, and tar stops at the
	// first end-of-archive marker — so a file made by concatenating two archives
	// carries a second `billet` that the duplicate check below never sees, while the
	// manifest legitimately names the whole thing. The invariant this function
	// exists to hold is that the archive names ONE binary; two, in a shape that
	// different extractors resolve differently, is exactly the choice a record must
	// not silently describe. Turning multistream off makes the second stream
	// detectable rather than invisible.
	gz.Multistream(false)

	// AND THE DECOMPRESSED STREAM IS BOUNDED HARD, not merely observed. A gzip bomb
	// is the case a size check on the FILE cannot see: the compressed archive can
	// satisfy every size the manifest declares and still expand without limit. One
	// byte past the bound is read so exceeding it is DETECTED — a limit that simply
	// runs out looks to tar exactly like a clean end of archive.
	decompressed := &countingReader{r: io.LimitReader(gz, maxArchiveBytes+1)}
	reader := tar.NewReader(decompressed)

	var (
		seen    bool
		headers int
	)

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			// THE BOUND EXPLAINS THE ERROR IT CAUSED, so it is consulted first. Cutting
			// the stream off at the limit is what makes tar report a truncated or corrupt
			// archive: true about the bytes it was handed, and misleading about the file,
			// which is intact and merely larger than billet will read. Reporting the
			// consequence sends somebody looking for a corrupt download.
			if boundErr := withinDecompressedBound(decompressed, path); boundErr != nil {
				return "", boundErr
			}

			return "", fmt.Errorf("read %s: %w", path, err)
		}

		// COUNTED AFTER A MEMBER IS RETURNED, so the bound means what it says. It used
		// to be incremented before Next(), which made the pass that ended the walk
		// count as an entry: an archive with exactly maxArchiveEntries members was
		// refused for carrying "more than" that many.
		headers++
		if headers > maxArchiveEntries {
			return "", fmt.Errorf("%s carries more than %d entries, which is not an archive "+
				"billet published", path, maxArchiveEntries)
		}

		if err := withinDecompressedBound(decompressed, path); err != nil {
			return "", err
		}

		if header.Name != archiveMember {
			continue
		}

		// ONLY A REGULAR FILE COUNTS. A link named `billet` resolves to whatever the
		// extracting side already had, which is not a member of this archive at all
		// — so an archive whose billet is a link carries no billet.
		if header.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("%s carries a %s that is not a regular file, so nothing "+
				"in it is the binary this record would describe", path, archiveMember)
		}

		// AND ONLY ONE. Two members with one name is an archive whose meaning depends
		// on which the extractor happened to keep, and a record must not describe a
		// choice somebody else made.
		if seen {
			return "", fmt.Errorf("%s carries more than one %s, so which of them a host "+
				"installed is not something this archive says", path, archiveMember)
		}

		seen = true

		// THE BOUND IS A REFUSAL, NOT A TRUNCATION. Reading through a LimitReader
		// and hashing whatever came out turns an oversized member into a hash of its
		// first N bytes — which is a comparison against something that was never in
		// the archive. Refusing on the declared size says what happened instead.
		if header.Size > maxMemberBytes {
			return "", fmt.Errorf("the %s in %s declares %d bytes, above the %d billet will "+
				"read; nothing was recorded", archiveMember, path, header.Size, maxMemberBytes)
		}

		// AN EMPTY MEMBER NEEDS NO GUARD HERE, and one was written before this was
		// thought through. Two empty files hash alike, so an empty member matches
		// only an empty binary — which the caller already refuses before reaching
		// this, because there is nothing there to record. Against any real binary an
		// empty member simply fails the comparison below.
		got, err := hashReader(reader, maxMemberBytes)
		if err != nil {
			if boundErr := withinDecompressedBound(decompressed, path); boundErr != nil {
				return "", boundErr
			}

			return "", fmt.Errorf("read %s from %s: %w", archiveMember, path, err)
		}

		if got != want {
			return "", fmt.Errorf("the %s in %s hashes to %s and the binary being recorded "+
				"hashes to %s, so that binary did not come out of this archive; nothing "+
				"was recorded", archiveMember, path, got, want)
		}
	}

	if !seen {
		return "", fmt.Errorf("%s carries no %s, so it cannot be what this binary came "+
			"from; nothing was recorded. If this archive was downloaded, it may simply "+
			"not be the one the manifest names — its own hash is checked after this walk, "+
			"because both answers come from a single read", path, archiveMember)
	}

	// THE BOUND IS CHECKED AGAIN AFTER THE WALK ENDS, because the read that ENDED it
	// is not covered by a check made inside the loop.
	if err := withinDecompressedBound(decompressed, path); err != nil {
		return "", err
	}

	// NOTHING MAY HIDE BEHIND THE END-OF-ARCHIVE MARKER. tar stops at two zero
	// blocks and reports EOF, so decoded content after them is never walked — a
	// second `billet` there would defeat the duplicate refusal above while the file
	// still hashed to what the manifest says. Everything left in the stream must be
	// the padding a tar writer emits, and draining it is also what makes gzip verify
	// its own CRC over the stream rather than leaving it unchecked.
	if err := onlyPadding(decompressed, path); err != nil {
		return "", err
	}

	// AND NOTHING MAY FOLLOW THE STREAM ITSELF. With multistream off, a second gzip
	// stream is what Reset reports; io.EOF is the answer that means the file ended
	// where its first stream did.
	if err := gz.Reset(buffered); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", fmt.Errorf("%s carries more than one gzip stream, so which %s a "+
				"host installs depends on how it unpacks the file; nothing was recorded",
				path, archiveMember)
		}

		return "", boundedFirst(decompressed, path,
			fmt.Errorf("read past the end of %s: %w", path, err))
	}

	// EVERY BYTE OF THE FILE WENT THROUGH THE HASH, which is the whole reason the
	// size was passed in. Anything else means the digest describes a prefix, and a
	// prefix digest compared against a manifest is an attestation about a file that
	// does not exist. One byte past the size is read so a file that GREW since the
	// fstat is refused rather than silently hashed short.
	if _, err := io.Copy(io.Discard, io.LimitReader(tee, size-raw.n+1)); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	// NO TEST REACHES THIS EITHER, AND IT STAYS, for the same reason hashReader's
	// limit does. Nothing may follow the gzip stream and nothing may hide behind the
	// end marker, so for any file this walk accepts the stream IS the file and the
	// count matches by construction: the only way here is a file that changed while
	// it was being read. That is not deterministically stageable and is exactly what
	// a concurrent writer or a network filesystem does on its own.
	if raw.n != size {
		return "", fmt.Errorf("%s was %d bytes when billet looked at it and at least %d "+
			"when it read them, so it changed underneath this command; nothing was "+
			"recorded", path, size, raw.n)
	}

	return hex.EncodeToString(sum.Sum(nil)), nil
}

// withinDecompressedBound refuses an archive that has expanded past what billet
// will read.
func withinDecompressedBound(decompressed *countingReader, path string) error {
	if decompressed.n > maxArchiveBytes {
		return fmt.Errorf("%s decompresses to more than %d bytes, which is not an archive "+
			"billet published; nothing was recorded", path, maxArchiveBytes)
	}

	return nil
}

// onlyPadding refuses anything but a tar writer's zero padding in what is left of
// the decompressed stream.
func onlyPadding(decompressed *countingReader, path string) error {
	buf := make([]byte, 32<<10)

	for {
		n, err := decompressed.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return fmt.Errorf("%s carries content after its end-of-archive marker, "+
					"which nothing billet published does and which a walk of its members "+
					"cannot see; nothing was recorded", path)
			}
		}

		if boundErr := withinDecompressedBound(decompressed, path); boundErr != nil {
			return boundErr
		}

		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
}

// boundedFirst prefers the decompression bound over an error the bound produced.
//
// A HARD LIMIT MAKES EVERY READER DOWNSTREAM REPORT CORRUPTION, and gzip's own
// checksum is the loudest of them: the stream really is incomplete, because billet
// stopped supplying it. Saying so as "invalid checksum" describes billet's own
// truncation as damage to the operator's file.
func boundedFirst(decompressed *countingReader, path string, err error) error {
	if boundErr := withinDecompressedBound(decompressed, path); boundErr != nil {
		return boundErr
	}

	return err
}

// cmdReleaseRecord writes which release manifest produced an installed binary,
// after proving the manifest actually names it.
//
// WHY THIS IS A COMMAND AND NOT SHELL. `install.sh` is how most hosts get billet,
// and a host that cannot say which bytes it is running converges a rollout on its
// version alone — a name two builds can share. But the record is an ATTESTATION:
// a rollout treats a digest as proof and BLOCKS a host whose digest disagrees, so
// a record written without checking that the manifest names these bytes is
// stronger than its evidence, which is the exact class of defect this whole area
// exists to remove. A first version of the installer hashed whatever came back
// from a URL called release-manifest.json and recorded it against a binary
// verified only against a checksums file from the same host: a manifest served
// beside a different archive would have made that host converge as PROVED on
// bytes the manifest never named.
//
// Verifying it needs the manifest PARSED — its schema, its artifact list, the
// entry for this platform — and parsing JSON in POSIX shell to decide something
// this load-bearing is how a half-check that looks like a check gets written. So
// the shell downloads and this decides.
func cmdReleaseRecord(_ context.Context, args []string) error {
	fs := newFlagSet("billet release record")
	manifestPath := fs.String("manifest", "", "the release manifest this install came from")
	archivePath := fs.String("archive", "", "the archive the binary was extracted from")
	binaryPath := fs.String("binary", "", "the installed binary the record will describe")

	if err := parse(fs, args); err != nil {
		return err
	}

	for _, required := range []struct{ flag, value string }{
		{"--manifest", *manifestPath},
		{"--archive", *archivePath},
		{"--binary", *binaryPath},
	} {
		if required.value == "" {
			return fmt.Errorf("usage: billet release record --manifest <path> --archive "+
				"<path> --binary <path> (%s is required)", required.flag)
		}
	}

	// READ ONCE, AND THE DIGEST IS OF THESE BYTES.
	//
	// Parsing from one read and then reopening the path to hash it is two reads of
	// something that can change between them: a rename or a symlink swap in that
	// window validates manifest A and records manifest B's digest, which is a
	// record attesting to a document nobody checked. It is also where the bound
	// belongs — os.ReadFile loads whatever is there before any parser can refuse
	// its size.
	body, err := readBounded(*manifestPath, releasesource.MaxManifestBytes)
	if err != nil {
		return fmt.Errorf("read the release manifest: %w", err)
	}

	// PARSED BY THE READER THAT EVERY OTHER PATH USES, so a document this build
	// would refuse anywhere else is refused here too. A record derived from a
	// manifest billet cannot read would attest to something nothing can check.
	manifest, err := releasesource.ParseManifest(body)
	if err != nil {
		return fmt.Errorf("this is not a release manifest billet can act on: %w", err)
	}

	artifact, err := manifest.Select(hostOS, runtime.GOARCH, releasesource.KindArchive)
	if err != nil {
		return fmt.Errorf("%s publishes nothing for %s/%s, so it cannot be what produced "+
			"this binary: %w", manifest.Version, hostOS, runtime.GOARCH, err)
	}

	// THE CHAIN THAT MAKES THE RECORD TRUE, and every link is checked below rather
	// than assumed: the manifest names this archive and its hash, the archive
	// hashes to that, and the binary is the one inside it. A record written without
	// the whole chain says "these bytes came from that manifest" on the strength of
	// some files having arrived together.
	// ONE DESCRIPTOR FOR THE WHOLE ARCHIVE, opened once and rewound between the two
	// passes.
	//
	// Hashing the PATH and then reading its member is two reads of something that
	// can change between them: archive A supplies the hash the manifest accepts,
	// the file becomes archive B, and B's binary is recorded as having come from
	// A's manifest. That is an attestation stronger than its evidence, one layer
	// below the one this command exists to prevent. One descriptor does not fix it
	// on its own — a rename is closed, but another writer can still modify the same
	// inode between the passes — so there is only ONE pass, and the archive's hash
	// comes out of the same read that finds its member.
	archive, archiveSize, err := openRegular(*archivePath)
	if err != nil {
		return err
	}

	defer func() { _ = archive.Close() }()

	// THE SIZE IS CHECKED BEFORE ANYTHING IS HASHED, because it is already known.
	// The fstat above answers it for free, so hashing gigabytes first and then
	// refusing on a number that was available at the start spends the time anyway
	// — and the size is the cheaper half of the same question.
	//
	// Removing this check was wrong once, and the reasoning behind removing it was
	// too: "equal bytes have equal lengths" makes it redundant against the FILE and
	// says nothing about the MANIFEST, whose two facts about one artifact can
	// disagree with each other. A document billet cannot trust to be self-consistent
	// is not one to derive an attestation from.
	if archiveSize != artifact.Size {
		return fmt.Errorf("%s says %s is %d bytes and it is %d, so this manifest does not "+
			"describe this download; nothing was recorded",
			manifest.Version, artifact.Name, artifact.Size, archiveSize)
	}

	// THE SAME TREATMENT AS THE ARCHIVE: opened once, refused if it is not an
	// ordinary file, and hashed from that descriptor.
	binaryFile, binarySize, err := openRegular(*binaryPath)
	if err != nil {
		return err
	}

	defer func() { _ = binaryFile.Close() }()

	if binarySize == 0 {
		return fmt.Errorf("%s is empty, so there is nothing here to record", *binaryPath)
	}

	// A BINARY LARGER THAN THE ARCHIVE WILL EVER YIELD CANNOT BE IN IT, and asking
	// now costs a comparison rather than a read of the whole thing. The member this
	// is compared against is refused above that size, so hashing more than it here
	// only spends time on an answer already known.
	if binarySize > maxMemberBytes {
		return fmt.Errorf("%s is %d bytes, above the %d billet will read, so it cannot be "+
			"a member of any archive billet published; nothing was recorded",
			*binaryPath, binarySize, maxMemberBytes)
	}

	binary, err := hashReader(binaryFile, maxMemberBytes)
	if err != nil {
		return err
	}

	// AND THE BINARY IS THE ONE INSIDE THAT ARCHIVE, which the first version
	// ASSUMED because its one caller happened to extract it from there. An
	// assumption is not a link: the three paths arrive independently, so a caller
	// that passed some other billet — a stale one at the destination, a build from
	// elsewhere — would have produced a record attesting that those bytes came from
	// a manifest that says nothing about them. That is the same defect one layer
	// down from the one this command was written to fix.
	//
	// THE ARCHIVE'S HASH IS WHAT THIS RETURNS, taken from the bytes it just walked,
	// so the digest compared against the manifest below and the member compared
	// against the binary above came out of one read of one file.
	sum, err := archiveContains(archive, *archivePath, archiveSize, binary)
	if err != nil {
		return err
	}

	if sum != artifact.SHA256 {
		return fmt.Errorf("%s says %s hashes to %s and it hashes to %s, so this manifest "+
			"did not produce this download; nothing was recorded",
			manifest.Version, artifact.Name, artifact.SHA256, sum)
	}

	// THE MANIFEST'S OWN SHA256 IS THE IDENTITY A ROLLOUT DECIDES ON, not the
	// version inside it — which is the whole point of the field. Taken from the
	// bytes that were parsed, so the record cannot name a document other than the
	// one that was checked.
	digest := hex.EncodeToString(sha256Of(body))

	if err := provenance.Write(provenance.Record{
		Version:        manifest.Version,
		ManifestDigest: digest,
		BinarySHA256:   binary,
	}); err != nil {
		return err
	}

	fmt.Printf("Recorded that %s came from manifest %s.\n", manifest.Version, digest)

	return nil
}
