package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/archivestore"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/state"
)

// fakeArchiveStore is a bucket in a map. The S3 mechanics have their own tests
// in internal/archivestore; what these are about is what the COMMANDS do with
// one.
type fakeArchiveStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	order   []string
	putErr  error
	listErr error
	clock   time.Time
}

func newFakeArchiveStore() *fakeArchiveStore {
	return &fakeArchiveStore{
		objects: map[string][]byte{},
		clock:   time.Date(2026, 8, 30, 7, 0, 0, 0, time.UTC),
	}
}

func (f *fakeArchiveStore) Put(_ context.Context, key string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.putErr != nil {
		return f.putErr
	}

	if _, exists := f.objects[key]; exists {
		return errors.New("that object already exists")
	}

	f.objects[key] = append([]byte(nil), body...)
	f.order = append(f.order, key)

	return nil
}

func (f *fakeArchiveStore) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	body, ok := f.objects[key]
	if !ok {
		return nil, errors.New("no such object")
	}

	return append([]byte(nil), body...), nil
}

func (f *fakeArchiveStore) Prefix(deployment string) string {
	return "billet/" + deployment + "/"
}

func (f *fakeArchiveStore) List(
	_ context.Context, deployment string,
) ([]archivestore.Archive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.listErr != nil {
		return nil, f.listErr
	}

	var out []archivestore.Archive

	for key := range f.objects {
		rest, ok := strings.CutPrefix(key, "billet/")
		if !ok {
			continue
		}

		parts := strings.Split(rest, "/")
		if len(parts) != 3 || parts[2] != archivestore.ManifestEntry {
			continue
		}

		if deployment != "" && parts[0] != deployment {
			continue
		}

		out = append(out, archivestore.Archive{
			Deployment: parts[0], Name: parts[1], Modified: f.clock,
		})
	}

	return out, nil
}

func (f *fakeArchiveStore) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.order...)
}

// withArchiveStore points the commands at a fake bucket.
func withArchiveStore(t *testing.T, store *fakeArchiveStore) *fakeArchiveStore {
	t.Helper()

	prev := openArchiveStore

	t.Cleanup(func() { openArchiveStore = prev })

	openArchiveStore = func(*config.Config) (archiveStore, bool, error) {
		return store, true, nil
	}

	return store
}

// A backup is uploaded AFTER it is written, never instead.
//
// The local directory is the contract — it is what an operator's own tooling
// picks up and what stays behind when the network is down — so it exists and is
// complete whatever the bucket does.
func TestABackupIsUploadedAfterItIsWritten(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())

	f := newBackupFixture(t, true)
	dest := filepath.Join(t.TempDir(), "backup")

	if err := cmdLocalBackup(t.Context(), []string{
		"--config", f.configPath, "--out", dest,
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	if _, err := deployarchive.Open(t.Context(), dest); err != nil {
		t.Fatalf("the local archive is not one: %v", err)
	}

	written := store.written()
	if len(written) < 2 {
		t.Fatalf("the bucket holds %d objects, want a whole archive", len(written))
	}

	last := written[len(written)-1]
	if !strings.HasSuffix(last, "/"+deployarchive.EntryManifest) {
		t.Errorf("the last object uploaded was %s, want the manifest — a prefix whose manifest "+
			"lands first advertises a deployment that is not all there", last)
	}

	// UNDER THIS DEPLOYMENT'S OWN PREFIX, which is what lets two deployments
	// share a bucket and what an IAM policy is scoped to.
	if !strings.HasPrefix(last, "billet/"+f.deployment+"/") {
		t.Errorf("the archive went to %s, want this deployment's prefix", last)
	}
}

// An upload that fails leaves the local archive and says it is still good.
//
// THE OPERATOR'S NEXT MOVE DEPENDS ON THIS. "The backup failed" sends somebody
// looking for a backup they have; what they need to hear is that the directory
// is complete and only the copy did not happen.
func TestAnUploadFailureLeavesTheLocalArchiveIntact(t *testing.T) {
	store := newFakeArchiveStore()
	store.putErr = errors.New("the network went away")

	withArchiveStore(t, store)

	f := newBackupFixture(t, true)
	dest := filepath.Join(t.TempDir(), "backup")

	err := cmdLocalBackup(t.Context(), []string{"--config", f.configPath, "--out", dest})
	if err == nil {
		t.Fatal("an upload that failed reported success")
	}

	if _, err := deployarchive.Open(t.Context(), dest); err != nil {
		t.Fatalf("the local archive did not survive a failed upload: %v", err)
	}
}

// --no-upload writes the archive and touches no bucket.
func TestNoUploadWritesTheArchiveAndUploadsNothing(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())

	f := newBackupFixture(t, true)
	dest := filepath.Join(t.TempDir(), "backup")

	if err := cmdLocalBackup(t.Context(), []string{
		"--config", f.configPath, "--out", dest, "--no-upload",
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	if _, err := deployarchive.Open(t.Context(), dest); err != nil {
		t.Fatalf("the local archive is not one: %v", err)
	}

	if len(store.written()) != 0 {
		t.Errorf("--no-upload uploaded %v", store.written())
	}
}

// The whole point: a machine that has never seen this deployment restores it
// out of the bucket, with nothing local to start from.
func TestARestoreFetchesFromTheBucketAndPutsTheDeploymentBack(t *testing.T) {
	stubLifecycleLock(t)

	withArchiveStore(t, newFakeArchiveStore())

	src := newBackupFixture(t, true)

	if err := cmdLocalBackup(t.Context(), []string{
		"--config", src.configPath, "--out", filepath.Join(t.TempDir(), "backup"),
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	into := filepath.Join(t.TempDir(), "fetched")

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from-backup", "latest",
		"--into", into, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore --from-backup: %v", err)
	}

	id, found, err := state.PeekDeploymentID(tgt.stateDir)
	if err != nil || !found {
		t.Fatalf("PeekDeploymentID: %v (found=%v)", err, found)
	}

	if id != src.deployment {
		t.Errorf("restored deployment %s, want %s", id, src.deployment)
	}

	// THE FETCHED COPY IS KEPT. On the day this runs the operator has a new
	// machine and no local archive; deleting the one they just downloaded takes
	// away the only copy they have.
	if _, err := deployarchive.Open(t.Context(), into); err != nil {
		t.Errorf("the fetched archive was not kept at %s: %v", into, err)
	}
}

// `latest` REFUSES rather than guesses when the bucket holds two deployments.
//
// Restoring the wrong one installs an authority no node in this fleet trusts,
// and the machine asking has no identity of its own to disambiguate with.
func TestLatestRefusesWhenTheBucketHoldsTwoDeployments(t *testing.T) {
	store := newFakeArchiveStore()
	store.objects["billet/alpha/one/"+archivestore.ManifestEntry] = []byte("{}")
	store.objects["billet/beta/one/"+archivestore.ManifestEntry] = []byte("{}")

	_, err := resolveBackup(t.Context(), store, "", "latest")
	if err == nil {
		t.Fatal("latest chose between two deployments")
	}

	for _, want := range []string{"alpha", "beta", "--deployment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// An archive that is not there is answered with the ones that are.
func TestAnUnknownArchiveNamesWhatTheBucketHolds(t *testing.T) {
	store := newFakeArchiveStore()
	store.objects["billet/alpha/2026-08-29/"+archivestore.ManifestEntry] = []byte("{}")

	_, err := resolveBackup(t.Context(), store, "", "yesterday")
	if err == nil {
		t.Fatal("an archive that is not in the bucket resolved")
	}

	if !strings.Contains(err.Error(), "2026-08-29") {
		t.Errorf("the refusal does not say what IS there: %v", err)
	}
}

// An empty bucket says so rather than reporting nothing.
func TestAnEmptyBucketIsAnAnswer(t *testing.T) {
	_, err := resolveBackup(t.Context(), newFakeArchiveStore(), "", "latest")
	if err == nil {
		t.Fatal("an empty bucket resolved an archive")
	}

	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("the refusal does not mention the interrupted-upload case: %v", err)
	}
}

// A fetched archive is never put inside the state directory.
//
// It is the same two private keys under the same directory a restore scans, a
// `billet local uninstall` reports as preserved, and the CA allowlist walks.
func TestAFetchedArchiveMayNotLandInTheStateDirectory(t *testing.T) {
	withArchiveStore(t, newFakeArchiveStore())

	src := newBackupFixture(t, true)

	if err := cmdLocalBackup(t.Context(), []string{
		"--config", src.configPath, "--out", filepath.Join(t.TempDir(), "backup"),
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	tgt := newBackupFixture(t, false)

	inside := filepath.Join(tgt.stateDir, "archive")

	err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from-backup", "latest",
		"--into", inside, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("an archive was fetched into the state directory")
	}

	if _, statErr := os.Lstat(inside); statErr == nil {
		t.Error("the refusal still created the directory")
	}
}

// --from-backup with no bucket configured says what is missing, and why a URL
// could not have carried it.
func TestFromBackupWithoutABucketSaysWhatIsMissing(t *testing.T) {
	prev := openArchiveStore

	t.Cleanup(func() { openArchiveStore = prev })

	openArchiveStore = func(*config.Config) (archiveStore, bool, error) { return nil, false, nil }

	f := newBackupFixture(t, false)

	err := cmdLocalRestore(t.Context(), []string{
		"--config", f.configPath, "--from-backup", "latest", "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("--from-backup ran with no bucket configured")
	}

	if !strings.Contains(err.Error(), "backup.s3") || !strings.Contains(err.Error(), "region") {
		t.Errorf("the refusal does not say what to add: %v", err)
	}
}

// A SECOND --from-backup RUN IS NOT REFUSED BY THE COPY THE FIRST ONE KEPT.
//
// The fetched archive is kept deliberately, and the default destination is named
// after it, so an operator whose restore was interrupted runs the same command
// again and lands on a directory holding exactly this archive. Refusing that as
// "not empty" leaves a disaster-recovery command with nowhere to go on the day
// it is least convenient — and picking a fresh destination does not help, since
// the resume identity is the manifest rather than the pathname.
func TestARefetchReusesTheArchiveTheFirstAttemptKept(t *testing.T) {
	stubLifecycleLock(t)

	store := newFakeArchiveStore()
	withArchiveStore(t, store)

	src := newBackupFixture(t, true)

	if err := cmdLocalBackup(t.Context(), []string{
		"--config", src.configPath, "--out", filepath.Join(t.TempDir(), "backup"),
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	into := filepath.Join(t.TempDir(), "fetched")

	// A FIRST ATTEMPT THAT FETCHED AND THEN REFUSED: --dry-run reports and
	// changes nothing about the deployment, and the archive it downloaded stays.
	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from-backup", "latest", "--into", into, "--dry-run",
	}); err != nil {
		t.Fatalf("the first --from-backup run: %v", err)
	}

	if _, err := deployarchive.Open(t.Context(), into); err != nil {
		t.Fatalf("the first run did not keep the archive at %s: %v", into, err)
	}

	// WIDENED IN BETWEEN, because skipping PrepareDestination is also what skips
	// the one line that tightens this directory — and it holds the node-wire CA
	// key and the App key. An archive whose bytes verify says nothing about who
	// can read it.
	if err := os.Chmod(into, 0o755); err != nil {
		t.Fatalf("widen the kept directory: %v", err)
	}

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from-backup", "latest",
		"--into", into, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("the second --from-backup run onto the copy the first one kept: %v", err)
	}

	info, err := os.Lstat(into)
	if err != nil {
		t.Fatalf("inspect the reused directory: %v", err)
	}

	if info.Mode().Perm() != 0o700 {
		t.Errorf("the reused archive directory is mode %04o, want 0700", info.Mode().Perm())
	}

	id, found, err := state.PeekDeploymentID(tgt.stateDir)
	if err != nil || !found {
		t.Fatalf("PeekDeploymentID: %v (found=%v)", err, found)
	}

	if id != src.deployment {
		t.Errorf("restored deployment %s, want %s", id, src.deployment)
	}
}

// A DIRECTORY THAT IS NOT THIS ARCHIVE IS STILL REFUSED.
//
// The reuse above is decided by comparing the bucket's manifest with the one on
// disk and then verifying every digest under it, which is what stops it becoming
// "anything already here will do": half of one backup mixed into another is not
// a deployment, and both halves verify on their own.
func TestARefetchStillRefusesADirectoryThatIsNotThisArchive(t *testing.T) {
	stubLifecycleLock(t)

	store := newFakeArchiveStore()
	withArchiveStore(t, store)

	src := newBackupFixture(t, true)

	kept := filepath.Join(t.TempDir(), "backup")
	if err := cmdLocalBackup(t.Context(), []string{
		"--config", src.configPath, "--out", kept,
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	// SOMETHING ELSE ENTIRELY. No manifest, so nothing about this directory says
	// it is the archive being fetched, and the ordinary emptiness refusal stands.
	occupied := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(occupied, 0o700); err != nil {
		t.Fatalf("stage the occupied directory: %v", err)
	}

	if err := os.WriteFile(filepath.Join(occupied, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatalf("stage a file in it: %v", err)
	}

	err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from-backup", "latest",
		"--into", occupied, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("a fetch mixed an archive into a directory that already held something")
	}

	if !strings.Contains(err.Error(), "not empty") {
		t.Errorf("the refusal does not say the directory is occupied: %v", err)
	}

	// AND THE HALF THAT MATTERS MORE: this archive's manifest over an archive
	// that does not verify. A manifest comparison alone would read a truncated
	// or tampered copy as "no need to fetch again".
	damaged := filepath.Join(t.TempDir(), "damaged")
	copyArchiveDir(t, kept, damaged)

	if err := os.WriteFile(filepath.Join(damaged, "deployment-id"), []byte("something else\n"),
		0o600); err != nil {
		t.Fatalf("damage the copy: %v", err)
	}

	err = cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from-backup", "latest",
		"--into", damaged, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("a fetch accepted a directory whose archive does not verify")
	}

	// AND IT SAYS WHAT TO DO. Re-fetching cannot repair the copy — every entry is
	// written no-clobber — so the only way forward is removing that directory or
	// naming another, and a bare digest mismatch from further down the restore
	// does not tell an operator either of those.
	if !strings.Contains(err.Error(), "does not verify") ||
		!strings.Contains(err.Error(), damaged) {
		t.Errorf("the refusal does not name the directory and say it cannot be used: %v", err)
	}
}

// copyArchiveDir duplicates an archive directory, entries and all.
func copyArchiveDir(t *testing.T, from, to string) {
	t.Helper()

	if err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}

		dest := filepath.Join(to, rel)

		if d.IsDir() {
			return os.MkdirAll(dest, 0o700)
		}

		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		return os.WriteFile(dest, body, 0o600)
	}); err != nil {
		t.Fatalf("copy %s to %s: %v", from, to, err)
	}
}
