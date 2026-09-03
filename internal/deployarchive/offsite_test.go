package deployarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/state"
)

// memStore is an object store in a map, recording the ORDER things were written.
//
// The order is the property: a manifest is what makes a prefix an archive, so
// publishing it before the entries it describes would advertise a complete
// deployment that is not there. Nothing else in this package can observe that.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	order   []string

	// failAfter stops the store after this many writes, which is how an
	// interrupted upload is staged. Zero means never.
	failAfter int
	// tamper rewrites one key's bytes on the way out, so a fetch reads something
	// the manifest does not describe.
	tamper map[string][]byte
}

func newMemStore() *memStore {
	return &memStore{objects: map[string][]byte{}}
}

func (m *memStore) Put(_ context.Context, key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failAfter > 0 && len(m.order) >= m.failAfter {
		return errors.New("the network went away")
	}

	// THE STORE'S OWN NO-CLOBBER, reported with the sentinel a resume recognises —
	// a fake that returned a bare error could not exercise the resume at all.
	if _, exists := m.objects[key]; exists {
		return fmt.Errorf("%w: %s", ErrObjectExists, key)
	}

	m.objects[key] = append([]byte(nil), body...)
	m.order = append(m.order, key)

	return nil
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if body, ok := m.tamper[key]; ok {
		return append([]byte(nil), body...), nil
	}

	body, ok := m.objects[key]
	if !ok {
		return nil, errors.New("no such object")
	}

	return append([]byte(nil), body...), nil
}

func (m *memStore) written() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.order...)
}

// uploadOf backs a deployment up and pushes it to a store.
func uploadOf(t *testing.T, store ObjectStore, prefix string) (deployment, *Archive) {
	t.Helper()

	src := newDeployment(t)
	dir := filepath.Join(t.TempDir(), "backup")

	backupTo(t, src, dir)

	a, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := Upload(t.Context(), a, store, prefix); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	return src, a
}

// An archive that went to a store comes back as the same deployment.
//
// The whole point of the round trip: what is fetched is opened by the ONE
// verifier — every digest, the closed entry set, the cross-checks — and then
// restores into a directory that has never held a deployment.
func TestAnArchiveSurvivesAnObjectStoreRoundTrip(t *testing.T) {
	store := newMemStore()

	src, sent := uploadOf(t, store, "billet/dep/2026-08-30T07-00-00Z")

	back, err := Fetch(t.Context(), store, "billet/dep/2026-08-30T07-00-00Z",
		filepath.Join(t.TempDir(), "fetched"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if back.Manifest.DeploymentID != sent.Manifest.DeploymentID {
		t.Fatalf("the fetched archive names deployment %s, want %s",
			back.Manifest.DeploymentID, sent.Manifest.DeploymentID)
	}

	tgt := newTarget(t, src.github)

	restoreInto(t, back, tgt)

	id, found, err := state.PeekDeploymentID(tgt.StateDir)
	if err != nil || !found {
		t.Fatalf("PeekDeploymentID: %v (found=%v)", err, found)
	}

	if id != src.id {
		t.Errorf("restored deployment %s, want %s", id, src.id)
	}

	got, err := os.ReadFile(tgt.AppKeyPath)
	if err != nil {
		t.Fatalf("read the restored App key: %v", err)
	}

	if !bytes.Equal(got, src.appKey) {
		t.Error("the App key did not survive the round trip byte for byte")
	}
}

// THE MANIFEST IS WRITTEN LAST, which is the whole crash guarantee.
//
// It is what makes a prefix an archive: Open refuses a directory without one,
// and a bucket listing counts a prefix only when its manifest is there. Written
// first, an interruption would leave a prefix advertising a complete deployment
// and holding part of one.
func TestTheManifestIsPublishedLast(t *testing.T) {
	store := newMemStore()

	uploadOf(t, store, "billet/dep/now")

	written := store.written()
	if len(written) < 2 {
		t.Fatalf("only %d objects were written; this test would prove nothing", len(written))
	}

	if got := written[len(written)-1]; got != "billet/dep/now/"+EntryManifest {
		t.Errorf("the last object written was %s, want the manifest", got)
	}

	for _, key := range written[:len(written)-1] {
		if strings.HasSuffix(key, EntryManifest) {
			t.Errorf("%s was written before the entries it describes", key)
		}
	}
}

// An interrupted upload is not an archive anybody can fetch.
func TestAnInterruptedUploadIsNotAnArchive(t *testing.T) {
	store := newMemStore()
	store.failAfter = 2

	src := newDeployment(t)
	dir := filepath.Join(t.TempDir(), "backup")

	backupTo(t, src, dir)

	a, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := Upload(t.Context(), a, store, "billet/dep/half"); err == nil {
		t.Fatal("an upload that lost the network reported success")
	}

	// AND WHAT IS THERE CANNOT BE MISTAKEN FOR A BACKUP, which is the half that
	// matters: the entries landed, and without the manifest nothing will offer
	// them to an operator as a deployment.
	if _, err := Fetch(t.Context(), store, "billet/dep/half",
		filepath.Join(t.TempDir(), "fetched")); err == nil {
		t.Fatal("a half-uploaded prefix was fetched as though it were an archive")
	}
}

// AN INTERRUPTED UPLOAD RESUMES, which is what the command tells an operator to
// do.
//
// A store refuses to replace an occupied key — deliberately, since that refusal
// is what stops anything overwriting a backup — so a retry would otherwise stop
// on the first entry the first run had already published, and the manifest would
// never land, and the prefix would never become an archive. "Run it again" has
// to be true.
func TestAnInterruptedUploadResumes(t *testing.T) {
	store := newMemStore()
	store.failAfter = 2

	src := newDeployment(t)
	dir := filepath.Join(t.TempDir(), "backup")

	backupTo(t, src, dir)

	a, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := Upload(t.Context(), a, store, "billet/dep/now"); err == nil {
		t.Fatal("an upload that lost the network reported success")
	}

	partial := len(store.written())
	if partial == 0 {
		t.Fatal("nothing was uploaded, so the resume below proves nothing")
	}

	store.failAfter = 0

	if _, err := Upload(t.Context(), a, store, "billet/dep/now"); err != nil {
		t.Fatalf("the resumed upload: %v", err)
	}

	// AND IT IS AN ARCHIVE NOW, which it was not before.
	if _, err := Fetch(t.Context(), store, "billet/dep/now",
		filepath.Join(t.TempDir(), "fetched")); err != nil {
		t.Fatalf("the resumed upload did not produce a fetchable archive: %v", err)
	}
}

// A KEY THAT HOLDS SOMEBODY ELSE'S BYTES IS A REFUSAL, not a resume.
//
// Continuing past one would publish a manifest describing entries that are not
// there — an archive that lists a ledger and points at another archive's.
func TestAnUploadRefusesAKeyHoldingDifferentBytes(t *testing.T) {
	store := newMemStore()

	src := newDeployment(t)
	dir := filepath.Join(t.TempDir(), "backup")

	backupTo(t, src, dir)

	a, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Somebody else's object under a key this upload wants.
	if err := store.Put(t.Context(), "billet/dep/now/"+EntryIdentity,
		[]byte("another deployment")); err != nil {
		t.Fatalf("seed the store: %v", err)
	}

	_, err = Upload(t.Context(), a, store, "billet/dep/now")
	if err == nil {
		t.Fatal("an upload published over a key holding different bytes")
	}

	if !strings.Contains(err.Error(), "DIFFERENT bytes") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}

	// AND THE MANIFEST NEVER LANDED, so nothing will offer this prefix as a
	// backup.
	if _, ok := store.objects["billet/dep/now/"+EntryManifest]; ok {
		t.Error("a refused upload published its manifest")
	}
}

// Bytes that changed in the store are refused on the way in.
//
// A fetch reads media nobody has verified since it was written, and the files it
// is about to publish are credentials. Nothing is installed on the strength of a
// key name.
func TestBytesThatChangedInTheStoreAreRefused(t *testing.T) {
	store := newMemStore()

	uploadOf(t, store, "billet/dep/now")

	// The SAME LENGTH, because appending a byte tests the size check rather than
	// the digest — the mistake this repository has already recorded once.
	original := store.objects["billet/dep/now/"+EntryIdentity]
	tampered := append([]byte(nil), original...)
	tampered[0] ^= 0xff

	store.tamper = map[string][]byte{"billet/dep/now/" + EntryIdentity: tampered}

	_, err := Fetch(t.Context(), store, "billet/dep/now", filepath.Join(t.TempDir(), "fetched"))
	if err == nil {
		t.Fatal("a fetch installed bytes the manifest does not describe")
	}
}

// A manifest naming an entry outside the directory is refused before a single
// byte is fetched for it.
func TestAFetchRefusesAnEntryThatEscapesTheDirectory(t *testing.T) {
	store := newMemStore()

	uploadOf(t, store, "billet/dep/now")

	// A manifest is the one thing a fetch reads before it can verify anything, so
	// this is the shape that has to be refused structurally.
	m := Manifest{
		Schema: Schema, Kind: Kind, DeploymentID: "dep",
		Files: []FileRecord{{Path: "../../escaped", SHA256: "x", Size: 1}},
	}

	body, err := m.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	store.tamper = map[string][]byte{"billet/dep/now/" + EntryManifest: body}

	dir := filepath.Join(t.TempDir(), "fetched")

	if _, err := Fetch(t.Context(), store, "billet/dep/now", dir); err == nil {
		t.Fatal("a fetch accepted a manifest naming an entry outside its directory")
	}

	if _, err := os.Lstat(filepath.Join(filepath.Dir(filepath.Dir(dir)), "escaped")); err == nil {
		t.Error("a fetch wrote outside the directory it was given")
	}
}
