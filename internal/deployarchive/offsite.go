package deployarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

// ObjectStore is the narrow thing an off-box copy needs.
//
// TWO METHODS AND NO DELETE. The absence is the point rather than an omission: a
// store this package could delete through is one the control plane's own
// credential could destroy the history with, on the very host whose loss the
// off-box copy exists to survive. Retention belongs to the bucket.
//
// AN INTERFACE HERE RATHER THAN A DEPENDENCY ON internal/archivestore, so this
// package keeps knowing only what an archive IS, and the transport keeps knowing
// only how to move bytes. The command layer joins them.
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// ErrObjectExists is what a store's no-clobber Put reports for a key that is
// already occupied.
//
// DECLARED HERE SO A RESUME CAN RECOGNISE IT. A store refuses to replace an
// object — which is the property that stops anything overwriting a backup — and
// without a name for that refusal an upload interrupted half way could never be
// retried: the second run would stop on the first entry the first run had
// already published. internal/archivestore's own sentinel wraps this.
var ErrObjectExists = errors.New("deployarchive: that object already exists")

// Upload copies a verified archive to an object store.
//
// THE MANIFEST GOES LAST, and that ordering is the whole crash guarantee. A
// manifest is what makes a prefix an archive — Open refuses a directory without
// one as "not a billet backup", and a listing counts a prefix only when its
// manifest is there — so an upload interrupted anywhere leaves entries that
// nothing will offer an operator as a backup. Written first, an interruption
// would leave a prefix that ADVERTISES a complete deployment and holds part of
// one, which is the failure the whole package is built around.
//
// EVERY ENTRY IS RE-DIGESTED ON THE WAY OUT. Open verified this archive when it
// was opened, and these bytes are read again from a pathname afterwards — the
// same reason copyFile re-checks on the way IN. An archive that changed under
// billet must not be published as one it vouched for.
func Upload(ctx context.Context, a *Archive, store ObjectStore, prefix string) ([]string, error) {
	if a == nil || store == nil {
		return nil, errors.New("deployarchive: an upload needs an archive and a store")
	}

	// SORTED, so the order is the same on every run and a resumed upload after a
	// partial one revisits the same keys in the same sequence.
	files := append([]FileRecord(nil), a.Manifest.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	var uploaded []string

	for _, rec := range files {
		if rec.Path == EntryManifest {
			return nil, errors.New("deployarchive: the manifest must not declare itself; it is " +
				"published last and cannot carry its own digest")
		}

		body, err := readEntryForUpload(a, rec)
		if err != nil {
			return uploaded, err
		}

		key := path.Join(prefix, rec.Path)

		if err := putOrProveIdentical(ctx, store, key, body); err != nil {
			return uploaded, err
		}

		uploaded = append(uploaded, key)
	}

	manifest, err := readSmall(filepath.Join(a.Dir, EntryManifest))
	if err != nil {
		return uploaded, fmt.Errorf("deployarchive: read %s: %w", EntryManifest, err)
	}

	if digest(manifest) != a.manifestSHA {
		return uploaded, fmt.Errorf(
			"deployarchive: %s changed after this archive was verified, and billet will not "+
				"publish a manifest it has not vouched for", filepath.Join(a.Dir, EntryManifest))
	}

	key := path.Join(prefix, EntryManifest)

	if err := putOrProveIdentical(ctx, store, key, manifest); err != nil {
		return uploaded, err
	}

	return append(uploaded, key), nil
}

// Identity is what an archive's manifest alone says about it: the name its
// upload used and when it was taken. Reading one validates nothing else in the
// directory, so it still answers for an archive whose other entries are gone.
type Identity struct {
	Name      string
	CreatedAt time.Time
	digest    string
	files     []FileRecord
}

// ReadIdentity reads the identity of the archive at dir from its manifest.
func ReadIdentity(dir string) (Identity, error) {
	raw, err := readSmall(filepath.Join(dir, EntryManifest))
	if err != nil {
		return Identity{}, fmt.Errorf("deployarchive: read %s: %w", filepath.Join(dir, EntryManifest), err)
	}

	m, err := decodeManifest(raw)
	if err != nil {
		return Identity{}, err
	}

	created, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return Identity{}, fmt.Errorf("deployarchive: %s records no time it was taken: %w",
			filepath.Join(dir, EntryManifest), err)
	}

	a := Archive{Manifest: m, manifestSHA: digest(raw)}

	return Identity{Name: a.Name(), CreatedAt: created, digest: a.manifestSHA, files: m.Files}, nil
}

// VerifyRemainder proves that everything still in the archive at dir is what
// its manifest declares: every file a declared entry of the declared size and
// digest, and nothing a link, a device or anything else. A declared entry that
// is missing is allowed, because a removal cut short leaves exactly that. Its
// refusal names the first thing the manifest does not account for, which is
// local data the bucket does not hold.
func (id Identity) VerifyRemainder(dir string) error {
	declared := make(map[string]FileRecord, len(id.files))
	parents := map[string]bool{".": true}

	for _, rec := range id.files {
		declared[rec.Path] = rec

		for d := path.Dir(rec.Path); d != "." && d != "/"; d = path.Dir(d) {
			parents[d] = true
		}
	}

	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("deployarchive: read %s: %w", p, err)
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		switch {
		case d.IsDir() && parents[rel]:
			return nil
		case d.IsDir():
			return fmt.Errorf("deployarchive: %s is a directory no entry %s declares lies in", p, EntryManifest)
		case rel == EntryManifest:
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("deployarchive: %s is %s, which no archive entry is", p, d.Type())
		}

		rec, ok := declared[rel]
		if !ok {
			return fmt.Errorf("deployarchive: %s is not an entry %s declares", p, EntryManifest)
		}

		sum, size, err := digestFile(p)
		if err != nil {
			return err
		}

		if sum != rec.SHA256 || size != rec.Size {
			return fmt.Errorf("deployarchive: %s is not the entry %s declares", p, EntryManifest)
		}

		return nil
	})
}

// HeldBy reports whether the store holds this archive's own manifest at
// prefix: the same bytes, by digest, and not merely an archive of the same
// name. The manifest is uploaded last and carries every entry's digest, so
// the store holding it is the store holding the whole archive.
func (id Identity) HeldBy(ctx context.Context, store ObjectStore, prefix string) (bool, error) {
	remote, err := store.Get(ctx, path.Join(prefix, EntryManifest))
	if err != nil {
		return false, fmt.Errorf("deployarchive: read the bucket's %s for %s: %w", EntryManifest, id.Name, err)
	}

	return digest(remote) == id.digest, nil
}

// putOrProveIdentical writes one object, or proves the one already there is the
// same bytes.
//
// THIS IS WHAT MAKES "RUN IT AGAIN" TRUE. A store refuses to replace an occupied
// key — deliberately, because that refusal is what stops anything overwriting a
// backup — so an upload interrupted half way could otherwise never be retried:
// the second run would stop on the first entry the first run had published, and
// the manifest would never land, and the prefix would never become an archive.
//
// PROVED BY READING IT BACK, not assumed from the name. Two different archives
// of one deployment can be named for the same second, so a key being present is
// not evidence that it holds these bytes — and continuing past one that holds
// somebody else's would publish a manifest describing entries that are not
// there. A mismatch is a refusal naming the key.
func putOrProveIdentical(ctx context.Context, store ObjectStore, key string, body []byte) error {
	err := store.Put(ctx, key, body)

	switch {
	case err == nil:
		return nil
	case !errors.Is(err, ErrObjectExists):
		return fmt.Errorf("deployarchive: upload %s: %w", key, err)
	}

	have, getErr := store.Get(ctx, key)
	if getErr != nil {
		return fmt.Errorf(
			"deployarchive: %s is already in the store and billet could not read it back to see "+
				"whether it is this archive's copy: %w", key, getErr)
	}

	if !bytes.Equal(have, body) {
		return fmt.Errorf(
			"deployarchive: %s is already in the store and holds DIFFERENT bytes, so this is "+
				"another archive under the same name. billet will not replace it, and will not "+
				"publish a manifest describing entries that are not there — back up again, "+
				"which names the new archive for the instant it was taken", key)
	}

	return nil
}

// readEntryForUpload reads one declared entry and proves it is still the bytes
// the manifest describes.
func readEntryForUpload(a *Archive, rec FileRecord) ([]byte, error) {
	full, err := entryPath(a.Dir, rec.Path)
	if err != nil {
		return nil, err
	}

	// The ledger is the one entry with no useful size bound, and it is read whole
	// here because an object store's PUT takes a body rather than a stream. A
	// ledger is megabytes; maxSmallEntry would refuse it.
	body, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("deployarchive: read %s: %w", full, err)
	}

	sum := sha256.Sum256(body)

	if hex.EncodeToString(sum[:]) != rec.SHA256 || int64(len(body)) != rec.Size {
		return nil, fmt.Errorf(
			"deployarchive: %s changed after this archive was verified, and billet will not "+
				"upload bytes it has not vouched for", full)
	}

	return body, nil
}

// Fetch downloads an archive into dir and opens it.
//
// NOTHING IS TRUSTED ON THE WAY IN. The manifest arrives first and is decoded
// before anything else is asked for — so a schema this build does not read is
// refused before a single credential is written — and every entry name it
// declares goes through entryPath, which refuses one that escapes the directory.
// What lands is then handed to Open, which is the ONE verifier: digests, the
// closed entry set, and the cross-checks between the pieces. There is no second
// implementation of that here, because two would eventually disagree about what
// is safe.
func Fetch(ctx context.Context, store ObjectStore, prefix, dir string) (*Archive, error) {
	if store == nil {
		return nil, errors.New("deployarchive: a fetch needs a store")
	}

	body, err := store.Get(ctx, path.Join(prefix, EntryManifest))
	if err != nil {
		return nil, fmt.Errorf("deployarchive: fetch %s: %w",
			path.Join(prefix, EntryManifest), err)
	}

	m, err := decodeManifest(body)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("deployarchive: create %s: %w", dir, err)
	}

	// 0700 EXPLICITLY, whether billet created the directory or found it: it is
	// about to hold two private keys, and MkdirAll leaves an existing mode alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("deployarchive: tighten %s: %w", dir, err)
	}

	for _, rec := range m.Files {
		full, err := entryPath(dir, rec.Path)
		if err != nil {
			return nil, err
		}

		entry, err := store.Get(ctx, path.Join(prefix, rec.Path))
		if err != nil {
			return nil, fmt.Errorf("deployarchive: fetch %s: %w", path.Join(prefix, rec.Path), err)
		}

		if err := writeSmall(full, entry); err != nil {
			return nil, err
		}
	}

	if err := writeSmall(filepath.Join(dir, EntryManifest), body); err != nil {
		return nil, err
	}

	if err := syncArchiveDirs(dir, m.Files); err != nil {
		return nil, err
	}

	// AND ONLY NOW IS ANY OF IT BELIEVED.
	return Open(ctx, dir)
}
