package power

import (
	"log/slog"
	"syscall"
	"testing"
)

// A RELEASE UNLOCKS THE HOST EVEN WHILE A DUPLICATE OF ITS DESCRIPTOR IS OPEN,
// as a child forked between the claim and the release holds one.
func TestAReleaseUnlocksTheHostWhileADuplicateIsOpen(t *testing.T) {
	h := newFakeHost(t, 1)
	c, _ := newController(t, h, capBoth, nil)

	c.mu.Lock()
	dup, err := syscall.Dup(int(c.host.Fd()))
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(dup) }()

	c.ReleaseHost()
	other, err := New(Options{SysfsRoot: h.root, StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.ClaimHost(); err != nil {
		t.Fatalf("the claim after a release with a duplicate still open: %v", err)
	}
	other.ReleaseHost()
}

// A RESTORE-ONLY CONTROLLER THAT COULD NOT CLAIM THE HOST CLAIMS IT ONCE IT IS
// FREE, restores the leftover, and gives the host back once nothing is owed.
func TestARestoreOnlyControllerClaimsWhenItCanAndGivesTheHostBack(t *testing.T) {
	h := newFakeHost(t, 2)
	first, stateDir := newController(t, h, capBoth, nil)
	capNow(t, first)
	first.ReleaseHost() // its process died with the cap in force

	holder, err := New(Options{SysfsRoot: h.root, StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.ClaimHost(); err != nil {
		t.Fatal(err)
	}

	c, err := New(Options{SysfsRoot: h.root, StateDir: stateDir, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Outstanding() {
		t.Fatal("the leftover's record is not outstanding")
	}
	c.step(t.Context(), func() bool { return true })
	h.requireCapped(stateDir)

	holder.ReleaseHost()
	c.step(t.Context(), func() bool { return true })
	h.requireFull(stateDir)
	if err := holder.ClaimHost(); err != nil {
		t.Fatalf("the restore-only controller kept the host after restoring: %v", err)
	}

	// WITH NOTHING OWED IT NEITHER CLAIMS NOR REPORTS, however long another
	// process holds the host.
	c.step(t.Context(), func() bool { return true })
	if c.claimed() || !c.failing.IsZero() {
		t.Fatalf("a restore-only controller with nothing owed claimed (%v) or reported a failure (%v)",
			c.claimed(), c.failing)
	}
	holder.ReleaseHost()
}

// A TICK THAT RUNS AFTER CLOSE DOES NOTHING: it neither claims the host back
// nor writes, so a successor's claim is never refused by a closed controller.
func TestATickAfterCloseLeavesTheHostFree(t *testing.T) {
	h := newFakeHost(t, 1)
	c, stateDir := newController(t, h, capBoth, nil)
	capNow(t, c)
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h.requireFull(stateDir)

	c.step(t.Context(), func() bool { return true })
	if c.claimed() {
		t.Fatal("a tick after Close claimed the host again")
	}
	other, err := New(Options{SysfsRoot: h.root, StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.ClaimHost(); err != nil {
		t.Fatalf("a successor's claim after the closed controller's tick: %v", err)
	}
	other.ReleaseHost()
}
