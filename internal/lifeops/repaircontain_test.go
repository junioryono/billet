package lifeops

import (
	"os"
	"path/filepath"
	"strings"
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

	h := newHost(t)
	c, _ := h.converger(t, healthyHost(t, h))
	c.repairRoot = openRepairRoot

	if _, err := c.RepairPaths(dir, []RepairTarget{{Name: "ca/ca.key"}}, 990, 991); err == nil {
		t.Fatal("a nested target reached through a symlinked parent was opened; as root that would " +
			"have chowned another directory's key")
	}

	after, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(before, after) || after.Mode() != before.Mode() {
		t.Errorf("the outside file changed: %v became %v", before.Mode(), after.Mode())
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
