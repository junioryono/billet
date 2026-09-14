package lifeops

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A NESTED TARGET STAYS INSIDE THE ROOT: a symlink planted at an intermediate
// component (`ca` pointing outside the state directory) after the directory's
// own check is not followed to another directory's `ca.key`, because the
// parent is resolved by the root and only the final component by the
// regular-file rule. The outside file keeps its owner and mode.
func TestRepairRootKeepsANestedTargetInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()

	outside := t.TempDir()
	victim := filepath.Join(outside, "ca.key")

	if err := os.WriteFile(victim, []byte("somebody else's key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, filepath.Join(dir, "ca")); err != nil {
		t.Skipf("this filesystem cannot make a symlink: %v", err)
	}

	before, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}

	beforeStat, ok := before.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no ownership in the stat")
	}

	h := newHost(t)
	c, _ := h.converger(t, healthyHost(t, h))
	c.repairRoot = openRepairRoot

	// THE REFUSAL IS THE OPEN'S, not the owner's: the vulnerable open reaches the
	// outside file and repairOne then refuses its owner, which an assertion on
	// "an error happened" would accept. The message must say the open was
	// declined and must not be the ownership diagnostic.
	_, err = c.RepairPaths(dir, []RepairTarget{{Name: "ca/ca.key"}}, 990, 991)
	if err == nil {
		t.Fatal("a nested target reached through a symlinked parent was opened; as root that would " +
			"have chowned another directory's key")
	}

	if strings.Contains(err.Error(), "neither root nor the service account") {
		t.Fatalf("the symlinked parent was FOLLOWED and the outside file refused for its owner instead: %v", err)
	}

	if !strings.Contains(err.Error(), "open") {
		t.Errorf("the refusal does not say the open was declined: %v", err)
	}

	// The production root itself refuses the nested open, before any check.
	root, err := openRepairRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	if f, err := root.OpenRegular("ca/ca.key"); err == nil {
		_ = f.Close()

		t.Fatal("OpenRegular followed the symlinked parent out of the directory")
	}

	after, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}

	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no ownership in the stat")
	}

	if !os.SameFile(before, after) || after.Mode() != before.Mode() ||
		afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("the outside file changed: %v %d:%d became %v %d:%d", before.Mode(), beforeStat.Uid,
			beforeStat.Gid, after.Mode(), afterStat.Uid, afterStat.Gid)
	}

	// A NESTED TARGET UNDER A REAL DIRECTORY IS REACHED: the refusal above is the
	// symlink's, not the nesting's (the owner check refuses a file the test user
	// owns, which is the ordinary refusal for an unprivileged run).
	if err := os.Remove(filepath.Join(dir, "ca")); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(dir, "ca"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "ca", "ca.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = c.RepairPaths(dir, []RepairTarget{{Name: "ca/ca.key"}}, 990, 991)
	if err == nil || !strings.Contains(err.Error(), "neither root nor the service account") {
		t.Fatalf("a nested target under a real directory must be reached and judged on its owner, got %v", err)
	}
}
