// Package deployarchive captures and restores a billet deployment as ONE unit.
//
// THE UNIT IS FOUR THINGS AND THEY ARE USELESS APART. The ledger, the deployment
// identity, the GitHub App private key and the node-wire certificate authority
// each depend on the others to mean anything:
//
//   - a ledger without its identity is a fresh authority that cannot see the
//     compute the old one launched, so it reaps live jobs as orphans;
//   - an identity without the CA cannot issue a node certificate, and minting a
//     replacement authority drops every node in the fleet at once;
//   - a CA without the App key cannot get a token, so nothing is ever scheduled.
//
// Restoring a subset produces a deployment that looks healthy and is not, which
// is why the requirement is worded as a REFUSAL rather than a warning. This
// package refuses; it never repairs and never guesses.
//
// A POSTGRESQL LEDGER IS THE ONE PIECE BILLET DOES NOT CARRY, and saying so is
// the whole of schema 2. SQLite's VACUUM INTO produces a consistent copy of the
// entire ledger as one file; there is no equivalent billet should own for
// PostgreSQL, because a consistent copy there is pg_dump or the provider's
// snapshot — the operator's to run and to restore. Copying rows through billet's
// own connection would produce an archive that LOOKS like a backup and is not.
//
// SO THE ARCHIVE RECORDS THE LEDGER AS EXTERNAL RATHER THAN OMITTING IT
// SILENTLY, and that distinction is the feature. Until this existed the command
// FAILED OUTRIGHT on such a deployment, so the half billet does own was not
// captured either — and for a control plane built by the control-plane-postgres
// module that is the only recovery path there is: the module has no ledger volume
// by design, its root volume is delete_on_termination, and the App private key is
// issued exactly once. An identity-only archive is not a lesser backup of the
// same thing; it is the whole of what billet is entitled to copy, paired with a
// statement of where the rest lives.
//
// WHAT THE RESTORE SIDE OWES IN RETURN is a refusal. Pairing the two halves is
// the invariant, so an identity-only archive may not be installed onto a target
// whose config says the ledger is local, and may not be installed at all without
// the operator asserting the ledger it belongs to is back — which is the one
// thing billet cannot check, because the database is on the other end of a DSN.
//
// It prints nothing. Every refusal is returned as a lifeops.Refusal so the
// command layer renders them all at once — an operator who has to re-run a
// command to find the next problem is paying for a diagnostic that already knew.
package deployarchive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Schema is the archive format this build WRITES.
//
// A READER ACCEPTS EXACTLY THE SCHEMAS IT KNOWS — see readableSchemas, which is
// a set rather than this constant. Publishing a version a reader cannot read is
// a flag day nobody can pull their way out of, so the writer moves only after
// every build that might read one already understands the new number; that is
// the rule the runner-image manifest follows and the reason the set exists at
// all.
//
// 2 ADDS THE EXTERNAL LEDGER. Schema 1 could not express one: its entry set
// REQUIRES ledger/billet.db, so an archive without it is refused as incomplete —
// correctly, on schema 1, where an absent ledger really is a missing piece
// rather than a statement about where the ledger lives.
const Schema = 2

// readableSchemas is every version this build can restore FROM.
//
// SCHEMA 1 IS STILL READ, and that is not compatibility for its own sake: every
// archive an operator already holds is one, and they are read on the worst day
// of a deployment's life. A schema-1 manifest always carries a ledger, so it
// needs no external-ledger fields to be understood — its absence of them IS the
// answer.
//
// A schema-2 archive read by an older billet is refused by decodeManifest with a
// sentence naming both versions, which is what the bump buys: without it an
// identity-only archive would meet "an archive is not an archive without
// ledger/billet.db" and send an operator looking for a file that was never
// supposed to be there.
var readableSchemas = map[int]bool{1: true, 2: true}

// externalBackends is every engine an archive may name as holding its ledger.
//
// A CLOSED SET, NOT A FLOOR, which is the rule this package already applies to
// its entries. Accepting any non-empty string looked harmless — config
// validation would refuse an unknown backend long before a restore — but this
// package is EXPORTED and internal/e2e reaches it directly, so "unreachable
// through the CLI" is a property of one caller rather than of the rule. Two
// things went with it: Write would happily produce an archive naming an engine
// nothing can restore, and the planner accepted target `X` for an archive naming
// `X`, so two identical typos agreed with each other.
//
// SQLITE IS NOT IN IT, and that is not an omission. A SQLite ledger travels IN
// the archive; an archive claiming SQLite is external is describing a file it
// should be carrying.
var externalBackends = map[string]bool{"postgres": true}

// Kind is stamped in the manifest so a directory of unrelated files cannot be
// handed to restore and produce a confusing failure three checks later.
const Kind = "billet-deployment-backup"

// Entry names. STABLE ACROSS VERSIONS: they appear in a manifest that a later
// billet reads, so renaming one is a schema change.
const (
	EntryManifest = "manifest.json"
	EntryLedger   = "ledger/billet.db"
	EntryIdentity = "identity/deployment-id"
	EntryAppKey   = "github/app-private-key.pem"
	EntryConfig   = "config/billet.yaml"

	authorityPrefix = "authority/"
)

// AuthorityEntry is the archive name for one allowlisted authority file.
func AuthorityEntry(name string) string { return authorityPrefix + name }

// entryMode is what every file this package writes gets.
//
// ONE MODE FOR ALL OF THEM. Which entries are secret varies — the CA
// certificate is not, the CA key and the App key are — and giving them one mode
// removes a class of mistake rather than teaching a distinction an archive has
// no use for. The directory is 0700 as well, so this is the second of two
// answers to the same question rather than the only one.
const entryMode os.FileMode = 0o600

// maxSmallEntry bounds everything that is not the ledger.
//
// An identity is 33 bytes, a PEM key a couple of kilobytes, a config a few
// kilobytes more. Without a bound, an archive entry pointing at something
// enormous is read whole into memory before anything notices it is not what it
// claims to be.
const maxSmallEntry = 1 << 20

// FileRecord is one entry's identity in the manifest.
//
// THE DIGEST IS THE POINT. A restore reads an archive on the worst day of a
// deployment's life, off media nobody has verified since it was written, and the
// files it is about to publish are credentials. Nothing is installed on the
// strength of a filename.
type FileRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// writeSmall creates one archive entry, refusing to replace anything.
//
// O_EXCL AND O_NOFOLLOW TOGETHER. O_EXCL is what makes a destination that is
// already occupied an error rather than an overwrite; O_NOFOLLOW is what stops
// a planted symlink in the destination directory turning a 0600 write into a
// write somewhere the attacker can read. Neither is redundant: the first is
// about a mistake, the second about a hostile directory, and this function is
// used to write a CA private key.
func writeSmall(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("deployarchive: create %s: %w", filepath.Dir(path), err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, entryMode)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("deployarchive: %s already exists and billet will not write over it",
				path)
		}

		return fmt.Errorf("deployarchive: write %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("deployarchive: write %s: %w", path, err)
	}

	// EXPLICIT, because the umask takes bits off what OpenFile was asked for and
	// one of these files is a private key. Correcting by instruction rather than
	// hoping the process's umask happened to be 022.
	if err := f.Chmod(entryMode); err != nil {
		return fmt.Errorf("deployarchive: set the mode on %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("deployarchive: sync %s: %w", path, err)
	}

	return nil
}

// readSmall reads one entry, refusing anything that is not a plain file it was
// pointed straight at.
//
// A SYMLINK IS REFUSED RATHER THAN FOLLOWED. An archive is a directory an
// operator copied from somewhere, and a link inside it makes billet read a file
// the archive does not contain — after which the digest check compares the
// manifest against whatever that link found.
func readSmall(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err // may be os.IsNotExist, which callers branch on
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf(
			"deployarchive: %s is a symlink; billet reads an archive entry only from the path "+
				"the archive names", path)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("deployarchive: %s is not a regular file", path)
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("deployarchive: open %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(io.LimitReader(f, maxSmallEntry+1))
	if err != nil {
		return nil, fmt.Errorf("deployarchive: read %s: %w", path, err)
	}

	if len(body) > maxSmallEntry {
		return nil, fmt.Errorf("deployarchive: %s is larger than %d bytes, which no archive "+
			"entry of this kind is", path, maxSmallEntry)
	}

	return body, nil
}

// digest is the manifest's hash of a byte slice.
func digest(body []byte) string {
	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

// digestFile hashes a file without reading it whole into memory — the ledger is
// the one entry with no useful size bound.
func digestFile(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, fmt.Errorf("deployarchive: inspect %s: %w", path, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return "", 0, fmt.Errorf("deployarchive: %s is a symlink", path)
	}

	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("deployarchive: %s is not a regular file", path)
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, fmt.Errorf("deployarchive: open %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()

	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("deployarchive: read %s: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// onStaged observes a verified temporary just before it is published. Nil in
// production; a test uses it to stage the one race this function is written
// against.
var onStaged func(tmp string) error

// copyFile installs one file by streaming, and never puts bytes at the
// destination that it has not vouched for.
//
// STAGED, VERIFIED, THEN LINKED — the same shape `billet github-app create` uses
// for the App key, and for the same reason. The obvious version copies straight
// to the destination, hashes as it goes, and unlinks on a mismatch; that is a
// check-then-unlink BY PATHNAME, which is exactly the class the credential rules
// in this project refuse.
//
// BUT STAGING ALONE IS NOT ENOUGH, and the first version of this stopped there.
// The bytes are hashed through a DESCRIPTOR and then published by a PATHNAME,
// and nothing in between proves those still name the same inode: anything able
// to write this directory can rename the temporary away and leave its own file
// at that name, after which os.Link publishes the replacement as the ledger.
// Verifying the descriptor against the name before the link — and the
// destination against it afterwards — is what closes that, and it is what
// writeKeyAtomically does one credential over.
//
// THE DIGEST IS CHECKED BECAUSE Open VERIFIED BY PATHNAME. Open hashed the
// ledger through the name, and this reopens that name; on media somebody can
// change, the bytes installed would not be the bytes checked.
//
// O_NONBLOCK on the source for the reason readSmall has it: a FIFO swapped in
// for a regular file would block the restore forever rather than be rejected.
func copyFile(src, dst string, want FileRecord) (string, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("deployarchive: open %s: %w", src, err)
	}

	defer func() { _ = in.Close() }()

	srcInfo, err := in.Stat()
	if err != nil {
		return "", fmt.Errorf("deployarchive: inspect %s: %w", src, err)
	}

	if !srcInfo.Mode().IsRegular() {
		return "", fmt.Errorf("deployarchive: %s is not a regular file", src)
	}

	dir := filepath.Dir(dst)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("deployarchive: create %s: %w", dir, err)
	}

	out, err := os.CreateTemp(dir, ".billet-restore-*")
	if err != nil {
		return "", fmt.Errorf("deployarchive: stage %s: %w", dst, err)
	}

	tmp := out.Name()

	// stray is the staging name if it could not be removed — REPORTED rather
	// than swallowed. os.Link leaves two names for one file, and this project
	// already states the rule one credential over: an unmentioned second copy is
	// what nobody finds until it matters. It is not a failure of the restore, so
	// it travels back rather than becoming an error.
	stray := ""

	// THE DESCRIPTOR STAYS OPEN UNTIL THE LINK IS PROVED, because it is the only
	// thing that can say what the temporary NAME refers to. Removing by that name
	// is done last and only after the same check.
	//
	// THE IDENTITY CHECK COMES BEFORE THE CLOSE, and the order is the whole
	// point: f.Stat() on a CLOSED descriptor fails, which discardStaged reads as
	// "not ours" — so closing first leaves the staging file behind on every
	// SUCCESSFUL install. That is a second copy of a restored ledger, and one
	// credential over it is a second copy of an App key. The same mistake is
	// recorded against writeKeyAtomically in the billet-security skill.
	defer func() {
		if !discardStaged(out, tmp) {
			stray = tmp
		}

		_ = out.Close()
	}()

	// EXPLICIT, because CreateTemp's 0600 is the umask's to reduce and one of
	// the files this installs is a ledger.
	if err := out.Chmod(entryMode); err != nil {
		return stray, fmt.Errorf("deployarchive: set the mode on %s: %w", tmp, err)
	}

	h := sha256.New()

	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		return stray, fmt.Errorf("deployarchive: copy %s: %w", src, err)
	}

	if sum := hex.EncodeToString(h.Sum(nil)); sum != want.SHA256 || n != want.Size {
		return stray, fmt.Errorf(
			"deployarchive: %s changed after this backup was verified — it is now %d bytes with a "+
				"different digest, and billet will not install bytes it has not vouched for. "+
				"Nothing was written to %s", src, n, dst)
	}

	if err := out.Sync(); err != nil {
		return stray, fmt.Errorf("deployarchive: flush %s: %w", tmp, err)
	}

	if onStaged != nil {
		if err := onStaged(tmp); err != nil {
			return stray, err
		}
	}

	// THE NAME MUST STILL BE THE INODE THAT WAS HASHED.
	if err := sameAsDescriptor(out, tmp); err != nil {
		return stray, fmt.Errorf(
			"deployarchive: %s is no longer the file billet staged and verified, so it will not "+
				"be published to %s: %w", tmp, dst, err)
	}

	// FAILS RATHER THAN REPLACES. os.Rename would clobber whatever arrived at
	// the destination in the meantime; os.Link has no such form, which is why
	// this package uses it and why there is no rename fallback.
	if err := os.Link(tmp, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			return stray, fmt.Errorf(
				"deployarchive: %s already exists and billet will not write over it", dst)
		}

		return stray, fmt.Errorf("deployarchive: install %s: %w", dst, err)
	}

	// AND THE DESTINATION MUST BE IT TOO. The link took a name; this is the only
	// check that can say what the name it created now refers to.
	if err := sameAsDescriptor(out, dst); err != nil {
		return stray, fmt.Errorf(
			"deployarchive: %s is not the file billet staged, so this restore cannot vouch for "+
				"what is there. It was NOT removed — look at it before running this again: %w",
			dst, err)
	}

	return stray, nil
}

// sameAsDescriptor proves a pathname still refers to an open file.
//
// os.SameFile ON AN Lstat, so a symlink planted at the name is a mismatch
// rather than something followed to the right inode.
func sameAsDescriptor(f *os.File, path string) error {
	want, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect the staged file: %w", err)
	}

	got, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}

	if !os.SameFile(want, got) {
		return fmt.Errorf("%s is a different file", path)
	}

	return nil
}

// discardStaged removes the temporary name, and only while it still refers to
// the file this call staged.
//
// AFTER A SUCCESSFUL LINK THIS IS MANDATORY, not tidiness: the link leaves two
// names for one inode, and for the App key one layer over that is a second copy
// of a credential nobody accounts for. It is never a blind unlink, because the
// name may by then be somebody else's file.
//
// IT REPORTS WHETHER THE NAME IS GONE, and a false is carried back to the
// operator rather than swallowed — the same rule writeKeyAtomically follows,
// for the same reason: a second copy nothing mentions is one nobody finds.
// A name that is no longer this call's file counts as dealt with; it is not
// billet's to remove and saying so is the accurate answer.
func discardStaged(f *os.File, tmp string) bool {
	if err := sameAsDescriptor(f, tmp); err != nil {
		// Either gone already, or somebody else's. Neither leaves a copy of this
		// call's file at that name.
		return true
	}

	return os.Remove(tmp) == nil
}

// syncDir persists directory ENTRIES. A synced file whose directory entry was
// never written is a file nothing can find, and here that means an archive that
// reports success and comes back empty.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("deployarchive: open %s to sync it: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("deployarchive: sync %s: %w", dir, err)
	}

	return nil
}

// entryPath resolves an archive entry to a path, refusing a name that escapes.
//
// The names are constants in this package today, so this is a guard against a
// MANIFEST — which is a file from outside — naming `../../etc/something` and
// having a restore verify a digest against it.
func entryPath(dir, entry string) (string, error) {
	if entry == "" || strings.HasPrefix(entry, "/") || strings.Contains(entry, `\`) {
		return "", fmt.Errorf("deployarchive: %q is not an archive entry name", entry)
	}

	clean := filepath.ToSlash(filepath.Clean(entry))
	if clean != entry || clean == "." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("deployarchive: %q is not an archive entry name", entry)
	}

	return filepath.Join(dir, filepath.FromSlash(clean)), nil
}

// errNotAnArchive is what a directory that is not one produces, so the command
// layer can say "point --from at a backup" rather than relaying a stat error.
var errNotAnArchive = errors.New("deployarchive: this directory is not a billet backup")
