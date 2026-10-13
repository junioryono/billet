package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/archivestore"
	"github.com/junioryono/billet/internal/deployarchive"
)

// useBackupDir stands a fresh directory in for the packaged backup directory.
func useBackupDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	original := localBackupDir
	localBackupDir = dir

	t.Cleanup(func() { localBackupDir = original })

	return dir
}

// seedArchive writes one real archive outside the backup directory, for
// stageArchive to copy.
func seedArchive(t *testing.T, f backupFixture) string {
	t.Helper()

	seed := filepath.Join(t.TempDir(), "seed")

	if err := cmdLocalBackup(t.Context(), processEnv(), []string{
		"--config", f.configPath, "--out", seed, "--no-upload",
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	return seed
}

// stageArchive copies the seed into the backup directory as name, created at,
// and when uploaded puts its manifest in the bucket under the name the archive
// gives itself, which is what a finished upload leaves.
func stageArchive(t *testing.T, store *fakeArchiveStore, deployment, seed, name string, at time.Time, uploaded bool) {
	t.Helper()

	dir := filepath.Join(localBackupDir, name)
	copyArchiveDir(t, seed, dir)

	manifestPath := filepath.Join(dir, deployarchive.EntryManifest)

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}

	m["created_at"] = at.UTC().Format(time.RFC3339)

	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}

	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}

	if !uploaded {
		return
	}

	a, err := deployarchive.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("the staged archive is not one: %v", err)
	}

	key := store.Prefix(deployment) + a.Name() + "/" + archivestore.ManifestEntry
	if err := store.Put(t.Context(), key, raw); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func TestOnlyOlderArchivesTheBucketHoldsAreRemoved(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())
	f := newBackupFixture(t, true)
	seed := seedArchive(t, f)
	dir := useBackupDir(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// Eleven archives a day apart: the oldest two never finished uploading,
	// and the bucket holds another manifest under the third one's name.
	for i := range 11 {
		stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), i >= 2)
	}

	impostor, err := deployarchive.Open(t.Context(), filepath.Join(dir, "day-c"))
	if err != nil {
		t.Fatalf("open day-c: %v", err)
	}

	store.objects[store.Prefix(f.deployment)+impostor.Name()+"/"+archivestore.ManifestEntry] = []byte("{}")

	// Not an archive, so never removed whatever its age.
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o700); err != nil {
		t.Fatalf("stage notes: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "notes", "readme"), []byte("mine"), 0o600); err != nil {
		t.Fatalf("stage notes: %v", err)
	}

	// Another deployment's view of the same bucket removes nothing, while day-d
	// is still there to be removed.
	removed, err := pruneArchivesIn(t.Context(), store, "another-deployment", dir)
	if err != nil || len(removed) != 0 {
		t.Fatalf("another deployment removed %v (%v)", removed, err)
	}

	removed, err = pruneArchivesIn(t.Context(), store, f.deployment, dir)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	slices.Sort(removed)

	if want := []string{"day-d"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}

	want := []string{"day-a", "day-b", "day-c", "day-e", "day-f", "day-g", "day-h", "day-i", "day-j", "day-k", "notes"}
	if got := entriesOf(t, dir); !slices.Equal(got, want) {
		t.Fatalf("the directory holds %v, want %v", got, want)
	}
}

func TestABucketThatCannotBeListedRemovesNothing(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())
	f := newBackupFixture(t, true)
	seed := seedArchive(t, f)
	dir := useBackupDir(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for i := range keepLocalArchives + 2 {
		stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), true)
	}

	store.listErr = os.ErrDeadlineExceeded

	if removed, err := pruneArchivesIn(t.Context(), store, f.deployment, dir); err == nil || len(removed) != 0 {
		t.Fatalf("an unlisted bucket removed %v (%v)", removed, err)
	}

	if got := entriesOf(t, dir); len(got) != keepLocalArchives+2 {
		t.Fatalf("the directory holds %v", got)
	}
}

func TestTheTimersBackupPrunesAndAnOperatorsDoesNot(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())
	f := newBackupFixture(t, true)
	seed := seedArchive(t, f)
	dir := useBackupDir(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// One more than the allowance, so a prune that ran would remove one.
	for i := range keepLocalArchives + 1 {
		stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), true)
	}

	// An operator's --out elsewhere leaves the packaged directory alone. It
	// uploads to a copy of the bucket, because two backups in one second carry
	// one name.
	operator := newFakeArchiveStore()
	for key, body := range store.objects {
		operator.objects[key] = body
	}

	withArchiveStore(t, operator)

	if err := cmdLocalBackup(t.Context(), processEnv(), []string{
		"--config", f.configPath, "--out", filepath.Join(t.TempDir(), "mine"),
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	if got := entriesOf(t, dir); len(got) != keepLocalArchives+1 {
		t.Fatalf("an operator's backup pruned the packaged directory: %v", got)
	}

	if len(operator.written()) == 0 {
		t.Fatal("the operator's backup uploaded nothing, so it never reached the prune")
	}

	// The timer's run, into the packaged directory, takes the oldest out.
	withArchiveStore(t, store)

	if err := cmdLocalBackup(t.Context(), processEnv(), []string{
		"--config", f.configPath, "--out", filepath.Join(dir, "today"),
	}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	got := entriesOf(t, dir)
	if len(got) != keepLocalArchives || slices.Contains(got, "day-a") || slices.Contains(got, "day-b") ||
		!slices.Contains(got, "today") {
		t.Fatalf("the directory holds %v, want the newest %d with day-a and day-b gone", got, keepLocalArchives)
	}
}

func TestAnArchiveRemovalCutShortIsFinishedFromItsManifest(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())
	f := newBackupFixture(t, true)
	seed := seedArchive(t, f)
	dir := useBackupDir(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for i := range keepLocalArchives + 1 {
		stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), true)
	}

	// What a removal cut short leaves: a declared entry gone, the manifest not.
	if err := os.Remove(filepath.Join(dir, "day-a", deployarchive.EntryLedger)); err != nil {
		t.Fatalf("remove the ledger: %v", err)
	}

	if _, err := deployarchive.Open(t.Context(), filepath.Join(dir, "day-a")); err == nil {
		t.Fatal("an archive missing its ledger still opens, so this proves nothing about one that does not")
	}

	removed, err := pruneArchivesIn(t.Context(), store, f.deployment, dir)
	if err != nil || !slices.Equal(removed, []string{"day-a"}) {
		t.Fatalf("the prune removed %v (%v), want the cut-short day-a", removed, err)
	}
}

func TestAnArchiveHoldingWhatItsManifestDoesNotDeclareIsKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, archive string)
	}{
		{"an undeclared file", func(t *testing.T, archive string) {
			t.Helper()

			if err := os.WriteFile(filepath.Join(archive, "notes.txt"), []byte("mine"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"an undeclared directory", func(t *testing.T, archive string) {
			t.Helper()

			if err := os.Mkdir(filepath.Join(archive, "extra"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}},
		{"a declared file replaced by a directory", func(t *testing.T, archive string) {
			t.Helper()

			ledger := filepath.Join(archive, deployarchive.EntryLedger)
			if err := os.Remove(ledger); err != nil {
				t.Fatalf("remove: %v", err)
			}

			if err := os.Mkdir(ledger, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}},
		{"a declared file changed", func(t *testing.T, archive string) {
			t.Helper()

			if err := os.WriteFile(filepath.Join(archive, deployarchive.EntryLedger), []byte("changed"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := withArchiveStore(t, newFakeArchiveStore())
			f := newBackupFixture(t, true)
			seed := seedArchive(t, f)
			dir := useBackupDir(t)
			base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

			for i := range keepLocalArchives + 1 {
				stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), true)
			}

			tc.change(t, filepath.Join(dir, "day-a"))

			removed, err := pruneArchivesIn(t.Context(), store, f.deployment, dir)
			if err == nil || len(removed) != 0 {
				t.Fatalf("the prune removed %v (%v), want day-a kept and said", removed, err)
			}

			if _, err := os.Stat(filepath.Join(dir, "day-a", deployarchive.EntryManifest)); err != nil {
				t.Fatalf("day-a was touched: %v", err)
			}
		})
	}
}
