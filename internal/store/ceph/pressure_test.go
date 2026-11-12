package ceph

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// UNDER PRESSURE THE GENERATIONS UNUSED LONGEST GO, WHATEVER THEIR RETENTION, AND
// ONLY THOSE. Every key here is inside its seven days, which is what the full pass
// waits for; past pressureFill the sweep takes the oldest first and removes each
// one's pointer with it. A generation an active record names stays, and so does
// one used inside pressureIdle, which a job that read its pointer a moment ago may
// be about to clone.
func TestUnderPressureTheGenerationsUnusedLongestGoFirst(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	f := newCacheFake()
	f.poolFill = 0.9
	c := evictingClient(t, f, f.run)
	f.freshVolume(now)

	type generation struct {
		key, handle string
		used        time.Time
	}

	threeDays, twoHours, tenMinutes := now.Add(-72*time.Hour), now.Add(-2*time.Hour), now.Add(-10*time.Minute)
	// THE ONE USED LONGEST AGO WAS CREATED LATER, so its name sorts after the
	// other's, and only an order by use takes it first.
	oldest := generation{"acme/api/oldest", expiredGeneration(now.Add(-96*time.Hour), 'a'), threeDays}
	older := generation{"acme/api/older", expiredGeneration(now.Add(-120*time.Hour), 'b'), twoHours}
	recent := generation{"acme/api/recent", expiredGeneration(tenMinutes, 'c'), tenMinutes}
	held := generation{"acme/api/held", expiredGeneration(threeDays, 'd'), threeDays}

	for _, g := range []generation{older, recent, held, oldest} {
		f.addVolume(g.handle)
		f.metadata[g.handle] = map[string]string{cacheMetaPrefix + "used_at": g.used.Format(time.RFC3339Nano)}
		record := cachePointer{Key: g.key, Generation: "gen-" + g.handle, Handle: g.handle, UsedAt: g.used,
			RetentionHours: 7 * 24}
		setIndexRecord(t, f, c, pointerKey(g.key), record)
		setIndexRecord(t, f, c, generationKey(g.key, "gen-"+g.handle), record)
	}

	active, err := json.Marshal(cacheActive{Key: held.key, Handle: held.handle, Generation: "gen-" + held.handle,
		Expires: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("encode active record: %v", err)
	}
	f.metadata[c.cacheIndex()][activeKey("lease-1")] = string(active)

	report, err := c.EvictUnderPressure(t.Context())
	if err != nil {
		t.Fatalf("EvictUnderPressure: %v", err)
	}

	if !report.Pressed() || report.Fill != 0.9 || report.Threshold != pressureFill || report.Evicted != 2 {
		t.Errorf("report = %+v, want the pool pressed at 0.9 and two generations evicted", report)
	}

	for _, g := range []generation{oldest, older} {
		if f.images[g.handle] {
			t.Errorf("pressure kept %s, unused since %s", g.key, g.used.Format(time.RFC3339))
		}
		if _, ok := f.metadata[c.cacheIndex()][pointerKey(g.key)]; ok {
			t.Errorf("pressure left the pointer of %s, whose generation it removed", g.key)
		}
		if _, ok := f.metadata[c.cacheIndex()][generationKey(g.key, "gen-"+g.handle)]; ok {
			t.Errorf("pressure left the generation record of %s", g.key)
		}
	}

	for g, why := range map[generation]string{
		recent: "a generation used inside pressureIdle",
		held:   "a generation an active record names",
	} {
		if !f.images[g.handle] || f.ranWith("rm", g.handle) {
			t.Errorf("pressure removed %s (%s)", why, g.handle)
		}
		if _, ok := f.metadata[c.cacheIndex()][pointerKey(g.key)]; !ok {
			t.Errorf("pressure removed the pointer of %s (%s)", g.key, why)
		}
	}

	if first, second := f.callIndex("rm", oldest.handle), f.callIndex("rm", older.handle); first < 0 ||
		second < 0 || first > second {
		t.Errorf("pressure removed the generation unused since %s at call %d and the one unused since %s "+
			"at call %d: least recently used first", threeDays.Format(time.RFC3339), first,
			twoHours.Format(time.RFC3339), second)
	}
}

// BELOW ITS THRESHOLD THE POOL KEEPS EVERY GENERATION FOR ITS RETENTION, and past
// it the sweep frees the room: the generation, and in the same sweep the retired
// writer it was published from, which holds its data, by the purge between rounds
// and never under the cache lock. The generation is the real one a publication
// leaves, used two hours ago, inside its key's seven days.
func TestPressureFreesTheRoomOnlyPastItsThreshold(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	key := "acme/api/go"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lease, fence, err := c.acquireWriterAt(t.Context(), key, "lease-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := c.publishCASAt(t.Context(), key, "", candidate, lease, fence, now); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}

	twoHours := now.Add(-2 * time.Hour)
	var pointer cachePointer
	if err := json.Unmarshal([]byte(f.metadata[c.cacheIndex()][pointerKey(key)]), &pointer); err != nil {
		t.Fatalf("decode pointer: %v", err)
	}
	pointer.UsedAt = twoHours
	setIndexRecord(t, f, c, pointerKey(key), pointer)
	setIndexRecord(t, f, c, generationKey(key, candidate.Generation), pointer)
	f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = twoHours.Format(time.RFC3339Nano)

	if len(f.trash) != 1 {
		t.Fatalf("retired writer count = %d, want 1", len(f.trash))
	}

	f.poolFill = pressureFill - 0.01

	report, err := c.EvictUnderPressure(t.Context())
	if err != nil {
		t.Fatalf("EvictUnderPressure below the threshold: %v", err)
	}
	if report.Pressed() || report.Evicted != 0 || !f.images[candidate.Handle] || len(f.trash) != 1 {
		t.Fatalf("below its threshold the pool lost a generation inside its retention: %+v", report)
	}

	f.poolFill = pressureFill

	report, err = c.EvictUnderPressure(t.Context())
	if err != nil {
		t.Fatalf("EvictUnderPressure at the threshold: %v", err)
	}
	if !report.Pressed() || report.Evicted != 1 || f.images[candidate.Handle] {
		t.Fatalf("at its threshold the sweep kept the generation unused for two hours: %+v", report)
	}
	if len(f.trash) != 0 {
		t.Errorf("the sweep left %d retired writers holding the evicted generation's data", len(f.trash))
	}
	if call := trashDeletedUnderLock(f); call != "" {
		t.Errorf("the sweep ran %q under the cache lock", call)
	}
}

// A FILL THAT CANNOT BE READ EVICTS NOTHING. Pressure is never assumed: an
// unanswered `ceph df`, an answer that is not json, one that does not list the
// cache pool, and one with no fill or a fill outside 0 to 1 each return an error
// naming what was wrong and remove no generation, however old.
func TestAPoolFillThatCannotBeReadEvictsNothing(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		dfOut string
		dfErr error
		want  string
	}{
		"ceph df fails":            {dfErr: errors.New("exit status 1: ceph: no monitors"), want: "read how full"},
		"not json":                 {dfOut: "HEALTH_OK", want: "did not describe the pools"},
		"no cache pool":            {dfOut: `{"pools":[{"name":"billet-images","stats":{"percent_used":0.9}}]}`, want: "does not list pool billet-cache"},
		"no fill":                  {dfOut: `{"pools":[{"name":"billet-cache","stats":{}}]}`, want: "no fill between 0 and 1"},
		"a fill past one":          {dfOut: `{"pools":[{"name":"billet-cache","stats":{"percent_used":88.5}}]}`, want: "no fill between 0 and 1"},
		"a negative fill":          {dfOut: `{"pools":[{"name":"billet-cache","stats":{"percent_used":-0.1}}]}`, want: "no fill between 0 and 1"},
		"only another pool listed": {dfOut: `{"pools":[{"name":"other","stats":{"percent_used":0.99}}]}`, want: "does not list pool billet-cache"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			now := time.Now().UTC()
			f := newCacheFake()
			f.dfOut, f.dfErr = tc.dfOut, tc.dfErr
			c := evictingClient(t, f, f.run)

			old := now.Add(-72 * time.Hour)
			handle := expiredGeneration(old, 'a')
			f.addVolume(handle)
			record := cachePointer{Key: "acme/api/go", Generation: "gen-a", Handle: handle, UsedAt: old,
				RetentionHours: 7 * 24}
			setIndexRecord(t, f, c, pointerKey("acme/api/go"), record)

			report, err := c.EvictUnderPressure(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("EvictUnderPressure = %v, want an error saying %q", err, tc.want)
			}
			if report.Evicted != 0 || !f.images[handle] || f.ranWith("rm", handle) {
				t.Errorf("a fill that could not be read removed a generation: %+v", report)
			}
		})
	}
}
