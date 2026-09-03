package deployarchive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// Archive is a verified backup directory.
//
// VERIFIED AT OPEN, ONCE. Everything downstream — the planner, the --dry-run
// report and the executor — works from this value, so there is no path on which
// a file is installed that was not digest-checked, and no second reading that
// could see different bytes than the one that was checked.
type Archive struct {
	Dir      string
	Manifest Manifest

	// small holds every entry except the ledger, which stays on disk because it
	// is the only one with no useful size bound.
	small map[string][]byte

	// manifestSHA identifies this archive, and is computed HERE from the bytes
	// Open validated rather than by re-reading the file later. A second read
	// could see different bytes than the one that was checked — and a read that
	// FAILED would have to answer something, which is how two unrelated archives
	// come to share an empty digest and a restore journal accepts the wrong one.
	manifestSHA string
}

// requiredEntries is what an archive is not an archive without.
//
// The previous authority is deliberately absent: it exists only while a
// rotation is running, and requiring it would refuse every ordinary backup.
//
// THE LEDGER IS REQUIRED UNLESS THE MANIFEST SAYS IT IS EXTERNAL, which is why
// this takes the manifest rather than being a package-level slice. The claim
// decides — never the absence of the file. Deriving it the other way round would
// make a TRUNCATED archive and a deliberate one indistinguishable, and the
// truncated one would then restore an identity paired with nothing.
func requiredEntries(m Manifest) []string {
	required := []string{
		EntryIdentity,
		EntryAppKey,
		AuthorityEntry("ca.key"),
		AuthorityEntry("ca.crt"),
		AuthorityEntry("authority-created"),
	}

	if !m.Ledger.IsExternal() {
		required = append(required, EntryLedger)
	}

	return required
}

// knownEntries is the COMPLETE set schema 1 may declare.
//
// A CLOSED SET, NOT A FLOOR. Without it a manifest could declare
// `authority/ca-next.key`, pass every integrity check — the file is there and
// its digest matches — and be silently omitted from a restore that then reports
// success, because publication walks a fixed set of items rather than whatever
// the manifest lists. An entry this build does not know how to install must
// refuse the archive rather than travel in it unread.
//
// AN EXTERNAL ARCHIVE MAY NOT DECLARE A LEDGER AT ALL, and that is the same rule
// facing the other way: the manifest says the ledger is elsewhere, so an entry
// claiming to BE the ledger contradicts it. Refusing names the contradiction;
// accepting it would install a ledger the manifest says does not exist, onto a
// host whose config points the control plane at a database instead.
func knownEntries(m Manifest) map[string]bool {
	out := map[string]bool{
		EntryIdentity: true,
		EntryAppKey:   true,
		// Reference only; a restore never installs it. See BackupRequest.
		EntryConfig: true,
	}

	if !m.Ledger.IsExternal() {
		out[EntryLedger] = true
	}

	for _, f := range wirecert.AuthorityFiles {
		out[AuthorityEntry(f.Name)] = true
	}

	return out
}

// Open reads an archive and proves it is whole before anything acts on it.
//
// NOTHING IS TRUSTED BECAUSE OF ITS NAME. Every declared file is checked against
// the manifest's digest, every undeclared file is a refusal, and the pieces are
// checked against EACH OTHER: the identity file must agree with the manifest,
// the CA pair must hold together, and the authority must name the deployment the
// archive claims. An archive is read on the worst day of a deployment's life,
// off media nobody has verified since it was written.
func Open(ctx context.Context, dir string) (*Archive, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("deployarchive: resolve %s: %w", dir, err)
	}

	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("deployarchive: read the backup at %s: %w", abs, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("deployarchive: %s is a symlink; billet reads a backup only from "+
			"the path it was given", abs)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", errNotAnArchive, abs)
	}

	manifestPath, err := entryPath(abs, EntryManifest)
	if err != nil {
		return nil, err
	}

	raw, err := readSmall(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s has no %s in it", errNotAnArchive, abs, EntryManifest)
		}

		return nil, err
	}

	m, err := decodeManifest(raw)
	if err != nil {
		return nil, err
	}

	a := &Archive{Dir: abs, Manifest: m, small: map[string][]byte{}, manifestSHA: digest(raw)}

	if err := a.loadDeclared(); err != nil {
		return nil, err
	}

	if err := a.refuseUndeclared(); err != nil {
		return nil, err
	}

	if err := a.crossCheck(); err != nil {
		return nil, err
	}

	// SKIPPED FOR AN EXTERNAL LEDGER, because there is nothing beside the
	// manifest to compare it against. What replaces it is loadDeclared and
	// refuseUndeclared refusing any ledger entry or file at all, so "the manifest
	// describes what is here" is still established — by there being nothing.
	if !a.Manifest.Ledger.IsExternal() {
		if err := a.checkManifestDescribesItsLedger(ctx); err != nil {
			return nil, err
		}
	}

	return a, nil
}

// checkManifestDescribesItsLedger proves the schema the manifest claims is the
// schema the snapshot carries.
//
// THE MANIFEST IS WHAT THE PLANNER JUDGES, and a manifest that describes a
// different ledger than the one beside it would let an archive pass the
// newer-billet check on the strength of a list nothing verified. The digest
// already proves the snapshot is the one that was captured; this proves the
// summary of it is honest.
func (a *Archive) checkManifestDescribesItsLedger(ctx context.Context) error {
	got, err := state.PeekMigrations(ctx, a.LedgerPath())
	if err != nil {
		return err
	}

	want := a.Manifest.Ledger.Migrations

	if len(got) != len(want) {
		return fmt.Errorf(
			"deployarchive: %s says the ledger carries %d migrations and %s carries %d",
			EntryManifest, len(want), EntryLedger, len(got))
	}

	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf(
				"deployarchive: %s describes migration %d as %q and %s carries %q at that "+
					"position", EntryManifest, got[i].Version, want[i].Name, EntryLedger,
				got[i].Name)
		}
	}

	return nil
}

// loadDeclared reads and digest-checks everything the manifest names.
func (a *Archive) loadDeclared() error {
	declared := map[string]bool{}
	known := knownEntries(a.Manifest)

	for _, rec := range a.Manifest.Files {
		if declared[rec.Path] {
			return fmt.Errorf("deployarchive: %s declares %s twice", EntryManifest, rec.Path)
		}

		if !known[rec.Path] {
			return fmt.Errorf(
				"deployarchive: %s declares %s, which schema %d has no place for. This billet "+
					"would install every other entry and silently leave that one behind, so it "+
					"refuses the archive instead", EntryManifest, rec.Path, Schema)
		}

		declared[rec.Path] = true

		path, err := entryPath(a.Dir, rec.Path)
		if err != nil {
			return err
		}

		if rec.Path == EntryLedger {
			sum, size, err := digestFile(path)
			if err != nil {
				return err
			}

			if err := checkRecord(rec, sum, size); err != nil {
				return err
			}

			continue
		}

		body, err := readSmall(path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("deployarchive: %s declares %s and the file is not there",
					EntryManifest, rec.Path)
			}

			return err
		}

		if err := checkRecord(rec, digest(body), int64(len(body))); err != nil {
			return err
		}

		a.small[rec.Path] = body
	}

	var missing []string

	for _, want := range requiredEntries(a.Manifest) {
		if !declared[want] {
			missing = append(missing, want)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"%w: it is missing %s. The ledger, the deployment identity, the App key and the "+
				"node-wire authority are one unit — restoring a subset produces a deployment "+
				"that looks healthy and is not",
			errNotWholeDeployment, strings.Join(missing, ", "))
	}

	return nil
}

func checkRecord(rec FileRecord, sum string, size int64) error {
	if size != rec.Size {
		return fmt.Errorf("deployarchive: %s is %d bytes and %s says %d",
			rec.Path, size, EntryManifest, rec.Size)
	}

	if sum != rec.SHA256 {
		return fmt.Errorf(
			"deployarchive: %s does not match the digest %s records for it. This backup has been "+
				"changed or damaged since it was written, and billet will not install a "+
				"credential it cannot vouch for", rec.Path, EntryManifest)
	}

	return nil
}

// refuseUndeclared refuses a file in the archive that the manifest does not
// name.
//
// AN EXTRA FILE IS NOT HARMLESS. It is either a manifest that does not describe
// its own archive — in which case nothing here can be relied on — or something
// somebody added, and the one thing this directory is for is holding
// credentials that get installed onto a control plane.
func (a *Archive) refuseUndeclared() error {
	declared := map[string]bool{EntryManifest: true}
	for _, rec := range a.Manifest.Files {
		declared[rec.Path] = true
	}

	var extra []string

	err := filepath.WalkDir(a.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(a.Dir, path)
		if relErr != nil {
			return relErr
		}

		if !declared[filepath.ToSlash(rel)] {
			extra = append(extra, filepath.ToSlash(rel))
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("deployarchive: read %s: %w", a.Dir, err)
	}

	if len(extra) > 0 {
		sort.Strings(extra)

		return fmt.Errorf(
			"deployarchive: %s holds files %s does not describe (%s). Billet installs credentials "+
				"out of this directory and will not act on one whose manifest does not account "+
				"for what is in it", a.Dir, EntryManifest, strings.Join(extra, ", "))
	}

	return nil
}

// crossCheck proves the pieces belong to each other.
func (a *Archive) crossCheck() error {
	identity := strings.TrimSpace(string(a.small[EntryIdentity]))
	if identity != a.Manifest.DeploymentID {
		return fmt.Errorf(
			"deployarchive: %s holds deployment %s and %s says %s. These must be the same value; "+
				"billet cannot tell which is the deployment this backup belongs to",
			EntryIdentity, identity, EntryManifest, a.Manifest.DeploymentID)
	}

	cert, err := wirecert.ParseAuthorityPair(
		a.small[AuthorityEntry("ca.key")], a.small[AuthorityEntry("ca.crt")])
	if err != nil {
		return err
	}

	named, err := wirecert.AuthorityDeployment(cert)
	if err != nil {
		return err
	}

	if named != identity {
		return fmt.Errorf(
			"deployarchive: the authority in this backup was issued for deployment %s and the "+
				"backup is of %s. Verifying against a certificate authority is what decides which "+
				"nodes may connect, so restoring these together would hand this deployment "+
				"another installation's fleet", named, identity)
	}

	prevKey, haveKey := a.small[AuthorityEntry("ca-previous.key")]
	prevCert, haveCert := a.small[AuthorityEntry("ca-previous.crt")]

	if haveKey != haveCert {
		return fmt.Errorf(
			"deployarchive: this backup carries only half of the previous authority. The previous " +
				"KEY is what signs the certificate a control plane presents while its fleet " +
				"renews, so restoring without both leaves every un-renewed node unable to verify " +
				"it")
	}

	if haveKey {
		if _, err := wirecert.ParseAuthorityPair(prevKey, prevCert); err != nil {
			return fmt.Errorf("deployarchive: the previous authority in this backup does not hold "+
				"together: %w", err)
		}
	}

	// PARSED, NOT MERELY PRESENT. A truncated PEM is exactly what an interrupted
	// write leaves, and it fails at the first API call rather than here — on a
	// restored control plane, where nobody is watching for it.
	if err := github.ValidatePrivateKey(a.small[EntryAppKey]); err != nil {
		return fmt.Errorf("deployarchive: the GitHub App private key in this backup does not "+
			"parse: %w", err)
	}

	return nil
}

// Entry returns one small entry's bytes.
func (a *Archive) Entry(name string) ([]byte, bool) {
	body, ok := a.small[name]

	return body, ok
}

// LedgerPath is where the snapshot lives inside the archive.
func (a *Archive) LedgerPath() string {
	path, err := entryPath(a.Dir, EntryLedger)
	if err != nil {
		// Unreachable: EntryLedger is a constant this package controls, and Open
		// has already resolved it. Returning the join keeps this total rather
		// than panicking inside a control-plane binary.
		return filepath.Join(a.Dir, filepath.FromSlash(EntryLedger))
	}

	return path
}

// AuthorityNames lists the authority files this archive carries, in publication
// order.
//
// THE ORDER IS THE CRASH ARGUMENT, not alphabetical convenience, and the two
// pairs order OPPOSITELY because one is required and the other is not. The
// CURRENT key leads its certificate, matching how one is created: an
// interruption between them must leave the half-initialised state billet
// REFUSES loudly rather than a certificate whose key belongs to something else.
// The PREVIOUS certificate leads its key, matching how a rotation publishes one:
// there the certificate says a rotation was started and the key says it is
// committed, so an interruption between them has to leave a state a control
// plane treats as inert rather than one no rotation can produce. The marker is
// last, because its whole job is to make a LATER absence mean loss — written
// first, an interruption would leave a directory claiming to have had an
// authority that was never installed, which is the one state ErrAuthorityLost
// cannot be talked out of.
//
// publicationRank is what the executor actually sorts by; this list must agree
// with it, and a caller that depends on the order should say so.
func (a *Archive) AuthorityNames() []string {
	order := []string{"ca.key", "ca.crt", "ca-previous.crt", "ca-previous.key", "authority-created"}

	var out []string

	for _, name := range order {
		if _, ok := a.small[AuthorityEntry(name)]; ok {
			out = append(out, name)
		}
	}

	return out
}

// errNotWholeDeployment is returned when an archive describes less than a
// deployment.
var errNotWholeDeployment = errors.New("deployarchive: this backup is not a whole deployment")
