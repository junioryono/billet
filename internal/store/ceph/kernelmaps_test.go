package ceph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// kernelMapsNow is the clock every kernel-mapping test reads.
var kernelMapsNow = time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)

// volumeNamed is a writable cache volume's name, as cacheName gives it, named
// ago before kernelMapsNow.
func volumeNamed(ago time.Duration, nonce string) string {
	return fmt.Sprintf("cache-v-%d-%s", kernelMapsNow.Add(-ago).Unix(), padNonce(nonce))
}

// padNonce is a 24-hex-digit nonce ending in short.
func padNonce(short string) string { return strings.Repeat("0", 24-len(short)) + short }

// mapRig is a client over a fake rbd whose mapping table is table, and which
// records every unmap it is asked for and whether its context was live.
type mapRig struct {
	mu       sync.Mutex
	unmaps   []string
	liveCtx  []bool
	lists    int
	busy     map[string]bool
	failWith map[string]error
	onUnmap  func()
}

func newMapRig(t *testing.T, table string, sessions func() (func(string) bool, error)) (*Client, *mapRig) {
	t.Helper()

	rig := &mapRig{busy: map[string]bool{}, failWith: map[string]error{}}
	opts := []Option{WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(func(ctx context.Context, _ string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")

			rig.mu.Lock()
			defer rig.mu.Unlock()

			switch {
			case strings.Contains(joined, "device list"):
				rig.lists++

				return []byte(table), nil
			case strings.Contains(joined, "device unmap"):
				spec := args[len(args)-1]
				rig.unmaps = append(rig.unmaps, joined)

				// AFTER THE HOOK, which may cancel the caller: the context this unmap
				// runs on must outlive that.
				if rig.onUnmap != nil {
					rig.onUnmap()
				}

				rig.liveCtx = append(rig.liveCtx, ctx.Err() == nil)

				if rig.busy[spec] {
					return nil, errors.New("rbd: sysfs write failed\nrbd: unmap failed: (16) Device or resource busy")
				}

				return nil, rig.failWith[spec]
			}

			return nil, fmt.Errorf("unexpected rbd call: %s", joined)
		})}
	if sessions != nil {
		opts = append(opts, WithCacheSessions(sessions))
	}

	c, err := New(valid(), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.clock = func() time.Time { return kernelMapsNow }

	return c, rig
}

func sessionsNaming(names ...string) func() (func(string) bool, error) {
	return func() (func(string) bool, error) {
		return func(name string) bool { return slices.Contains(names, name) }, nil
	}
}

func row(id, pool, name, snap string) string {
	return fmt.Sprintf(`{"id":%q,"pool":%q,"namespace":"","name":%q,"snap":%q,"device":"/dev/rbd%s"}`,
		id, pool, name, snap, id)
}

// ONLY A WRITABLE VOLUME NAMED LONG AGO THAT NO SESSION NAMES IS UNMAPPED, and
// by its image, never forced.
//
// The table is the reference node's shape (2026-10-16: root disks and live
// volumes side by side) with every other kind of row a node can hold beside it:
// a generation check, a volume a session names, one named moments ago whose
// record may not be written yet, an image-pool map from `images pull`, a name
// billet does not give, a volume's snapshot, and another pool's image.
func TestTheSweepUnmapsOnlyAWritableVolumeNoSessionNamesAndNamedLongAgo(t *testing.T) {
	t.Parallel()

	var (
		leaked     = volumeNamed(10*24*time.Hour, "a1")
		held       = volumeNamed(7*time.Hour, "b2")
		live       = volumeNamed(9*time.Hour, "c3")
		recent     = volumeNamed(VolumeMapAge-time.Minute, "d4")
		generation = fmt.Sprintf("cache-g-%d-%s", kernelMapsNow.Add(-time.Hour).Unix(), padNonce("e5"))
	)

	table := "[" + strings.Join([]string{
		row("0", "billet-cache", "billet-c5dd400d12e011d8c985f37afbe4e0ca", "-"),
		row("1", "billet-cache", leaked, "-"),
		row("2", "billet-cache", generation, "g1"),
		row("3", "billet-cache", live, "-"),
		row("4", "billet-cache", recent, "-"),
		row("5", "billet-images", "ubuntu-2404-x64", "-"),
		row("6", "billet-cache", "cache-v-1792178999-by-hand", "-"),
		row("7", "billet-cache", leaked+"x", "-"),
		row("8", "billet-cache", held, "-"),
		row("9", "billet-cache", volumeNamed(8*time.Hour, "f6"), "s1"),
		row("10", "rbd", volumeNamed(8*time.Hour, "a7"), "-"),
	}, ",") + "]"

	c, rig := newMapRig(t, table, sessionsNaming(live))
	rig.busy["billet-cache/"+held] = true

	report, err := c.SweepKernelMappings(t.Context())
	if err == nil || !strings.Contains(err.Error(), held) || !strings.Contains(err.Error(), "holds it open") {
		t.Fatalf("a mapping the kernel holds open was not reported by name: %v", err)
	}
	if strings.Contains(err.Error(), leaked) {
		t.Errorf("a mapping that was released is reported as a failure: %v", err)
	}

	want := []string{
		"--id site-reader device unmap billet-cache/" + leaked,
		"--id site-reader device unmap billet-cache/" + held,
	}
	if !slices.Equal(rig.unmaps, want) {
		t.Fatalf("the sweep unmapped\n  %q\nwant exactly\n  %q", rig.unmaps, want)
	}

	for _, unmap := range rig.unmaps {
		if strings.Contains(unmap, "force") || strings.Contains(unmap, "/dev/") {
			t.Errorf("an unmap was forced or named a device rather than an image: %s", unmap)
		}
	}

	states := map[string]MappingState{}
	for _, m := range report.Mappings {
		states[m.Pool+"/"+m.Name+"@"+m.Snap] = m.State
	}

	for spec, want := range map[string]MappingState{
		"billet-cache/billet-c5dd400d12e011d8c985f37afbe4e0ca@-": MappingRootDisk,
		"billet-cache/" + leaked + "@-":                          MappingUnmapped,
		"billet-cache/" + generation + "@g1":                     MappingGeneration,
		"billet-cache/" + live + "@-":                            MappingInSession,
		"billet-cache/" + recent + "@-":                          MappingRecent,
		"billet-images/ubuntu-2404-x64@-":                        MappingOther,
		"billet-cache/cache-v-1792178999-by-hand@-":              MappingOther,
		"billet-cache/" + leaked + "x@-":                         MappingOther,
		"billet-cache/" + held + "@-":                            MappingHeld,
		"billet-cache/" + volumeNamed(8*time.Hour, "f6") + "@s1": MappingOther,
	} {
		if states[spec] != want {
			t.Errorf("%s: %q, want %q", spec, states[spec], want)
		}
	}

	if len(report.Mappings) != 10 {
		t.Errorf("the report holds %d mappings, want the 10 in billet's pools and none of another pool's",
			len(report.Mappings))
	}
}

// THE SAME TABLE, READ WITHOUT THE SWEEP, CHANGES NOTHING: what `billet check`
// and the node's gauge count.
func TestReadingTheMappingsUnmapsNothing(t *testing.T) {
	t.Parallel()

	leaked := volumeNamed(VolumeMapAge+time.Minute, "a1")
	c, rig := newMapRig(t, "["+row("1", "billet-cache", leaked, "-")+"]", nil)

	report, err := c.KernelMappings(t.Context(), func(string) bool { return false })
	if err != nil {
		t.Fatalf("KernelMappings: %v", err)
	}
	if got := report.Count(MappingUnaccounted); got != 1 {
		t.Errorf("%d unaccounted mappings, want 1: %+v", got, report.Mappings)
	}
	if len(rig.unmaps) != 0 {
		t.Errorf("reading the mappings unmapped %q", rig.unmaps)
	}
}

// A PASS THAT CANNOT READ THE SESSIONS UNMAPS NOTHING: without them a live
// job's volume is indistinguishable from a leaked one.
func TestTheSweepUnmapsNothingWithoutTheSessions(t *testing.T) {
	t.Parallel()

	table := "[" + row("1", "billet-cache", volumeNamed(10*24*time.Hour, "a1"), "-") + "]"

	for name, sessions := range map[string]func() (func(string) bool, error){
		"no reader": nil,
		// WITH A JUDGE BESIDE THE ERROR, which says nothing is named: the error
		// alone must refuse the pass.
		"an unreadable record": func() (func(string) bool, error) {
			return func(string) bool { return false },
				errors.New("node: read cache custody entry 1 of 2: permission denied")
		},
		"a reader that returns none": func() (func(string) bool, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, rig := newMapRig(t, table, sessions)

			if _, err := c.SweepKernelMappings(t.Context()); err == nil ||
				!strings.Contains(err.Error(), "kernel mappings are kept") {
				t.Errorf("the sweep did not refuse: %v", err)
			}
			if len(rig.unmaps) != 0 {
				t.Errorf("the sweep unmapped %q without knowing which volumes the sessions hold", rig.unmaps)
			}
		})
	}
}

// A MAPPING TABLE THAT CANNOT BE READ IS NOT AN EMPTY ONE.
func TestAMappingTableThatCannotBeReadIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()

	for name, table := range map[string]string{
		"null":                 "null",
		"not json":             "rbdN mapped",
		"a row with no name":   `[{"id":"1","pool":"billet-cache","name":"","snap":"-","device":"/dev/rbd1"}]`,
		"a row with no pool":   `[{"id":"1","pool":"","name":"cache-v-1-x","snap":"-","device":"/dev/rbd1"}]`,
		"a null row among one": `[null]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, _ := newMapRig(t, table, nil)

			report, err := c.KernelMappings(t.Context(), func(string) bool { return false })
			if err == nil {
				t.Errorf("an unreadable table read as %+v", report)
			}
		})
	}
}

// AN UNMAP ALREADY STARTED IS NEVER CANCELLED, AND NONE STARTS AFTER.
//
// A signal cuts the kernel's release short and leaves the image's lock with the
// host's one kernel client, whose break blocklists every device on the host
// (unmapDevice, measured 2026-10-03).
func TestAnUnmapStartedRunsOnAndNoneStartsOnceThePassIsCancelled(t *testing.T) {
	t.Parallel()

	first, second := volumeNamed(8*time.Hour, "a1"), volumeNamed(9*time.Hour, "b2")
	table := "[" + row("1", "billet-cache", first, "-") + "," + row("2", "billet-cache", second, "-") + "]"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	c, rig := newMapRig(t, table, sessionsNaming())
	rig.onUnmap = cancel

	report, err := c.SweepKernelMappings(ctx)
	if err == nil || !strings.Contains(err.Error(), "stopped before unmapping") {
		t.Errorf("a pass cancelled before its second unmap did not say so: %v", err)
	}

	if len(rig.unmaps) != 1 || !strings.HasSuffix(rig.unmaps[0], first) {
		t.Fatalf("unmaps %q, want only the first, started before the cancellation", rig.unmaps)
	}
	if !rig.liveCtx[0] {
		t.Error("the unmap ran on the caller's cancellable context, which a stop would cut short")
	}

	if got := report.Count(MappingUnmapped); got != 1 {
		t.Errorf("%d unmapped, want the first", got)
	}
	if got := report.Count(MappingUnknown); got != 1 {
		t.Errorf("%d could-not-tell, want the second, never started", got)
	}
}

// AN UNMAP THAT FAILS FOR ANY OTHER REASON IS COULD-NOT-TELL, never released.
func TestAnUnmapThatFailsOtherwiseIsCouldNotTell(t *testing.T) {
	t.Parallel()

	leaked := volumeNamed(8*time.Hour, "a1")
	c, rig := newMapRig(t, "["+row("1", "billet-cache", leaked, "-")+"]", sessionsNaming())
	rig.failWith["billet-cache/"+leaked] = errors.New("rbd: unmap failed: (108) Cannot send after transport endpoint shutdown")

	report, err := c.SweepKernelMappings(t.Context())
	if err == nil || !strings.Contains(err.Error(), leaked) {
		t.Errorf("a failed unmap was not reported by name: %v", err)
	}
	if got := report.Count(MappingUnknown); got != 1 {
		t.Errorf("%d could-not-tell, want 1: %+v", got, report.Mappings)
	}
}
