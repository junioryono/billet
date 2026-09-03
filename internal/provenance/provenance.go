// Package provenance records which release manifest produced the billet that is
// installed on this machine, and proves the record still describes it.
//
// WHY A VERSION STRING IS NOT ENOUGH. A rollout resolves a channel once, to one
// immutable signed manifest, and persists that manifest's DIGEST precisely so
// every host installs the same bytes. A node's registration, though, carries the
// version string its binary was BUILT with and nothing about the bytes behind it
// — so a host upgraded out of band, or rebuilt under the same name, converges a
// rollout on evidence weaker than the decision it is converging. This is the
// record that closes the gap: the updater writes what it installed, and the node
// reports it.
//
// WHY THE BINARY'S OWN HASH IS IN IT. A record naming only a version is defeated
// by the exact case the digest exists to catch — two builds carrying one version
// string, which is what a moved tag produces. Binding the record to the bytes
// means a binary replaced by hand afterwards reports NOTHING rather than
// inheriting the last upgrade's provenance, and "nothing" is an answer the
// rollout already knows how to read.
//
// A LEAF, ON PURPOSE. The updater in cmd/billet writes this and the node client
// in internal/nodeclient reads it, so it can depend on neither; stdlib only.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Path is where the record lives.
//
// A VAR SO A TEST CAN OWN THE DIRECTORY IT WRITES INTO, the same seam
// cmd/billet's upgradeRoot uses and for the same reason: the real path is
// durable state on the machine running the test.
var Path = "/var/lib/billet/installed.json"

// Record is what produced the installed binary.
type Record struct {
	// Version is the release this was installed for, as `billet version` reports
	// it. Diagnostic: nothing decides from it, because the whole point of this
	// file is that a version string is not evidence.
	Version string `json:"version"`

	// ManifestDigest is the sha256 of the signed release manifest that named the
	// artifact this binary came from. It is the thing a rollout compares against
	// its own recorded decision.
	ManifestDigest string `json:"manifest_digest"`

	// BinarySHA256 is the sha256 of the binary that was installed.
	//
	// WHAT MAKES THE RECORD PROVE ANYTHING. Without it the file is a claim about
	// whatever binary happens to sit at the installed path now, which a later
	// hand-replacement inherits silently.
	BinarySHA256 string `json:"binary_sha256"`
}

// maxRecordBytes bounds what will be parsed. A record is a few hundred bytes;
// the bound refuses whatever else ends up at that path.
const maxRecordBytes = 64 << 10

// ErrNoRecord means nothing on this machine says which manifest produced it.
//
// THE ORDINARY CASE, NOT A FAULT. A host installed from a package, built from
// source, or upgraded before this existed has no record, and every caller has to
// treat that as "cannot tell" rather than as a refusal.
var ErrNoRecord = errors.New("provenance: this installation has no record of which " +
	"release manifest produced it")

// ErrNotThisBinary means a record exists and describes different bytes.
//
// DISTINCT FROM ErrNoRecord, because the two are different facts about the
// machine and only one of them is ordinary. This one says something replaced the
// binary without updating the record — which is exactly the case the hash is
// here to catch, and which a caller should say out loud rather than treat as
// silence.
var ErrNotThisBinary = errors.New("provenance: the installed record describes different " +
	"bytes than the binary that is running")

// Write makes a record durable.
//
// ATOMIC, AND FLUSHED WITH ITS DIRECTORY. This is written during an upgrade,
// between stopping a machine's services and starting them again, so a power cut
// in that window must leave either the old record or the new one — never a
// half-written file that the next read refuses and that makes a correctly
// upgraded host report nothing.
func Write(record Record) error {
	if record.BinarySHA256 == "" {
		// A RECORD WITHOUT THE HASH PROVES NOTHING, and writing one would put a
		// file on the machine that every reader has to distrust. Refusing here
		// keeps "no record" meaning what it says.
		return errors.New("provenance: a record must carry the hash of the binary it " +
			"describes, or it cannot be shown to describe it")
	}

	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("provenance: render the record: %w", err)
	}

	body = append(body, '\n')

	dir := filepath.Dir(Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("provenance: prepare %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".installed-*.json")
	if err != nil {
		return fmt.Errorf("provenance: create a temporary record: %w", err)
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("provenance: write the record: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("provenance: flush the record: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("provenance: close the record: %w", err)
	}

	// READABLE BY THE NODE, WHICH MAY NOT BE THE USER THAT WROTE IT. The updater
	// runs as root on a packaged Linux host; a macOS node runs as the operator.
	// Nothing in here is secret — it is a digest and two hashes.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("provenance: set the record's mode: %w", err)
	}

	if err := os.Rename(tmp.Name(), Path); err != nil {
		return fmt.Errorf("provenance: publish the record: %w", err)
	}

	return syncDir(dir)
}

// Read returns the record on this machine, without checking that it still
// describes the running binary.
//
// FOR A DIAGNOSTIC, NOT FOR A DECISION. `billet host-upgrade --status` and the
// operator reading it want to see what is written down even when it no longer
// matches; anything that DECIDES from provenance goes through Installed.
func Read() (Record, error) {
	body, err := os.ReadFile(Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, ErrNoRecord
		}

		return Record{}, fmt.Errorf("provenance: read %s: %w", Path, err)
	}

	if len(body) > maxRecordBytes {
		return Record{}, fmt.Errorf("provenance: %s is %d bytes, which is not a record "+
			"billet wrote", Path, len(body))
	}

	var record Record
	if err := json.Unmarshal(body, &record); err != nil {
		return Record{}, fmt.Errorf("provenance: %s could not be read: %w", Path, err)
	}

	if record.ManifestDigest == "" || record.BinarySHA256 == "" {
		return record, fmt.Errorf("provenance: %s names no manifest digest or no binary "+
			"hash, so it cannot show which release produced this installation", Path)
	}

	return record, nil
}

// Installed reports the manifest digest that produced the running binary.
//
// IT PROVES THE RECORD STILL APPLIES rather than trusting it. A record whose
// binary hash does not match the executable is reported as ErrNotThisBinary and
// yields no digest, because the alternative is a host inheriting the last
// upgrade's provenance for bytes nobody can account for — which is worse than
// saying nothing, since a rollout reads "nothing" correctly and reads a wrong
// digest as proof.
//
// CALL IT ONCE, AND EARLY, AND HOLD THE ANSWER. Hashing the executable is a
// ~22MB read and a node re-registers on every reconnect, so asking per
// registration pays for one answer repeatedly. More importantly the answer must
// describe the bytes the CALLER STARTED WITH: os.Executable resolves to a path on
// macOS, so a binary replaced later would otherwise be hashed in place of the one
// actually running. The caching lives in the caller rather than here because the
// caller is what has a lifetime — a package-level cache would be a value no test
// could clear and a state this leaf has no business owning.
func Installed() (string, error) {
	return readInstalled()
}

func readInstalled() (string, error) {
	record, err := Read()
	if err != nil {
		return "", err
	}

	self, err := executable()
	if err != nil {
		return "", fmt.Errorf("provenance: find the running binary: %w", err)
	}

	sum, err := HashFile(self)
	if err != nil {
		return "", err
	}

	if sum != record.BinarySHA256 {
		return "", fmt.Errorf("%w: %s records %s and %s hashes to %s",
			ErrNotThisBinary, Path, short(record.BinarySHA256), self, short(sum))
	}

	return record.ManifestDigest, nil
}

// executable is os.Executable behind a seam.
//
// A SEAM SO THE TESTS DRIVE THE REAL COMPARISON. os.Executable resolves this
// process, which under `go test` is the test binary — one that has no record and
// makes every case look alike. A test that reimplemented the comparison against
// a file of its own would pass with this function deleted, which is the failure
// this whole change exists to stop somebody else making.
var executable = os.Executable

// HashFile returns a file's sha256, lowercase hex.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("provenance: open %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("provenance: hash %s: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// short renders a hash for a message an operator reads.
func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}

	return sum[:12]
}

// syncDir flushes a directory entry, so a rename survives a power cut.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("provenance: open %s to flush it: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("provenance: flush %s: %w", dir, err)
	}

	return nil
}
