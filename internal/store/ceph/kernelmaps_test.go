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

// kernelMapsNow is the clock a kernel-mapping test starts at.
var kernelMapsNow = time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)

// volumeNamed is a writable cache volume's name, as cacheName gives it, named
// ago before kernelMapsNow.
func volumeNamed(ago time.Duration, nonce string) string {
	return fmt.Sprintf("cache-v-%d-%s", kernelMapsNow.Add(-ago).Unix(), padNonce(nonce))
}

// padNonce is a 24-hex-digit nonce ending in short.
func padNonce(short string) string { return strings.Repeat("0", 24-len(short)) + short }

// mapRig is a client over a fake rbd whose mapping table is table, and which
// records every unmap it is asked for, whether its context was live and how
// long it was given.
type mapRig struct {
	mu        sync.Mutex
	now       time.Time
	unmaps    []string
	liveCtx   []bool
	deadlines []time.Duration
	lists     int
	// listsAtSessions is how many tables had been listed when the sessions were
	// read, once per pass.
	listsAtSessions []int
	busy            map[string]bool
	failWith        map[string]error
	onUnmap         func()
	// mappers is the node each volume records as the one that mapped it, absent
	// for rigOwner; noMapper records none, and metaErr fails the read.
	mappers  map[string]string
	noMapper map[string]bool
	metaErr  map[string]error
	// trashed are the names the cache pool's trash lists, and trashErr fails
	// the listing; onMeta runs as an owner read answers.
	trashed  []string
	trashErr error
	onMeta   func()
}

// rigOwner is the node a rig's client maps volumes for.
const rigOwner = "deployment-a/node-1"

func newMapRig(t *testing.T, table string, inSession func(string) bool, readErr error, noReader bool) (*Client, *mapRig) {
	t.Helper()

	rig := &mapRig{now: kernelMapsNow, busy: map[string]bool{}, failWith: map[string]error{},
		mappers: map[string]string{}, noMapper: map[string]bool{}, metaErr: map[string]error{}}
	opts := []Option{WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), WithMapOwner(rigOwner),
		withRunner(func(ctx context.Context, _ string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")

			rig.mu.Lock()
			defer rig.mu.Unlock()

			switch {
			// AS JSON, the only shape the sweep can read: rbd's table is prose.
			case joined == "--id site-reader --format json device list":
				rig.lists++

				return []byte(table), nil
			case joined == "--id site-reader --format json trash list billet-cache":
				if rig.trashErr != nil {
					return nil, rig.trashErr
				}
				entries := make([]string, 0, len(rig.trashed))
				for i, name := range rig.trashed {
					entries = append(entries, fmt.Sprintf(`{"id":"%x","name":%q}`, i+1, name))
				}

				return []byte("[" + strings.Join(entries, ",") + "]"), nil
			case strings.HasPrefix(joined, "--id site-reader --format json image-meta list billet-cache/"):
				name := strings.TrimPrefix(args[len(args)-1], "billet-cache/")
				// A READ STARTED ON A DONE CONTEXT FAILS, as rbd's would; one the
				// pass is cancelled during (onMeta) still answers.
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if rig.onMeta != nil {
					rig.onMeta()
				}
				if err := rig.metaErr[name]; err != nil {
					return nil, err
				}
				if rig.noMapper[name] {
					return []byte(`{"billet.cache.lineage":"1"}`), nil
				}
				mapper, ok := rig.mappers[name]
				if !ok {
					mapper = rigOwner
				}

				return []byte(`{"billet.cache.lineage":"1","billet.cache.mapped_by":"` + mapper + `"}`), nil
			case strings.HasPrefix(joined, "--id site-reader device unmap "):
				spec := args[len(args)-1]
				rig.unmaps = append(rig.unmaps, joined)

				// AFTER THE HOOK, which may cancel the caller: the context this unmap
				// runs on must outlive that.
				if rig.onUnmap != nil {
					rig.onUnmap()
				}

				rig.liveCtx = append(rig.liveCtx, ctx.Err() == nil)
				if deadline, ok := ctx.Deadline(); ok {
					rig.deadlines = append(rig.deadlines, time.Until(deadline))
				} else {
					rig.deadlines = append(rig.deadlines, 0)
				}

				if rig.busy[spec] {
					return nil, errors.New("rbd: sysfs write failed\nrbd: unmap failed: (16) Device or resource busy")
				}

				return nil, rig.failWith[spec]
			}

			return nil, fmt.Errorf("unexpected rbd call: %s", joined)
		})}
	if !noReader {
		opts = append(opts, WithCacheSessions(func() (func(string) bool, error) {
			rig.mu.Lock()
			rig.listsAtSessions = append(rig.listsAtSessions, rig.lists)
			rig.mu.Unlock()

			return inSession, readErr
		}))
	}

	c, err := New(valid(), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.clock = func() time.Time {
		rig.mu.Lock()
		defer rig.mu.Unlock()

		return rig.now
	}

	return c, rig
}

// nextPass moves the rig's clock on to the next pass that may unmap.
func (r *mapRig) nextPass() { r.advance(mapSightingGap) }

// advance moves the rig's clock on by d.
func (r *mapRig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.now = r.now.Add(d)
}

func naming(names ...string) func(string) bool {
	return func(name string) bool { return slices.Contains(names, name) }
}

func row(id, pool, name, snap string) string {
	return fmt.Sprintf(`{"id":%q,"pool":%q,"namespace":"","name":%q,"snap":%q,"device":"/dev/rbd%s"}`,
		id, pool, name, snap, id)
}

func table(rows ...string) string { return "[" + strings.Join(rows, ",") + "]" }

func statesOf(report MappingReport) map[string]MappingState {
	states := map[string]MappingState{}
	for _, m := range report.Mappings {
		states[m.Device+" "+m.Pool+"/"+m.Namespace+"/"+m.Name+"@"+m.Snap] = m.State
	}

	return states
}

// ONLY A WRITABLE VOLUME NAMED LONG AGO THAT NO SESSION NAMES, SEEN SO TWICE AN
// HOUR APART, IS UNMAPPED, and by its image, never forced.
//
// The table is the reference node's shape (2026-10-16: root disks and live
// volumes side by side) with every other kind of row a node can hold beside it:
// a generation check, a volume a session names, one named too recently, an
// image-pool map from `images pull` and a writable volume's name in the image
// pool, a name billet does not give, a volume's snapshot, one in another
// namespace, and another pool's image.
func TestTheSweepUnmapsOnlyAWritableVolumeNoSessionNamesSeenTwice(t *testing.T) {
	t.Parallel()

	var (
		leaked     = volumeNamed(10*24*time.Hour, "a1")
		held       = volumeNamed(7*time.Hour, "b2")
		live       = volumeNamed(9*time.Hour, "c3")
		recent     = volumeNamed(4*time.Hour, "d4")
		generation = fmt.Sprintf("cache-g-%d-%s", kernelMapsNow.Add(-time.Hour).Unix(), padNonce("e5"))
		snapshot   = volumeNamed(8*time.Hour, "f6")
		imagePool  = volumeNamed(8*time.Hour, "b8")
		namespaced = volumeNamed(8*time.Hour, "c9")
	)

	c, rig := newMapRig(t, table(
		row("0", "billet-cache", "billet-c5dd400d12e011d8c985f37afbe4e0ca", "-"),
		row("1", "billet-cache", leaked, "-"),
		row("2", "billet-cache", generation, "g1"),
		row("3", "billet-cache", live, "-"),
		row("4", "billet-cache", recent, "-"),
		row("5", "billet-images", "ubuntu-2404-x64", "-"),
		row("6", "billet-cache", "cache-v-1792178999-by-hand", "-"),
		row("7", "billet-cache", leaked+"0", "-"),
		row("8", "billet-cache", held, "-"),
		row("9", "billet-cache", snapshot, "s1"),
		row("10", "rbd", volumeNamed(8*time.Hour, "a7"), "-"),
		row("11", "billet-images", imagePool, "-"),
		row("12", "billet-images", "billet-74d15afc85a9a2152d8137eeda6229f2", "-"),
		`{"id":"13","pool":"billet-cache","namespace":"ci","name":"`+namespaced+`","snap":"-","device":"/dev/rbd13"}`,
	), naming(live), nil, false)
	rig.busy["billet-cache/"+held] = true

	// THE FIRST SIGHTING UNMAPS NOTHING: a clock stepped forward would make a
	// volume still waiting for its record look old.
	first, err := c.SweepKernelMappings(t.Context())
	if err != nil {
		t.Fatalf("the first pass, which unmaps nothing, failed: %v", err)
	}
	if len(rig.unmaps) != 0 {
		t.Fatalf("the first sighting unmapped %q", rig.unmaps)
	}
	if got := first.Count(MappingUnaccounted); got != 2 {
		t.Errorf("the first pass found %d unaccounted, want the leaked and the held one", got)
	}

	rig.nextPass()

	report, err := c.SweepKernelMappings(t.Context())
	if err == nil || !strings.Contains(err.Error(), held) || !strings.Contains(err.Error(), "holds it open") {
		t.Fatalf("a mapping the kernel holds open was not reported by name: %v", err)
	}
	if strings.Contains(err.Error(), leaked+" ") {
		t.Errorf("a mapping that was released is reported as a failure: %v", err)
	}

	want := []string{
		"--id site-reader device unmap billet-cache/" + leaked,
		"--id site-reader device unmap billet-cache/" + held,
	}
	if !slices.Equal(rig.unmaps, want) {
		t.Fatalf("the sweep unmapped\n  %q\nwant exactly\n  %q", rig.unmaps, want)
	}

	// NEVER CUT SHORT BY THE CALLER, AND GIVEN A MAP'S BOUND, not rbd's ordinary
	// fifteen seconds: an unmap does a map's kernel work.
	for i, live := range rig.liveCtx {
		if !live || rig.deadlines[i] <= DefaultTimeout {
			t.Errorf("unmap %d ran on a context live %v with %v left, want live with more than %v",
				i, live, rig.deadlines[i], DefaultTimeout)
		}
	}

	if !slices.Equal(rig.listsAtSessions, []int{0, 1}) {
		t.Errorf("the sessions were read after %v tables, want before each pass's table", rig.listsAtSessions)
	}

	states := statesOf(report)
	for spec, want := range map[string]MappingState{
		"/dev/rbd0 billet-cache//billet-c5dd400d12e011d8c985f37afbe4e0ca@-":   MappingRootDisk,
		"/dev/rbd1 billet-cache//" + leaked + "@-":                            MappingUnmapped,
		"/dev/rbd2 billet-cache//" + generation + "@g1":                       MappingGeneration,
		"/dev/rbd3 billet-cache//" + live + "@-":                              MappingInSession,
		"/dev/rbd4 billet-cache//" + recent + "@-":                            MappingRecent,
		"/dev/rbd5 billet-images//ubuntu-2404-x64@-":                          MappingOther,
		"/dev/rbd6 billet-cache//cache-v-1792178999-by-hand@-":                MappingOther,
		"/dev/rbd7 billet-cache//" + leaked + "0@-":                           MappingOther,
		"/dev/rbd8 billet-cache//" + held + "@-":                              MappingHeld,
		"/dev/rbd9 billet-cache//" + snapshot + "@s1":                         MappingOther,
		"/dev/rbd11 billet-images//" + imagePool + "@-":                       MappingOther,
		"/dev/rbd12 billet-images//billet-74d15afc85a9a2152d8137eeda6229f2@-": MappingOther,
		"/dev/rbd13 billet-cache/ci/" + namespaced + "@-":                     MappingOther,
	} {
		if states[spec] != want {
			t.Errorf("%s: %q, want %q", spec, states[spec], want)
		}
	}

	if len(report.Mappings) != 13 {
		t.Errorf("the report holds %d mappings, want the 13 in billet's pools and none of another pool's",
			len(report.Mappings))
	}
}

// THE AGE IS SIX HOURS BY THE NAME, AND NO LESS: a volume named a second short
// of it may still be waiting for its record.
func TestAVolumeIsJudgedOnlyOnceItsNameIsSixHoursOld(t *testing.T) {
	t.Parallel()

	young, old := volumeNamed(6*time.Hour-time.Second, "a1"), volumeNamed(6*time.Hour, "b2")
	c, rig := newMapRig(t, table(row("1", "billet-cache", young, "-"), row("2", "billet-cache", old, "-")),
		naming(), nil, false)

	report, err := c.KernelMappings(t.Context(), naming())
	if err != nil {
		t.Fatalf("KernelMappings: %v", err)
	}

	states := statesOf(report)
	if states["/dev/rbd1 billet-cache//"+young+"@-"] != MappingRecent ||
		states["/dev/rbd2 billet-cache//"+old+"@-"] != MappingUnaccounted {
		t.Errorf("a volume a second short of six hours and one of exactly six read %v", states)
	}
	if len(rig.unmaps) != 0 {
		t.Errorf("reading the mappings unmapped %q", rig.unmaps)
	}
}

// A MAPPING ANY PASS FOUND ACCOUNTED FOR STARTS ITS SIGHTINGS AGAIN: what is
// proved is one mapping unaccounted for an hour.
func TestASightingStartsAgainWhenTheMappingWasAccountedFor(t *testing.T) {
	t.Parallel()

	volume := volumeNamed(10*time.Hour, "a1")
	recorded := false
	c, rig := newMapRig(t, table(row("1", "billet-cache", volume, "-")), nil, nil, true)
	c.sessions = func() (func(string) bool, error) {
		return func(string) bool { return recorded }, nil
	}

	for pass, isRecorded := range []bool{false, true, false} {
		recorded = isRecorded
		if _, err := c.SweepKernelMappings(t.Context()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		rig.nextPass()
	}

	if len(rig.unmaps) != 0 {
		t.Errorf("a mapping seen unaccounted, then held by a session, then unaccounted again was unmapped: %q",
			rig.unmaps)
	}

	// AND THE NEXT PASS, an hour after the second sighting, releases it.
	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rig.unmaps) != 1 {
		t.Errorf("a mapping unaccounted at two passes an hour apart was not released: %q", rig.unmaps)
	}
}

// AN IMAGE MAPPED TWICE LOSES ONE MAPPING A PASS: `rbd device unmap <spec>`
// releases whichever comes first, so a second unmap in the same pass would act
// on the second row's mapping under the first row's verdict.
func TestAnImageMappedTwiceIsUnmappedOnceAPass(t *testing.T) {
	t.Parallel()

	volume := volumeNamed(10*time.Hour, "a1")
	c, rig := newMapRig(t, table(row("1", "billet-cache", volume, "-"), row("2", "billet-cache", volume, "-")),
		naming(), nil, false)

	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.nextPass()

	report, err := c.SweepKernelMappings(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(rig.unmaps) != 1 {
		t.Errorf("one pass unmapped %q, want one unmap of the image", rig.unmaps)
	}
	if report.Count(MappingUnmapped) != 1 || report.Count(MappingUnaccounted) != 1 {
		t.Errorf("want one mapping released and the other left for the next pass: %+v", report.Mappings)
	}
}

// A PASS THAT CANNOT READ THE SESSIONS UNMAPS NOTHING: without them a live
// job's volume is indistinguishable from a leaked one.
func TestTheSweepUnmapsNothingWithoutTheSessions(t *testing.T) {
	t.Parallel()

	rows := table(row("1", "billet-cache", volumeNamed(10*24*time.Hour, "a1"), "-"))

	for name, c := range map[string]struct {
		inSession func(string) bool
		readErr   error
		noReader  bool
		says      string
	}{
		"no reader": {noReader: true, says: "given no way to read"},
		// WITH A JUDGE BESIDE THE ERROR, which says nothing is named: the error
		// alone must refuse the pass.
		"an unreadable record": {inSession: naming(),
			readErr: errors.New("node: read cache custody entry 1 of 2: permission denied"), says: "permission denied"},
		"a reader that returns none": {says: "the reader returned none"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, rig := newMapRig(t, rows, c.inSession, c.readErr, c.noReader)

			for range 2 {
				if _, err := client.SweepKernelMappings(t.Context()); err == nil ||
					!strings.Contains(err.Error(), "kernel mappings are kept") || !strings.Contains(err.Error(), c.says) {
					t.Errorf("the sweep did not refuse saying %q: %v", c.says, err)
				}
				rig.nextPass()
			}
			if len(rig.unmaps) != 0 {
				t.Errorf("the sweep unmapped %q without knowing which volumes the sessions hold", rig.unmaps)
			}
		})
	}
}

// A MAPPING TABLE THAT CANNOT BE READ IS NOT AN EMPTY ONE, and a row that does
// not say what it maps is not somebody else's.
func TestAMappingTableThatCannotBeReadIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()

	old := volumeNamed(10*time.Hour, "a1")

	for name, c := range map[string]struct{ table, says string }{
		"null":               {"null", "did not answer with a json device list"},
		"not json":           {"rbdN mapped", "did not answer with a json device list"},
		"a row with no name": {`[{"id":"1","pool":"billet-cache","name":"","snap":"-","device":"/dev/rbd1"}]`, "without its pool and image"},
		"a row with no pool": {`[{"id":"1","pool":"","name":"cache-v-1-x","snap":"-","device":"/dev/rbd1"}]`, "without its pool and image"},
		"a null row":         {`[null]`, "without its pool and image"},
		"a row with no snap": {`[{"id":"1","pool":"billet-cache","name":"` + old + `","device":"/dev/rbd1"}]`, "which snapshot it maps"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, _ := newMapRig(t, c.table, naming(), nil, false)

			report, err := client.KernelMappings(t.Context(), naming())
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("an unreadable table read as %+v, %v; want an error saying %q", report, err, c.says)
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
	c, rig := newMapRig(t, table(row("1", "billet-cache", first, "-"), row("2", "billet-cache", second, "-")),
		naming(), nil, false)

	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.nextPass()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

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
	c, rig := newMapRig(t, table(row("1", "billet-cache", leaked, "-")), naming(), nil, false)
	rig.failWith["billet-cache/"+leaked] = errors.New("rbd: unmap failed: (108) Cannot send after transport endpoint shutdown")

	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.nextPass()

	report, err := c.SweepKernelMappings(t.Context())
	if err == nil || !strings.Contains(err.Error(), leaked) || !strings.Contains(err.Error(), "(108)") {
		t.Errorf("a failed unmap was not reported by name and cause: %v", err)
	}
	if got := report.Count(MappingUnknown); got != 1 {
		t.Errorf("%d could-not-tell, want 1: %+v", got, report.Mappings)
	}
}

// A SECOND SIGHTING UNMAPS ONLY AN HOUR AFTER THE FIRST, whatever the passes
// between: a pass at once, or a second short of the hour, unmaps nothing.
func TestASecondSightingUnmapsOnlyAnHourAfterTheFirst(t *testing.T) {
	t.Parallel()

	leaked := volumeNamed(10*time.Hour, "a1")
	c, rig := newMapRig(t, table(row("1", "billet-cache", leaked, "-")), naming(), nil, false)

	for _, step := range []time.Duration{0, 0, 59*time.Minute + 59*time.Second} {
		rig.advance(step)
		if _, err := c.SweepKernelMappings(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(rig.unmaps) != 0 {
			t.Fatalf("a mapping first seen unaccounted %v ago was unmapped: %q",
				rig.now.Sub(kernelMapsNow), rig.unmaps)
		}
	}

	rig.advance(time.Second)
	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rig.unmaps) != 1 {
		t.Errorf("a mapping unaccounted for an hour by two sightings was not released: %q", rig.unmaps)
	}

	// A RELEASED MAPPING THE TABLE STILL LISTS, as a second mapping of the image
	// would be, starts its hour again rather than being unmapped at once.
	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rig.unmaps) != 1 {
		t.Errorf("a mapping listed again after its release was unmapped without a fresh hour: %q", rig.unmaps)
	}
}

// ONLY THIS NODE'S VOLUME IS UNMAPPED: the mapping table is the host's and the
// custody records this node's, so another identity's volume on the same host is
// in none of them, and only the record of who mapped it tells the two apart. A
// volume that records nobody, or another node, is kept without a failure; one
// whose record cannot be read is could-not-tell.
func TestTheSweepUnmapsOnlyAVolumeThisNodeMapped(t *testing.T) {
	t.Parallel()

	var (
		mine       = volumeNamed(10*time.Hour, "a1")
		theirs     = volumeNamed(10*time.Hour, "b2")
		unrecorded = volumeNamed(10*time.Hour, "c3")
		unreadable = volumeNamed(10*time.Hour, "d4")
	)

	c, rig := newMapRig(t, table(
		row("1", "billet-cache", mine, "-"), row("2", "billet-cache", theirs, "-"),
		row("3", "billet-cache", unrecorded, "-"), row("4", "billet-cache", unreadable, "-"),
	), naming(), nil, false)
	rig.mappers[theirs] = "deployment-b/node-1"
	rig.noMapper[unrecorded] = true
	// NOT ENOENT, which asks the trash: a read that failed otherwise says nothing.
	rig.metaErr[unreadable] = errors.New("rbd: image-meta list: (110) Connection timed out")

	if _, err := c.SweepKernelMappings(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.nextPass()

	report, err := c.SweepKernelMappings(t.Context())
	if err == nil || !strings.Contains(err.Error(), unreadable) || !strings.Contains(err.Error(), "read which node mapped") ||
		strings.Contains(err.Error(), theirs) || strings.Contains(err.Error(), unrecorded) {
		t.Errorf("want only the unreadable owner reported as a failure: %v", err)
	}

	if want := []string{"--id site-reader device unmap billet-cache/" + mine}; !slices.Equal(rig.unmaps, want) {
		t.Fatalf("the sweep unmapped %q, want only this node's volume", rig.unmaps)
	}

	states := statesOf(report)
	for name, want := range map[string]MappingState{
		"/dev/rbd1 billet-cache//" + mine:       MappingUnmapped,
		"/dev/rbd2 billet-cache//" + theirs:     MappingNotMine,
		"/dev/rbd3 billet-cache//" + unrecorded: MappingNotMine,
		"/dev/rbd4 billet-cache//" + unreadable: MappingUnknown,
	} {
		if got := states[name+"@-"]; got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// A CLIENT THAT NAMES NO NODE UNMAPS NOTHING: nothing could prove a volume its.
func TestTheSweepUnmapsNothingWithoutAnOwner(t *testing.T) {
	t.Parallel()

	c, rig := newMapRig(t, table(row("1", "billet-cache", volumeNamed(10*time.Hour, "a1"), "-")),
		naming(), nil, false)
	c.owner = ""

	for range 2 {
		if _, err := c.SweepKernelMappings(t.Context()); err == nil || !strings.Contains(err.Error(), "names no node") {
			t.Errorf("a client with no owner swept: %v", err)
		}
		rig.nextPass()
	}
	if len(rig.unmaps) != 0 {
		t.Errorf("a client with no owner unmapped %q", rig.unmaps)
	}
}
