package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/hostupgrade"
)

// useRecoveryRoot points upgradeRoot at a fresh directory and opens it the way
// the transaction lock does, as a descriptor.
func useRecoveryRoot(t *testing.T) *os.File {
	t.Helper()

	root := t.TempDir()
	original := upgradeRoot
	upgradeRoot = root

	t.Cleanup(func() { upgradeRoot = original })

	f, err := os.Open(root)
	if err != nil {
		t.Fatalf("open the root: %v", err)
	}

	t.Cleanup(func() { _ = f.Close() })

	return f
}

// stageRecovery writes a recovery directory shaped like a transaction's: a
// journal at step, last updated at, and a nested preserved tree.
func stageRecovery(t *testing.T, name string, step hostupgrade.Step, updated time.Time) string {
	t.Helper()

	dir := filepath.Join(upgradeRoot, name)

	if err := os.MkdirAll(filepath.Join(dir, "preserved"), 0o700); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}

	body, err := json.Marshal(hostupgrade.Journal{
		Dir: dir, Step: step,
		StartedAt: updated.Add(-time.Minute).Format(time.RFC3339Nano),
		UpdatedAt: updated.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("encode the journal: %v", err)
	}

	for file, content := range map[string][]byte{
		hostupgrade.JournalName:                 body,
		hostupgrade.SnapshotName:                []byte("ledger"),
		filepath.Join("preserved", "billet"):    []byte("binary"),
		filepath.Join("preserved", "unit.conf"): []byte("unit"),
	} {
		if err := os.WriteFile(filepath.Join(dir, file), content, 0o600); err != nil {
			t.Fatalf("stage %s: %v", file, err)
		}
	}

	return dir
}

func entriesOf(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

func TestOnlyTheNewestDecidedRecoveriesAreKept(t *testing.T) {
	root := useRecoveryRoot(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// Six decided transactions: the three newest stay, a rolled-back one among
	// them, and the three oldest go.
	stageRecovery(t, "upgrade-100", hostupgrade.StepCommitted, base.Add(1*time.Hour))
	stageRecovery(t, "upgrade-101", hostupgrade.StepCommitted, base.Add(2*time.Hour))
	stageRecovery(t, "upgrade-102", hostupgrade.StepRolledBack, base.Add(3*time.Hour))
	stageRecovery(t, "upgrade-103", hostupgrade.StepCommitted, base.Add(4*time.Hour))
	stageRecovery(t, "upgrade-104", hostupgrade.StepRolledBack, base.Add(5*time.Hour))
	stageRecovery(t, "upgrade-105", hostupgrade.StepCommitted, base.Add(6*time.Hour))

	// Older than every one of them, and each kept for its own reason.
	stageRecovery(t, "upgrade-1", hostupgrade.StepInstalled, base)
	stageRecovery(t, "20260906T055555262385392", hostupgrade.StepCommitted, base)
	claimed := stageRecovery(t, "upgrade-2", hostupgrade.StepCommitted, base)

	// RELATIVE AND UNCLEAN, which names the same directory as billet's own
	// absolute pointer does.
	if err := os.Symlink("./"+filepath.Base(claimed), filepath.Join(upgradeRoot, activePointer)); err != nil {
		t.Fatalf("publish the claim: %v", err)
	}

	unreadable := stageRecovery(t, "upgrade-3", hostupgrade.StepCommitted, base)
	if err := os.WriteFile(filepath.Join(unreadable, hostupgrade.JournalName), []byte("{"), 0o600); err != nil {
		t.Fatalf("damage the journal: %v", err)
	}

	// A link at a recovery name is not a recovery directory, and a link inside
	// one that goes is removed without what it names.
	outside := t.TempDir()
	stageOutside := filepath.Join(outside, "upgrade-4")

	original := upgradeRoot
	upgradeRoot = outside
	stageRecovery(t, "upgrade-4", hostupgrade.StepCommitted, base)
	upgradeRoot = original

	if err := os.Symlink(stageOutside, filepath.Join(upgradeRoot, "upgrade-5")); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	if err := os.Symlink(stageOutside, filepath.Join(upgradeRoot, "upgrade-100", "preserved", "elsewhere")); err != nil {
		t.Fatalf("plant the inner link: %v", err)
	}

	removed, err := pruneRecoveries(root)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	slices.Sort(removed)

	if want := []string{"upgrade-100", "upgrade-101", "upgrade-102"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}

	want := []string{"20260906T055555262385392", activePointer, "upgrade-1", "upgrade-103", "upgrade-104",
		"upgrade-105", "upgrade-2", "upgrade-3", "upgrade-5"}
	if got := entriesOf(t, upgradeRoot); !slices.Equal(got, want) {
		t.Fatalf("the root holds %v, want %v", got, want)
	}

	if got := entriesOf(t, filepath.Join(stageOutside, "preserved")); len(got) != 2 {
		t.Fatalf("what the links named was touched: %v", got)
	}
}

func TestNoRecoveryIsRemovedWhileTheNewestFitTheAllowance(t *testing.T) {
	root := useRecoveryRoot(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	for i := range keepRecoveries {
		stageRecovery(t, "upgrade-"+string(rune('1'+i)), hostupgrade.StepCommitted, base.Add(time.Duration(i)*time.Hour))
	}

	removed, err := pruneRecoveries(root)
	if err != nil || len(removed) != 0 {
		t.Fatalf("prune removed %v (%v), want nothing", removed, err)
	}

	if got := entriesOf(t, upgradeRoot); len(got) != keepRecoveries {
		t.Fatalf("the root holds %v", got)
	}
}

// committedHost is a host on which a committed transaction's finish succeeds.
type committedHost struct{ hostupgrade.Host }

func (committedHost) RecordInstalled(context.Context, string, string) error { return nil }
func (committedHost) ClearFence(context.Context, string) error              { return nil }
func (committedHost) StartServices(context.Context) error                   { return nil }
func (committedHost) ProveStable(context.Context) error                     { return nil }

func TestAFinishedUpgradeRemovesOlderRecoveries(t *testing.T) {
	root := useRecoveryRoot(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	for i, name := range []string{"upgrade-10", "upgrade-11", "upgrade-12", "upgrade-13"} {
		stageRecovery(t, name, hostupgrade.StepCommitted, base.Add(time.Duration(i)*time.Hour))
	}

	current := stageRecovery(t, "upgrade-14", hostupgrade.StepCommitted, base.Add(10*time.Hour))

	if err := os.Symlink(current, filepath.Join(upgradeRoot, activePointer)); err != nil {
		t.Fatalf("publish the claim: %v", err)
	}

	journal, err := hostupgrade.ReadJournal(current)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}

	var out bytes.Buffer

	env := cli.Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }}

	if err := finishHostUpgrade(t.Context(), env, root, journal, committedHost{}); err != nil {
		t.Fatalf("finish: %v\n%s", err, out.String())
	}

	if got, want := entriesOf(t, upgradeRoot), []string{"upgrade-12", "upgrade-13", "upgrade-14"}; !slices.Equal(got, want) {
		t.Fatalf("the root holds %v, want %v", got, want)
	}

	if !strings.Contains(out.String(), "Removed 2 older recovery directories") {
		t.Fatalf("the removal was not said:\n%s", out.String())
	}
}

func TestADirectoryThatIsNotTheOneJudgedIsKept(t *testing.T) {
	root := useRecoveryRoot(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	stageRecovery(t, "upgrade-1", hostupgrade.StepCommitted, base)
	other := stageRecovery(t, "upgrade-2", hostupgrade.StepCommitted, base)

	judged, err := os.Stat(other)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := removeTreeAt(root, "upgrade-1", judged, hostupgrade.JournalName); err == nil {
		t.Fatal("a directory that is not the one judged was removed")
	}

	if got := entriesOf(t, filepath.Join(upgradeRoot, "upgrade-1")); len(got) == 0 {
		t.Fatal("the directory that was not judged lost its contents")
	}
}

func TestADirectoryReplacedWhileItIsJudgedIsNotTheOneJudged(t *testing.T) {
	root := useRecoveryRoot(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	original := stageRecovery(t, "upgrade-1", hostupgrade.StepCommitted, base)

	_, ok := judgedDirAt(root, "upgrade-1", func() bool {
		if err := os.Rename(original, original+"-moved"); err != nil {
			t.Fatalf("move the judged directory: %v", err)
		}

		stageRecovery(t, "upgrade-1", hostupgrade.StepInstalled, base)

		return true
	})
	if ok {
		t.Fatal("a directory replaced while it was judged was answered as the one judged")
	}

	for _, dir := range []string{original, original + "-moved"} {
		if got := entriesOf(t, dir); len(got) != 3 {
			t.Fatalf("%s holds %v, want it untouched", dir, got)
		}
	}
}

// lockDir makes a directory's entries unremovable until the test ends, which
// only an account without root's override can be held to.
func lockDir(t *testing.T, dir string) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root removes entries from a directory it cannot write")
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("lock %s: %v", dir, err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("unlock %s: %v", dir, err)
		}
	})
}

func TestARemovalCutShortKeepsItsJournalAndIsFinishedNextTime(t *testing.T) {
	root := useRecoveryRoot(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	for i, name := range []string{"upgrade-1", "upgrade-2", "upgrade-3", "upgrade-4"} {
		stageRecovery(t, name, hostupgrade.StepCommitted, base.Add(time.Duration(i)*time.Hour))
	}

	locked := filepath.Join(upgradeRoot, "upgrade-1", "preserved")
	lockDir(t, locked)

	if removed, err := pruneRecoveries(root); err == nil || len(removed) != 0 {
		t.Fatalf("a removal that could not finish answered %v (%v)", removed, err)
	}

	if _, err := os.Stat(filepath.Join(upgradeRoot, "upgrade-1", hostupgrade.JournalName)); err != nil {
		t.Fatalf("the journal went before the removal finished: %v", err)
	}

	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	removed, err := pruneRecoveries(root)
	if err != nil || !slices.Equal(removed, []string{"upgrade-1"}) {
		t.Fatalf("the next pass removed %v (%v), want upgrade-1", removed, err)
	}
}

func TestTheProofIsLeftForLast(t *testing.T) {
	useRecoveryRoot(t)
	dir := stageRecovery(t, "upgrade-1", hostupgrade.StepCommitted, time.Now())

	f, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = f.Close() }()

	if err := removeContents(f, dir, hostupgrade.JournalName, 0); err != nil {
		t.Fatalf("remove the contents: %v", err)
	}

	if got := entriesOf(t, dir); !slices.Equal(got, []string{hostupgrade.JournalName}) {
		t.Fatalf("the directory holds %v, want the journal alone", got)
	}
}

// recordSyncs replaces the flush seam for one test with one that records each
// directory flushed and what it held then, and fails the flush of fail.
func recordSyncs(t *testing.T, fail string) *[]string {
	t.Helper()

	var flushed []string

	saved := guardSync
	guardSync = func(f *os.File) error {
		var held []string
		if entries, err := os.ReadDir(f.Name()); err == nil {
			for _, e := range entries {
				held = append(held, e.Name())
			}
		}

		flushed = append(flushed, f.Name()+": "+strings.Join(held, ","))

		if f.Name() == fail {
			return errors.New("the flush failed")
		}

		return saved(f)
	}

	t.Cleanup(func() { guardSync = saved })

	return &flushed
}

func TestTheProofOutlivesAFlushThatFailed(t *testing.T) {
	root := useRecoveryRoot(t)
	dir := stageRecovery(t, "upgrade-1", hostupgrade.StepCommitted, time.Now())

	judged, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	recordSyncs(t, dir)

	if err := removeTreeAt(root, "upgrade-1", judged, hostupgrade.JournalName); err == nil {
		t.Fatal("a removal whose flush failed reported success")
	}

	if got := entriesOf(t, dir); !slices.Equal(got, []string{hostupgrade.JournalName}) {
		t.Fatalf("the directory holds %v, want the journal alone", got)
	}
}

func TestARemovalFlushesTheDirectoryThenItsParent(t *testing.T) {
	root := useRecoveryRoot(t)
	dir := stageRecovery(t, "upgrade-1", hostupgrade.StepCommitted, time.Now())

	judged, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	flushed := recordSyncs(t, "")

	if err := removeTreeAt(root, "upgrade-1", judged, hostupgrade.JournalName); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// The directory is flushed holding the journal alone, then the root once it
	// no longer holds the directory.
	want := []string{dir + ": " + hostupgrade.JournalName, root.Name() + ": "}
	if !slices.Equal(*flushed, want) {
		t.Fatalf("flushed %v, want %v", *flushed, want)
	}

	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the directory is still there: %v", err)
	}
}

// recoveriesIn is the recovery directories under the upgrade root, without the
// marks and the lock the timer leaves beside them.
func recoveriesIn(t *testing.T) []string {
	t.Helper()

	var names []string

	for _, name := range entriesOf(t, upgradeRoot) {
		if recoveryTempName.MatchString(name) {
			names = append(names, name)
		}
	}

	return names
}

// THE FIRST UPGRADE ONTO A RELEASE THAT PRUNES IS CARRIED OUT BY ONE THAT DOES
// NOT, so the timer's tick that finds nothing to do is where the host's
// retention happens, under the lock it already holds.
func TestTheTimersIdleTickRemovesOlderRecoveries(t *testing.T) {
	runningAs(t, "v0.9.4")

	cfg, _ := ledgerWithRollout(t, "v0.9.4")
	cfgPath := writeConfigFor(t, cfg)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	for i, name := range []string{"upgrade-10", "upgrade-11", "upgrade-12", "upgrade-13", "upgrade-14"} {
		stageRecovery(t, name, hostupgrade.StepCommitted, base.Add(time.Duration(i)*time.Hour))
	}

	out := capture(t, func() {
		if err := hostUpgradeFromRollout(t.Context(), processEnv(), cfg, cfgPath, false); err != nil {
			t.Errorf("the timer: %v", err)
		}
	})

	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("the timer did not settle as nothing to do:\n%s", out)
	}

	if got, want := recoveriesIn(t), []string{"upgrade-12", "upgrade-13", "upgrade-14"}; !slices.Equal(got, want) {
		t.Fatalf("the root holds %v, want %v", got, want)
	}

	if !strings.Contains(out, "Removed 2 older recovery directories") {
		t.Fatalf("the removal was not said:\n%s", out)
	}
}

// A GUARDED HOST IS A CONVERGE'S, and the timer that leaves it alone removes
// nothing from it either.
func TestTheTimerRemovesNothingFromAGuardedHost(t *testing.T) {
	newGuardedFixture(t)
	runningAs(t, "v0.9.4")

	cfg, _ := ledgerWithRollout(t, "v0.9.4")
	cfgPath := writeConfigFor(t, cfg)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	mustHold(t, "ci-1")

	names := []string{"upgrade-10", "upgrade-11", "upgrade-12", "upgrade-13", "upgrade-14"}
	for i, name := range names {
		stageRecovery(t, name, hostupgrade.StepCommitted, base.Add(time.Duration(i)*time.Hour))
	}

	out := capture(t, func() {
		if err := hostUpgradeFromRollout(t.Context(), processEnv(), cfg, cfgPath, false); err != nil {
			t.Errorf("the timer on a guarded host: %v", err)
		}
	})

	if !strings.Contains(out, "guarded by ci-1 since") {
		t.Fatalf("the timer did not see the guard:\n%s", out)
	}

	if got := recoveriesIn(t); !slices.Equal(got, names) {
		t.Fatalf("the guarded root holds %v, want every one of %v", got, names)
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatal(err)
	}
}
