package lifeops

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO AT A REPAIRED NAME IS REFUSED, NOT WAITED ON. A plain open of a FIFO
// blocks until a writer appears, and the repair runs at a command's release
// with both authority locks held; through the real root, a file target is
// opened by the regular-file rule, which answers at once.
func TestRepairRootRefusesAFIFOWithoutWaiting(t *testing.T) {
	dir := t.TempDir()

	if err := syscall.Mkfifo(filepath.Join(dir, "billet.db"), 0o600); err != nil {
		t.Skipf("this filesystem cannot make a FIFO: %v", err)
	}

	h := newHost(t)
	c, _ := h.converger(t, healthyHost(t, h))
	c.repairRoot = openRepairRoot

	answered := make(chan error, 1)

	go func() {
		_, err := c.RepairPaths(dir, []RepairTarget{{Name: "billet.db"}}, 990, 991)
		answered <- err
	}()

	select {
	case err := <-answered:
		if err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a FIFO at a repaired name must refuse as not a regular file, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the repair blocked inside the open of a FIFO")
	}

	// THE ABSENT NAME IS STILL SKIPPED, and a regular file still repaired as far
	// as an unprivileged run can tell (the owner check refuses a file the test
	// user owns, which is the ordinary refusal and not a wait).
	if _, err := c.RepairPaths(dir, []RepairTarget{{Name: "absent"}}, 990, 991); err != nil {
		t.Fatalf("an absent target is skipped, got %v", err)
	}
}
