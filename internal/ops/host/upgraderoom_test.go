package host

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/provenance"
)

// withFreeBytes stands answer in for the free-space reading.
func withFreeBytes(t *testing.T, answer func(path string) (int64, error)) {
	t.Helper()

	original := freeBytes
	freeBytes = answer

	t.Cleanup(func() { freeBytes = original })
}

// serverWithLedger is a server config whose SQLite ledger and write-ahead log
// hold the sizes given.
func serverWithLedger(t *testing.T, db, wal int) *config.Config {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "billet.db"), make([]byte, db), 0o600); err != nil {
		t.Fatal(err)
	}

	if wal > 0 {
		if err := os.WriteFile(filepath.Join(dir, "billet.db-wal"), make([]byte, wal), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return &config.Config{Server: &config.ServerConfig{IdentityDir: dir}}
}

// THE UPGRADE ASKS FOR THE LEDGER'S SIZE AND THE MARGIN under the upgrade root,
// the write-ahead log included, and nothing of a ledger for a host with no server.
func TestTheUpgradeAsksForTheLedgerAndTheMargin(t *testing.T) {
	withUpgradeRoot(t)

	const db, wal = 3 << 20, 1 << 20

	server := serverWithLedger(t, db, wal)
	need := int64(db + wal + upgradeRoomMargin)

	for _, tc := range []struct {
		name    string
		cfg     *config.Config
		free    int64
		refused bool
	}{
		{"a server with exactly the room", server, need, false},
		{"a server a byte short", server, need - 1, true},
		{"a node with the margin", nodeConfig, upgradeRoomMargin, false},
		{"a node a byte short of the margin", nodeConfig, upgradeRoomMargin - 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked string

			withFreeBytes(t, func(path string) (int64, error) {
				asked = path

				return tc.free, nil
			})

			err := refuseShortOfRoom(tc.cfg)
			if tc.refused != (err != nil) {
				t.Fatalf("refuseShortOfRoom = %v, want refused %v", err, tc.refused)
			}
			if asked != upgradeRoot {
				t.Errorf("the room was read on %q, want the upgrade root %q", asked, upgradeRoot)
			}
			if err != nil && !strings.Contains(err.Error(), mib(need)) && !strings.Contains(err.Error(), mib(upgradeRoomMargin)) {
				t.Errorf("the refusal %q does not say what it needs", err)
			}
		})
	}
}

// A READING THAT FAILS REFUSES, and so does a server whose ledger cannot be
// measured: could not tell is not room.
func TestAnUpgradeThatCannotTellItsRoomIsRefused(t *testing.T) {
	withUpgradeRoot(t)
	withFreeBytes(t, func(string) (int64, error) { return 0, errors.New("statfs: input/output error") })

	if err := refuseShortOfRoom(nodeConfig); err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("an unreadable disk = %v, want the reading's failure", err)
	}

	withFreeBytes(t, func(string) (int64, error) { return 1 << 40, nil })

	missing := &config.Config{Server: &config.ServerConfig{IdentityDir: t.TempDir()}}
	if err := refuseShortOfRoom(missing); err == nil || !strings.Contains(err.Error(), "how large the ledger is") {
		t.Errorf("a ledger that cannot be measured = %v, want it refused", err)
	}
}

// THE ROOM IS JUDGED BEFORE THE CLAIM, so a short disk changes nothing on the
// host: no claim, no stop, and another transaction's claim untouched.
func TestAShortDiskRefusesTheUpgradeBeforeAClaimIsAttempted(t *testing.T) {
	withUpgradeRoot(t)

	prevOS, prevRelease, prevPath := cli.HostOS, runningRelease, provenance.Path
	cli.HostOS = "darwin"
	runningRelease = func() string { return "v0.16.0" }
	provenance.Path = filepath.Join(t.TempDir(), "installed.json")

	t.Cleanup(func() { cli.HostOS, runningRelease, provenance.Path = prevOS, prevRelease, prevPath })

	withFreeBytes(t, func(string) (int64, error) { return upgradeRoomMargin - 1, nil })

	tx := heldTxLock(t)

	other := filepath.Join(upgradeRoot, "upgrade-other")
	if err := os.Symlink(other, filepath.Join(upgradeRoot, activePointer)); err != nil {
		t.Fatal(err)
	}

	err := actOnResolved(t.Context(), processEnv(), nodeConfig, "", hostUpgradeTarget{generation: 10},
		newUpgradeAck(""), nil, candidateManifest(t), strings.Repeat("b", 64), tx)
	if err == nil || !strings.Contains(err.Error(), "refused before anything stops") ||
		strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("the upgrade returned %v, want the room's refusal and nothing of the claim", err)
	}

	if got, err := os.Readlink(filepath.Join(upgradeRoot, activePointer)); err != nil || got != other {
		t.Errorf("the other transaction's claim is %q (err %v), want it untouched", got, err)
	}
}

// A SHORT DISK PRUNES INSIDE THE COUNT, oldest first and only archives the bucket
// holds, until the room is back, and never the newest.
func TestAShortDiskPrunesOlderArchivesUntilTheRoomIsBack(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())
	f := newBackupFixture(t, true)
	seed := seedArchive(t, f)
	dir := useBackupDir(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	id, err := deployarchive.ReadIdentity(seed)
	if err != nil {
		t.Fatal(err)
	}

	size, ok := id.Size()
	if !ok || size <= 0 {
		t.Fatalf("the seed archive declares %d bytes (usable %v), so the room proves nothing", size, ok)
	}

	// Five archives, under the count. day-b never finished uploading.
	for i := range 5 {
		stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), i != 1)
	}

	archivesLeft := func() int {
		n := 0
		for _, e := range entriesOf(t, dir) {
			if strings.HasPrefix(e, "day-") {
				n++
			}
		}

		return n
	}

	// The room comes back once three archives are left: two archives' worth free.
	var elsewhere []string

	withFreeBytes(t, func(path string) (int64, error) {
		if path != dir {
			elsewhere = append(elsewhere, path)
		}

		return int64(5-archivesLeft()) * size, nil
	})

	removed, err := pruneArchivesIn(t.Context(), store, f.deployment, dir)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if len(elsewhere) > 0 {
		t.Errorf("the room was read on %v, want only the backup directory %s", elsewhere, dir)
	}

	slices.Sort(removed)

	if want := []string{"day-a", "day-c"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want the two oldest the bucket holds, day-b kept for its upload", removed)
	}

	if want := []string{"day-b", "day-d", "day-e"}; !slices.Equal(entriesOf(t, dir), want) {
		t.Fatalf("the directory holds %v, want %v", entriesOf(t, dir), want)
	}

	// A disk that never has the room still keeps the newest.
	withFreeBytes(t, func(string) (int64, error) { return 0, nil })

	if _, err := pruneArchivesIn(t.Context(), store, f.deployment, dir); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if want := []string{"day-b", "day-e"}; !slices.Equal(entriesOf(t, dir), want) {
		t.Fatalf("the directory holds %v, want the newest and the one the bucket lacks", entriesOf(t, dir))
	}
}

// A NEWEST ARCHIVE THAT DECLARES NO USABLE SIZE PRUNES ONLY PAST THE COUNT, and
// says so: its manifest has not been checked against its files, and a total that
// overflows would make the room negative and the pruning stop on a full disk.
func TestANewestArchiveWithNoUsableSizePrunesOnlyPastTheCount(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())
	f := newBackupFixture(t, true)
	seed := seedArchive(t, f)
	dir := useBackupDir(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for i := range keepLocalArchives + 1 {
		stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), true)
	}

	newest := filepath.Join(dir, "day-"+string(rune('a'+keepLocalArchives)), deployarchive.EntryManifest)

	raw, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	files, ok := m["files"].([]any)
	if !ok || len(files) < 2 {
		t.Fatalf("the manifest declares %v, want at least two files to overflow", m["files"])
	}

	for _, rec := range files[:2] {
		entry, ok := rec.(map[string]any)
		if !ok {
			t.Fatalf("a file record is %T", rec)
		}

		entry["size"] = int64(1) << 62
	}

	if raw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(newest, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	withFreeBytes(t, func(string) (int64, error) { return 0, nil })

	removed, err := pruneArchivesIn(t.Context(), store, f.deployment, dir)
	if err == nil || !strings.Contains(err.Error(), "declares no usable size") {
		t.Errorf("prune = %v, want the newest archive's size refused and said", err)
	}

	if !slices.Equal(removed, []string{"day-a"}) {
		t.Errorf("removed %v, want only day-a, the one past the count, on a disk reading empty", removed)
	}
}

// A ROOM THAT CANNOT BE READ PRUNES ONLY PAST THE COUNT, and says so.
func TestAnUnreadableRoomPrunesOnlyPastTheCount(t *testing.T) {
	store := withArchiveStore(t, newFakeArchiveStore())
	f := newBackupFixture(t, true)
	seed := seedArchive(t, f)
	dir := useBackupDir(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for i := range keepLocalArchives + 1 {
		stageArchive(t, store, f.deployment, seed, "day-"+string(rune('a'+i)), base.AddDate(0, 0, i), true)
	}

	withFreeBytes(t, func(string) (int64, error) { return 0, errors.New("statfs: input/output error") })

	removed, err := pruneArchivesIn(t.Context(), store, f.deployment, dir)
	if err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Errorf("prune = %v, want the unreadable room said", err)
	}

	if !slices.Equal(removed, []string{"day-a"}) {
		t.Errorf("removed %v, want only day-a, the one past the count", removed)
	}
}
