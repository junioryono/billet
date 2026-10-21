package ceph

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A MAPPING WHOSE VOLUME IS IN THE TRASH IS RELEASED: the volume was disposed of,
// and a trashed image cannot be mapped again, so the mapping is a leftover of a
// map the kernel finished after the failed Create trashed the volume (2026-10-03),
// whose owner can no longer be read by name. A volume neither in the pool nor in
// its trash, or a trash that cannot be read, is could-not-tell.
func TestAMappingOfATrashedVolumeIsReleased(t *testing.T) {
	t.Parallel()

	var (
		trashed = volumeNamed(10*time.Hour, "a1")
		absent  = volumeNamed(10*time.Hour, "b2")
	)
	gone := errors.New("rbd: error opening image: (2) No such file or directory")

	c, rig := newMapRig(t, table(row("1", "billet-cache", trashed, "-"), row("2", "billet-cache", absent, "-")),
		naming(), nil, false)
	rig.metaErr[trashed], rig.metaErr[absent] = gone, gone
	// THE SAME NAME WITH ANOTHER NONCE IS NOT THE VOLUME.
	rig.trashed = []string{volumeNamed(10*time.Hour, "c3"), trashed}

	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.nextPass()

	report, err := c.SweepKernelMappings(t.Context())
	if err == nil || !strings.Contains(err.Error(), absent) || !strings.Contains(err.Error(), "neither in the pool") {
		t.Errorf("a mapped volume neither in the pool nor in its trash was not could-not-tell: %v", err)
	}
	if want := []string{"--id site-reader device unmap billet-cache/" + trashed}; !slices.Equal(rig.unmaps, want) {
		t.Fatalf("the sweep unmapped %q, want only the trashed volume's mapping", rig.unmaps)
	}

	states := statesOf(report)
	if states["/dev/rbd1 billet-cache//"+trashed+"@-"] != MappingUnmapped ||
		states["/dev/rbd2 billet-cache//"+absent+"@-"] != MappingUnknown {
		t.Errorf("states %v", states)
	}

	// AND A TRASH THAT CANNOT BE READ PROVES NOTHING.
	unreadable, rig2 := newMapRig(t, table(row("1", "billet-cache", trashed, "-")), naming(), nil, false)
	rig2.metaErr[trashed] = gone
	rig2.trashErr = errors.New("rbd: list: (110) Connection timed out")

	if _, err := unreadable.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig2.nextPass()

	if _, err := unreadable.SweepKernelMappings(t.Context()); err == nil || !strings.Contains(err.Error(), "(110)") {
		t.Errorf("an unreadable trash was not reported: %v", err)
	}
	if len(rig2.unmaps) != 0 {
		t.Errorf("a mapping was released on a trash that could not be read: %q", rig2.unmaps)
	}
}

// NO UNMAP STARTS WHEN THE PASS IS CANCELLED WHILE ITS OWNER IS READ: the read
// is the last thing before the unmap is detached from the pass, and a stop that
// came during it is already spending its wait.
func TestNoUnmapStartsWhenThePassIsCancelledDuringTheOwnerRead(t *testing.T) {
	t.Parallel()

	leaked := volumeNamed(10*time.Hour, "a1")
	c, rig := newMapRig(t, table(row("1", "billet-cache", leaked, "-")), naming(), nil, false)

	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.nextPass()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rig.onMeta = cancel

	report, err := c.SweepKernelMappings(ctx)
	if err == nil || !strings.Contains(err.Error(), "stopped before unmapping") {
		t.Errorf("a pass cancelled during the owner read did not say so: %v", err)
	}
	if len(rig.unmaps) != 0 {
		t.Errorf("an unmap started after the pass was cancelled: %q", rig.unmaps)
	}
	if got := report.Count(MappingUnknown); got != 1 {
		t.Errorf("%d could-not-tell, want the mapping whose unmap never started", got)
	}
}
