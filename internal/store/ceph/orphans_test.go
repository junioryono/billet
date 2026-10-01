package ceph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// orphanVolume names a writable volume the way cacheName does, at a given time.
func orphanVolume(named time.Time, nonce byte) string {
	return fmt.Sprintf("billet-cache/cache-v-%d-%s", named.Unix(), strings.Repeat(string(nonce), 24))
}

func verdicts(report OrphanReport) map[string]OrphanVerdict {
	out := map[string]OrphanVerdict{}
	for _, image := range report.Images {
		out["billet-cache/"+image.Name] = image.Verdict
	}

	return out
}

func trashMoved(f *cacheFake, handle string) bool {
	return f.ranWith("trash", "mv", handle)
}

// AN INTACT ORPHAN IS RECLAIMED ONLY ON PROOF, AND ONLY THROUGH THE TRASH. Every
// image that fails one proof is kept, each for its own reason, and the one that
// passes them all is moved with `rbd trash mv` and then deleted by the purge;
// nothing is ever removed with `rbd rm`.
func TestOrphanVolumesAreReclaimedOnlyOnProofAndThroughTheTrash(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-DefaultOrphanAge - time.Hour)

	orphan := orphanVolume(old, 'a')
	young := orphanVolume(now.Add(-DefaultOrphanAge+time.Hour), 'b')
	watched := orphanVolume(old, 'c')
	mapped := orphanVolume(old, 'd')
	inSession := orphanVolume(old, 'e')
	indexed := orphanVolume(old, 'f')
	snapshotted := orphanVolume(old, '1')
	statusFails := orphanVolume(old, '2')
	snapsFail := orphanVolume(old, '3')
	noWatchers := orphanVolume(old, '4')
	halfRemoved := orphanVolume(old, '5')
	lineage := orphanVolume(old, '8')
	used := orphanVolume(old, '9')
	unreadableUse := orphanVolume(old, '0')
	publishing := orphanVolume(old.Add(2*time.Second), 'b')
	moveUnconfirmed := orphanVolume(old.Add(time.Second), 'a')
	vanishing := orphanVolume(old.Add(3*time.Second), 'a')
	generation := fmt.Sprintf("billet-cache/cache-g-%d-%s", old.Unix(), strings.Repeat("6", 24))
	unparseable := fmt.Sprintf("billet-cache/cache-v-%d-by-hand", old.Unix())
	padded := fmt.Sprintf("billet-cache/cache-v-0%d-%s", old.Unix(), strings.Repeat("7", 24))

	for _, image := range []string{
		orphan, young, watched, mapped, inSession, indexed, snapshotted, statusFails, snapsFail,
		noWatchers, generation, unparseable, padded, lineage, used, unreadableUse, publishing,
		moveUnconfirmed, vanishing,
	} {
		f.images[image] = true
	}
	f.halfRemoved[halfRemoved] = true
	f.watchers[watched] = 1
	f.watchers[publishing] = 1
	f.mappings["/dev/rbd9"] = mapped
	f.snapshots[snapshotted+"@staging"] = true
	f.trashSnapshots[lineage+"@retired"] = true
	f.metadata[used] = map[string]string{
		"billet.cache.used_at": now.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	f.metadata[unreadableUse] = map[string]string{"billet.cache.used_at": "yesterday"}
	f.metadata["billet-cache/.cache-index"] = map[string]string{
		"billet.cache.active.x": `{"key":"k","handle":"` + indexed + `"}`,
	}

	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		switch {
		// ANOTHER NODE PUBLISHES IT between the snapshot list and the watchers:
		// Snapshot writes used_at, then unmaps, so the watcher is gone by the time
		// rbd answers.
		case slices.Contains(args, "status") && slices.Contains(args, publishing):
			f.metadata[publishing] = map[string]string{
				"billet.cache.used_at": now.Format(time.RFC3339Nano),
			}
			delete(f.watchers, publishing)
		case slices.Contains(args, "trash") && slices.Contains(args, "mv") &&
			slices.Contains(args, moveUnconfirmed):
			if _, err := f.run(ctx, bin, args); err != nil {
				t.Fatalf("the fake refused the move: %v", err)
			}

			return nil, context.DeadlineExceeded
		// REMOVED BY SOMEONE ELSE after its watchers were read: its metadata
		// cannot be listed, which is not the same as having no use time.
		case slices.Contains(args, "image-meta") && slices.Contains(args, vanishing):
			delete(f.images, vanishing)

			return nil, errors.New("exit status 2: rbd: error opening image: (2) No such file or directory")
		case slices.Contains(args, "status") && slices.Contains(args, statusFails):
			return nil, errors.New("exit status 110: rbd: error: (110) Connection timed out")
		case slices.Contains(args, "snap") && slices.Contains(args, "ls") && slices.Contains(args, snapsFail):
			return nil, errors.New("exit status 13: rbd: error: (13) Permission denied")
		case slices.Contains(args, "status") && slices.Contains(args, noWatchers):
			return []byte(`{"migration":{}}`), nil
		}

		return f.run(ctx, bin, args)
	}

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.clock = func() time.Time { return now }

	_, sessionName, _ := strings.Cut(inSession, "/")

	report, err := c.ReclaimOrphans(t.Context(), OrphanOptions{
		OlderThan: DefaultOrphanAge, Limit: 100, Reclaim: true,
		InSession: func(name string) bool { return name == sessionName },
	})
	if err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}

	got := verdicts(report)
	for image, want := range map[string]OrphanVerdict{
		orphan:          OrphanMoved,
		young:           OrphanTooYoung,
		watched:         OrphanWatched,
		mapped:          OrphanWatched,
		inSession:       OrphanInSession,
		indexed:         OrphanInIndex,
		snapshotted:     OrphanSnapshotted,
		statusFails:     OrphanUnknown,
		snapsFail:       OrphanUnknown,
		noWatchers:      OrphanUnknown,
		halfRemoved:     OrphanHalfRemoved,
		generation:      OrphanGeneration,
		unparseable:     OrphanForeign,
		padded:          OrphanForeign,
		lineage:         OrphanSnapshotted,
		used:            OrphanUsed,
		unreadableUse:   OrphanUnknown,
		publishing:      OrphanUsed,
		moveUnconfirmed: OrphanMoveUnknown,
		vanishing:       OrphanGone,
	} {
		if got[image] != want {
			t.Errorf("%s: verdict %q, want %q", image, got[image], want)
		}
		if image == orphan || image == moveUnconfirmed {
			continue
		}
		if trashMoved(f, image) {
			t.Errorf("%s (%s) was moved to the trash", image, want)
		}
	}

	for _, image := range report.Images {
		if image.Verdict == OrphanUnknown && image.Err == nil {
			t.Errorf("%s could not be judged and carries no reason", image.Name)
		}
	}

	if f.images[orphan] || !trashMoved(f, orphan) {
		t.Fatal("the proved orphan was not moved to the trash")
	}

	for _, call := range f.calls {
		if slices.Contains(call, "rm") || slices.Contains(call, "purge") {
			t.Fatalf("an orphan pass ran %v; it may only move a volume to the trash", call)
		}
	}

	// THE PURGE FINISHES IT, under the bound it gives every trash entry.
	n, err := c.PurgeTrash(t.Context())
	if n < 1 || len(f.trash) != 0 {
		t.Errorf("PurgeTrash deleted %d, left %v; want the moved orphan deleted (%v)", n, f.trash, err)
	}
}

// A LISTING CHANGES NOTHING. Without Reclaim the pass asks the same questions
// and reports the volume reclaimable, and never moves it.
func TestAnOrphanListingChangesNothing(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	now := time.Now()
	orphan := orphanVolume(now.Add(-8*24*time.Hour), 'a')
	f.images[orphan] = true

	c := cacheClient(t, f)
	c.clock = func() time.Time { return now }

	report, err := c.ReclaimOrphans(t.Context(), OrphanOptions{
		OlderThan: DefaultOrphanAge, Limit: 10, InSession: func(string) bool { return false },
	})
	if err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}

	if got := verdicts(report)[orphan]; got != OrphanReclaimable {
		t.Errorf("verdict %q, want %q", got, OrphanReclaimable)
	}

	if !f.images[orphan] || trashMoved(f, orphan) || !f.ranWith("status", orphan) {
		t.Error("a listing moved the volume, or never asked rbd about it")
	}
}

// ONE PASS ASKS ABOUT AT MOST ITS LIMIT, OLDEST FIRST, and reports the rest as
// deferred rather than leaving them out.
func TestAnOrphanPassIsBoundedAndWorksOldestFirst(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	now := time.Now()
	oldest := orphanVolume(now.Add(-30*24*time.Hour), 'c')
	older := orphanVolume(now.Add(-20*24*time.Hour), 'b')
	newest := orphanVolume(now.Add(-10*24*time.Hour), 'a')

	for _, image := range []string{oldest, older, newest} {
		f.images[image] = true
	}

	c := cacheClient(t, f)
	c.clock = func() time.Time { return now }

	report, err := c.ReclaimOrphans(t.Context(), OrphanOptions{
		OlderThan: DefaultOrphanAge, Limit: 2, Reclaim: true, InSession: func(string) bool { return false },
	})
	if err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}

	got := verdicts(report)
	if got[oldest] != OrphanMoved || got[older] != OrphanMoved || got[newest] != OrphanDeferred {
		t.Errorf("verdicts %v; want the two oldest moved and the newest deferred", got)
	}

	if f.ranWith("status", newest) || !f.images[newest] {
		t.Error("a pass asked about, or moved, an image past its limit")
	}

	if report.Count(OrphanMoved) != 2 || report.Count(OrphanDeferred) != 1 {
		t.Errorf("counted %d moved and %d deferred, want 2 and 1",
			report.Count(OrphanMoved), report.Count(OrphanDeferred))
	}
}

// WHAT A PASS KEEPS DOES NOT USE UP ITS LIMIT. Older volumes kept on every pass,
// such as half-removed ones the purge is still finishing, would otherwise hold
// the orphans behind them back forever.
func TestKeptVolumesDoNotUseUpAnOrphanPassLimit(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	now := time.Now()
	halfRemoved := orphanVolume(now.Add(-30*24*time.Hour), 'c')
	snapshotted := orphanVolume(now.Add(-20*24*time.Hour), 'b')
	orphan := orphanVolume(now.Add(-10*24*time.Hour), 'a')

	f.halfRemoved[halfRemoved] = true
	f.images[snapshotted] = true
	f.snapshots[snapshotted+"@staging"] = true
	f.images[orphan] = true

	c := cacheClient(t, f)
	c.clock = func() time.Time { return now }

	report, err := c.ReclaimOrphans(t.Context(), OrphanOptions{
		OlderThan: DefaultOrphanAge, Limit: 1, Reclaim: true, InSession: func(string) bool { return false },
	})
	if err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}

	got := verdicts(report)
	if got[halfRemoved] != OrphanHalfRemoved || got[snapshotted] != OrphanSnapshotted ||
		got[orphan] != OrphanMoved {
		t.Errorf("verdicts %v; want the two kept and the orphan behind them moved", got)
	}
}

// THE ZERO VALUE REFUSES, before rbd is asked anything: no age, an age under a
// day, no limit, and no word on this node's sessions are each refused.
func TestAnOrphanPassRefusesOptionsItCannotStandOn(t *testing.T) {
	t.Parallel()

	sessions := func(string) bool { return false }
	for name, opts := range map[string]OrphanOptions{
		"zero value":       {},
		"under a day":      {OlderThan: OrphanMinimumAge - time.Minute, Limit: 1, InSession: sessions},
		"no limit":         {OlderThan: DefaultOrphanAge, InSession: sessions},
		"no session check": {OlderThan: DefaultOrphanAge, Limit: 1},
	} {
		f := newCacheFake()
		c := cacheClient(t, f)

		if _, err := c.ReclaimOrphans(t.Context(), opts); err == nil {
			t.Errorf("%s: accepted", name)
		}

		if len(f.calls) != 0 {
			t.Errorf("%s: ran %v before refusing", name, f.calls)
		}
	}
}

// AN INDEX THAT CANNOT BE READ JUDGES NOTHING, because every image would then be
// unproved against it.
func TestAnOrphanPassThatCannotReadTheIndexMovesNothing(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	now := time.Now()
	orphan := orphanVolume(now.Add(-8*24*time.Hour), 'a')
	f.images[orphan] = true
	f.metaListErr = errors.New("exit status 108: rbd: error: (108) Cannot send after transport endpoint shutdown")

	c := cacheClient(t, f)
	c.clock = func() time.Time { return now }

	if _, err := c.ReclaimOrphans(t.Context(), OrphanOptions{
		OlderThan: DefaultOrphanAge, Limit: 10, Reclaim: true, InSession: func(string) bool { return false },
	}); err == nil {
		t.Error("a pass went ahead without the cache index")
	}

	if !f.images[orphan] || trashMoved(f, orphan) {
		t.Error("a volume was moved with the cache index unread")
	}
}

// AN INDEX ANSWER THAT IS NOT THE INDEX JUDGES NOTHING. rbd prints the metadata
// as one json object, or nothing for an index without any; anything else is not
// a list this pass can stand on.
func TestAnOrphanPassThatCannotParseTheIndexMovesNothing(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	now := time.Now()
	orphan := orphanVolume(now.Add(-8*24*time.Hour), 'a')
	f.images[orphan] = true

	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "image-meta") && slices.Contains(args, "list") {
			return []byte("There are 2 metadata on this image:\n"), nil
		}

		return f.run(ctx, bin, args)
	}

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.clock = func() time.Time { return now }

	if _, err := c.ReclaimOrphans(t.Context(), OrphanOptions{
		OlderThan: DefaultOrphanAge, Limit: 10, Reclaim: true, InSession: func(string) bool { return false },
	}); err == nil {
		t.Error("a pass went ahead on an index answer it could not read")
	}

	if !f.images[orphan] || trashMoved(f, orphan) {
		t.Error("a volume was moved with the cache index unparsed")
	}
}

// A MAPPING TABLE THAT NAMES THE VOLUME PROTECTS IT, however incomplete the
// entry, and one this pass cannot read is could-not-tell rather than "not
// mapped".
func TestAnOrphanPassTakesNoProofFromAMappingItCannotRead(t *testing.T) {
	t.Parallel()

	now := time.Now()
	orphan := orphanVolume(now.Add(-8*24*time.Hour), 'a')
	_, name, _ := strings.Cut(orphan, "/")

	for answer, want := range map[string]OrphanVerdict{
		`[{"pool":"billet-cache","name":"` + name + `"}]`:       OrphanWatched,
		`[{"pool":"other","name":"` + name + `","device":"x"}]`: OrphanWatched,
		`[{}]`:   OrphanUnknown,
		`[null]`: OrphanUnknown,
		`null`:   OrphanUnknown,
	} {
		f := newCacheFake()
		f.images[orphan] = true

		run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
			if slices.Contains(args, "device") && slices.Contains(args, "list") {
				return []byte(answer), nil
			}

			return f.run(ctx, bin, args)
		}

		c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(run))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		c.clock = func() time.Time { return now }

		report, err := c.ReclaimOrphans(t.Context(), OrphanOptions{
			OlderThan: DefaultOrphanAge, Limit: 10, Reclaim: true, InSession: func(string) bool { return false },
		})
		if err != nil {
			t.Fatalf("%s: ReclaimOrphans: %v", answer, err)
		}

		if got := verdicts(report)[orphan]; got != want {
			t.Errorf("device list %s: verdict %q, want %q", answer, got, want)
		}

		if trashMoved(f, orphan) {
			t.Errorf("device list %s: the volume was moved", answer)
		}
	}
}

// THE DEFAULT AGE OUTLASTS THE LONGEST JOB GITHUB RUNS ON A SELF-HOSTED RUNNER,
// five days by its documentation, and the floor is a day.
func TestTheOrphanAgeOutlastsTheLongestJob(t *testing.T) {
	t.Parallel()

	if DefaultOrphanAge <= 5*24*time.Hour || OrphanMinimumAge < 24*time.Hour {
		t.Errorf("default %s, floor %s; want past five days and at least a day",
			DefaultOrphanAge, OrphanMinimumAge)
	}
}
