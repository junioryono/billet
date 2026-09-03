package deployarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
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
