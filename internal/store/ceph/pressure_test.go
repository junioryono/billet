package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

// UNDER PRESSURE THE GENERATIONS UNUSED LONGEST GO, WHATEVER THEIR RETENTION, AND
// ONLY THOSE. Every key here is inside its seven days, which is what the full pass
// waits for; past pressureFill the sweep takes the oldest first, by the later of
// its pointer's and its generation record's use, since a restore of a named
// generation refreshes the record alone, and removes each one's pointer, so the
// key reads as a miss and a job can save it again. A generation an active record
// names stays, and so does one used inside pressureIdle, which a job that read
// its pointer a moment ago may be about to clone.
func TestUnderPressureTheGenerationsUnusedLongestGoFirst(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	f := newCacheFake()
	f.poolFill = 0.9
	c := evictingClient(t, f, f.run)
	f.freshVolume(now)

	type generation struct {
		key, handle string
		// pointed is when the key's pointer was last read, used when anything,
		// a pointer read or a restore of the generation by name, last read it.
		pointed, used time.Time
	}

	fourDays, threeDays := now.Add(-96*time.Hour), now.Add(-72*time.Hour)
	twoHours, ninetyMinutes, tenMinutes := now.Add(-2*time.Hour), now.Add(-90*time.Minute), now.Add(-10*time.Minute)
	// THE ONE USED LONGEST AGO WAS CREATED LATER, so its name sorts after the
	// other's, and only an order by use takes it first.
	oldest := generation{"acme/api/oldest", expiredGeneration(fourDays, 'a'), threeDays, threeDays}
	older := generation{"acme/api/older", expiredGeneration(now.Add(-120*time.Hour), 'b'), twoHours, twoHours}
	// RESTORED BY NAME ninety minutes ago, through a pointer last read four days
	// ago: the oldest pointer of all, and the most recent use of the three.
	restored := generation{"acme/api/restored", expiredGeneration(now.Add(-144*time.Hour), 'e'), fourDays,
		ninetyMinutes}
	recent := generation{"acme/api/recent", expiredGeneration(tenMinutes, 'c'), tenMinutes, tenMinutes}
	held := generation{"acme/api/held", expiredGeneration(threeDays, 'd'), threeDays, threeDays}

	for _, g := range []generation{older, recent, restored, held, oldest} {
		f.addVolume(g.handle)
		f.metadata[g.handle] = map[string]string{cacheMetaPrefix + "used_at": g.used.Format(time.RFC3339Nano)}
		record := cachePointer{Key: g.key, Generation: "gen-" + g.handle, Handle: g.handle, UsedAt: g.pointed,
			RetentionHours: 7 * 24}
		setIndexRecord(t, f, c, pointerKey(g.key), record)
		record.UsedAt = g.used
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

	if !report.Pressed() || report.Fill != 0.9 || report.Threshold != pressureFill || report.Evicted != 3 {
		t.Errorf("report = %+v, want the pool pressed at 0.9 and three generations evicted", report)
	}

	for _, g := range []generation{oldest, older, restored} {
		if f.images[g.handle] {
			t.Errorf("pressure kept %s, unused since %s", g.key, g.used.Format(time.RFC3339))
		}
		if _, ok := f.metadata[c.cacheIndex()][generationKey(g.key, "gen-"+g.handle)]; ok {
			t.Errorf("pressure left the generation record of %s", g.key)
		}
		// THE POINTER GOES WITH IT, so nothing hands out the removed generation and
		// a save of the key is not refused as already there.
		if _, ok := f.metadata[c.cacheIndex()][pointerKey(g.key)]; ok {
			t.Errorf("pressure left the pointer of %s, whose generation it removed", g.key)
		}
		if _, err := c.Current(t.Context(), g.key); !errors.Is(err, storecontract.ErrMiss) {
			t.Errorf("Current(%s) after pressure took its generation = %v, want a miss", g.key, err)
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

	order := []int{f.callIndex("rm", oldest.handle), f.callIndex("rm", older.handle),
		f.callIndex("rm", restored.handle)}
	if order[0] < 0 || order[1] < 0 || order[2] < 0 || order[0] > order[1] || order[1] > order[2] {
		t.Errorf("pressure removed the generations last used three days, two hours and ninety minutes ago "+
			"at calls %v: least recently used first, by the latest of any record's use", order)
	}
}

// A LINEAGE BEHIND A GENERATION PRESSURE TOOK FOLLOWS IT, BY PRESSURE ALONE. Each
// publication is cloned from the one before, through the retired writer between
// them, so the older generation cannot go while the newer one stands. Pressure
// takes the newer one and its pointer; the purge deletes the writer; and the
// older one, which no pointer names, is pressure's next candidate, with no
// superseded sweep run here to take it instead. All inside their retention.
func TestPressureUnwindsTheLineageBehindTheGenerationItTakes(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	f.poolFill = 0.9
	c := cacheClient(t, f)
	now := time.Now().UTC()
	twoHours := now.Add(-2 * time.Hour)
	key := "acme/api/go-chain"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lease, fence, err := c.acquireWriterAt(t.Context(), key, "lease-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	first, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot first: %v", err)
	}
	if err := c.publishCASAt(t.Context(), key, "", first, lease, fence, now); err != nil {
		t.Fatalf("PublishCAS first: %v", err)
	}

	volume, err = c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone first: %v", err)
	}
	later := now.Add(time.Second)
	lease, fence, err = c.acquireWriterAt(t.Context(), key, "lease-2", time.Minute, later)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}
	second, err := c.snapshotAt(t.Context(), volume, later)
	if err != nil {
		t.Fatalf("Snapshot second: %v", err)
	}
	if err := c.publishCASAt(t.Context(), key, first.Generation, second, lease, fence, later); err != nil {
		t.Fatalf("PublishCAS second: %v", err)
	}

	// EVERY USE TWO HOURS AGO: past pressureIdle and the superseded grace, inside
	// the seven-day retention.
	for _, candidate := range []string{first.Handle, second.Handle} {
		f.metadata[candidate][cacheMetaPrefix+"used_at"] = twoHours.Format(time.RFC3339Nano)
	}
	for metadataKey, value := range f.metadata[c.cacheIndex()] {
		if !strings.HasPrefix(metadataKey, cacheMetaPrefix+"pointer.") &&
			!strings.HasPrefix(metadataKey, cacheMetaPrefix+"generation.") {
			continue
		}

		var record cachePointer
		if err := json.Unmarshal([]byte(value), &record); err != nil {
			t.Fatalf("decode %s: %v", metadataKey, err)
		}
		record.UsedAt = twoHours
		setIndexRecord(t, f, c, metadataKey, record)
	}

	// ONE PRESSURE SWEEP: its rounds alternate the purge and eviction, so the
	// generation a child still read in its first round goes in a later one.
	report, err := c.EvictUnderPressure(t.Context())
	if err != nil {
		t.Fatalf("EvictUnderPressure: %v", err)
	}

	for _, candidate := range []string{first.Handle, second.Handle} {
		if f.images[candidate] {
			t.Errorf("one pressure sweep kept %s of a lineage it had taken the head of (%+v)", candidate, report)
		}
	}
	if len(f.trash) != 0 {
		t.Errorf("one pressure sweep left %d retired writers of that lineage: %v", len(f.trash), f.trash)
	}
	if _, err := c.Current(t.Context(), key); !errors.Is(err, storecontract.ErrMiss) {
		t.Errorf("Current after pressure took the lineage = %v, want a miss", err)
	}
}

// A GENERATION PRESSURE TRIED AND DID NOT REMOVE IS LEFT FOR AN HOUR, whether it
// failed or was refused without an error because a child still reads it.
// Pressure starts from the least recently used every sweep, so either would
// otherwise be asked again first, and enough of them would spend every sweep's
// budget before anything behind them. A deferral that has run out is forgotten,
// even for a generation no sweep asks about again.
func TestAGenerationPressureDidNotRemoveWaitsItsTurn(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		verb    string
		answer  error
		wantErr string
	}{
		"its removal fails": {
			verb:    "purge",
			answer:  errors.New("exit status 5: rbd: snapshot purge failed: (5) Input/output error"),
			wantErr: "Input/output",
		},
		"a child still reads it": {
			verb:   "rm",
			answer: cacheExitError{code: 39, message: "exit status 39: rbd: image has children: (39) Directory not empty"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			now := time.Now().UTC()
			f := newCacheFake()
			f.poolFill = 0.9

			threeDays := now.Add(-72 * time.Hour)
			stuck := expiredGeneration(threeDays, 'a')
			attempts := 0
			run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
				if slices.Contains(args, tc.verb) && slices.Contains(args, stuck) {
					attempts++

					return nil, tc.answer
				}

				return f.run(ctx, bin, args)
			}
			c := evictingClient(t, f, run)
			f.freshVolume(now)

			clock := now
			c.clock = func() time.Time { return clock }

			add := func(handle, key string, used time.Time) {
				f.addVolume(handle)
				f.metadata[handle] = map[string]string{cacheMetaPrefix + "used_at": used.Format(time.RFC3339Nano)}
				record := cachePointer{Key: key, Generation: "gen-" + handle, Handle: handle, UsedAt: used,
					RetentionHours: 7 * 24}
				setIndexRecord(t, f, c, pointerKey(key), record)
				setIndexRecord(t, f, c, generationKey(key, "gen-"+handle), record)
			}
			add(stuck, "acme/api/stuck", threeDays)

			_, err := c.EvictUnderPressure(t.Context())
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("EvictUnderPressure = %v, want the failure reported", err)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("EvictUnderPressure = %v, want a refusal without an error", err)
			}
			if attempts != 1 || !f.images[stuck] {
				t.Fatalf("the generation was tried %d times in one sweep and is present %v, want once and kept",
					attempts, f.images[stuck])
			}

			behind := expiredGeneration(now.Add(-48*time.Hour), 'b')
			add(behind, "acme/api/behind", now.Add(-48*time.Hour))

			clock = now.Add(pressureRetryAfter / 2)

			report, err := c.EvictUnderPressure(t.Context())
			if err != nil {
				t.Fatalf("EvictUnderPressure inside the deferral: %v", err)
			}
			if attempts != 1 || report.Evicted != 1 || f.images[behind] {
				t.Errorf("inside its deferral the generation was tried again (%d tries) or the one behind it "+
					"was not reached: %+v", attempts, report)
			}

			clock = now.Add(pressureRetryAfter + time.Minute)

			if _, err := c.EvictUnderPressure(t.Context()); (err != nil) != (tc.wantErr != "") {
				t.Errorf("past its deferral the sweep answered %v", err)
			}
			if attempts != 2 {
				t.Errorf("past its deferral the generation was tried %d times in all, want twice", attempts)
			}

			// GONE BY ANOTHER HAND, its deferral run out: forgotten at the next sweep.
			delete(f.images, stuck)
			clock = now.Add(3 * pressureRetryAfter)

			if _, err := c.EvictUnderPressure(t.Context()); err != nil {
				t.Fatalf("EvictUnderPressure after the generation went: %v", err)
			}

			c.evictMu.Lock()
			left := len(c.pressureDeferred)
			c.evictMu.Unlock()

			if left != 0 {
				t.Errorf("%d expired deferrals outlived the generations they were for", left)
			}
		})
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
