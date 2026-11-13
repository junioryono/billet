package ebss3

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// attachedHere attaches volume to the test instance under its first free name, as a
// volume this store attached earlier.
func attachedHere(t *testing.T, s *Store, volume string) {
	t.Helper()
	if _, err := s.attachLocal(t.Context(), volume); err != nil {
		t.Fatalf("attach %s: %v", volume, err)
	}
}

// AN ATTACH ASKS ONLY FOR A NAME NONE OF THIS STORE'S ATTACHMENTS HOLDS, and a detached
// volume's name is free again: walking from /dev/sdf through every name in use cost one
// refused AttachVolume a live attachment, 205 in one run (2026-11-12).
func TestAnAttachSkipsTheNamesThisStoresAttachmentsHold(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s, _, attach, _ := localTestStore(&now)

	attachedHere(t, s, "vol-a")
	attachedHere(t, s, "vol-b")
	if !slices.Equal(attach.asked, []string{"/dev/sdf", "/dev/sdg"}) {
		t.Fatalf("names asked for: %v, want /dev/sdf then /dev/sdg, nothing refused", attach.asked)
	}

	if err := s.detachLocal(t.Context(), "vol-a"); err != nil {
		t.Fatalf("detach vol-a: %v", err)
	}
	delete(attach.used, "/dev/sdf") // EC2 frees the name with the attachment.
	attachedHere(t, s, "vol-c")
	if got := attach.asked[len(attach.asked)-1]; got != "/dev/sdf" || len(attach.asked) != 3 {
		t.Fatalf("names asked for: %v, want vol-a's /dev/sdf again once it was detached", attach.asked)
	}
}

// A NAME ANOTHER PROCESS'S VOLUME HOLDS IS STILL REFUSED BY EC2, and the walk goes past it
// without keeping it: a volume attached before this process started is not this store's
// to know of.
func TestANameThisStoreDoesNotKnowIsStillAskedAndPassed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s, _, attach, _ := localTestStore(&now)
	attach.used["/dev/sdf"] = true

	attachedHere(t, s, "vol-a")
	delete(attach.used, "/dev/sdf")
	attachedHere(t, s, "vol-b")
	if !slices.Equal(attach.asked, []string{"/dev/sdf", "/dev/sdg", "/dev/sdf"}) {
		t.Fatalf("names asked for: %v, want /dev/sdf refused, /dev/sdg taken, then /dev/sdf free", attach.asked)
	}
}

// DETACHES GO ONE AT A TIME, the next only once EC2 has carried the last out: EC2 does
// them in order per instance, and every one asked at once is one more an attach waits
// behind (#861).
func TestDetachesGoOneAtATime(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		s, _, attach, _ := localTestStore(&now)
		attachedHere(t, s, "vol-a")
		attachedHere(t, s, "vol-b")
		gate := make(chan struct{})
		attach.awaitGate = gate
		closed := false
		defer func() {
			if !closed {
				close(gate)
			}
		}()

		errs := make(chan error, 2)
		go func() { errs <- s.detachLocal(t.Context(), "vol-a") }()
		go func() { errs <- s.detachLocal(t.Context(), "vol-b") }()
		synctest.Wait()

		attach.mu.Lock()
		asked := len(attach.detached)
		attach.mu.Unlock()
		if asked != 1 {
			t.Fatalf("%d detaches asked of EC2 while the first was being carried out; want 1", asked)
		}

		close(gate)
		closed = true
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("detach: %v", err)
			}
		}
		if len(attach.detached) != 2 {
			t.Fatalf("detached %v, want both", attach.detached)
		}
	})
}

// A DETACH WAITS FOR THIS STORE'S ATTACH STILL WAITING FOR ITS DEVICE, and no longer than
// detachYield: an attach asked of EC2 after a detach waits for it, so a detach asked
// first costs the attach its whole length.
func TestADetachWaitsForAnAttachStillWaitingForItsDevice(t *testing.T) {
	t.Parallel()

	for name, appears := range map[string]bool{"the device appears": true, "it never does": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
				s, _, attach, disks := localTestStore(&now)
				attachedHere(t, s, "vol-old")
				attach.mu.Lock()
				disks.hidden["vol-new"] = true
				attach.mu.Unlock()

				attached := make(chan error, 1)
				go func() {
					_, err := s.attachLocal(t.Context(), "vol-new")
					attached <- err
				}()
				synctest.Wait()

				detached := make(chan error, 1)
				go func() { detached <- s.detachLocal(t.Context(), "vol-old") }()
				time.Sleep(5 * time.Second)
				synctest.Wait()
				attach.mu.Lock()
				early := len(attach.detached)
				attach.mu.Unlock()
				if early != 0 {
					t.Fatalf("a detach was asked of EC2 while an attach waited for its device")
				}

				if appears {
					attach.mu.Lock()
					delete(disks.hidden, "vol-new")
					attach.mu.Unlock()
					if err := <-attached; err != nil {
						t.Fatalf("the attach: %v", err)
					}
					if err := <-detached; err != nil {
						t.Fatalf("the detach: %v", err)
					}

					return
				}

				// NO LONGER THAN detachYield (and the poll that notices it), while the attach
				// still waits: 5 s have passed, so the rest of it.
				time.Sleep(detachYield - 5*time.Second + 2*detachYieldPoll)
				synctest.Wait()
				select {
				case err := <-detached:
					if err != nil {
						t.Fatalf("the detach: %v", err)
					}
				default:
					t.Fatalf("no detach %s after it began waiting, with the attach still out", detachYield)
				}
				select {
				case err := <-attached:
					t.Fatalf("the attach ended (%v) before the detach was let go; the yield was not what bounded it", err)
				default:
				}
				if err := <-attached; err == nil {
					t.Fatal("an attach whose device never appeared succeeded")
				}
			})
		})
	}
}

// A DETACH IS ASKED OF EC2 ONLY ONCE EC2 IS CARRYING OUT NO OTHER OF THIS STORE'S, as EC2
// itself answers: one whose wait gave up still counts, though nothing here holds a turn
// for it any more.
func TestADetachWaitsForEC2ToFinishTheStoresOtherDetaches(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		s, _, attach, _ := localTestStore(&now)
		attachedHere(t, s, "vol-a")
		attachedHere(t, s, "vol-b")
		attach.holdDetaching = true

		gaveUp, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := s.detachLocal(gaveUp, "vol-a"); err == nil {
			t.Fatal("a detach whose end was not seen answered success")
		}

		next := make(chan error, 1)
		go func() { next <- s.detachLocal(t.Context(), "vol-b") }()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		attach.mu.Lock()
		asked := slices.Clone(attach.detached)
		attach.mu.Unlock()
		if !slices.Equal(asked, []string{"vol-a"}) {
			t.Fatalf("detaches asked of EC2 while vol-a's was still being carried out: %v; want vol-a's alone", asked)
		}

		attach.finishDetaching("vol-a")
		time.Sleep(2 * detachSettlePoll)
		synctest.Wait()
		attach.finishDetaching("vol-b")
		if err := <-next; err != nil {
			t.Fatalf("vol-b's detach: %v", err)
		}
		if !slices.Equal(attach.detached, []string{"vol-a", "vol-b"}) {
			t.Fatalf("detaches asked of EC2: %v; want vol-a, then vol-b", attach.detached)
		}
	})
}

// A VOLUME WHOSE OWN DETACH IS STILL BEING CARRIED OUT IS WAITED FOR ON A RETRY, never
// asked to detach a second time.
func TestARetriedDetachWaitsForTheOneAlreadyAsked(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		s, _, attach, _ := localTestStore(&now)
		attachedHere(t, s, "vol-a")
		attach.holdDetaching = true

		gaveUp, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := s.detachLocal(gaveUp, "vol-a"); err == nil {
			t.Fatal("a detach whose end was not seen answered success")
		}

		retried := make(chan error, 1)
		go func() { retried <- s.detachLocal(t.Context(), "vol-a") }()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		attach.finishDetaching("vol-a")
		if err := <-retried; err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if !slices.Equal(attach.detached, []string{"vol-a"}) {
			t.Fatalf("detaches asked of EC2: %v; want vol-a's once", attach.detached)
		}
	})
}

// A RECONCILE FORGETS NOTHING IF THE NAMES CHANGED WHILE EC2'S LISTING WAS READ: an attach
// that took its name and finished meanwhile is not in a listing read before it, and its
// name is live.
func TestAReconcileKeepsNamesThatChangedDuringTheListing(t *testing.T) {
	t.Parallel()

	q := newAttachQueue()
	if !q.reserve("/dev/sdf", "vol-stale") {
		t.Fatal("could not stage /dev/sdf")
	}
	stale, at := q.staleNames()
	if !q.reserve("/dev/sdg", "vol-new") {
		t.Fatal("could not reserve /dev/sdg")
	}
	q.reconcile(stale, at, map[string]time.Time{})
	if q.reserve("/dev/sdg", "vol-other") || q.reserve("/dev/sdf", "vol-other") {
		t.Fatal("a reconcile forgot names though they changed while the listing was read")
	}

	stale, at = q.staleNames()
	q.reconcile(stale, at, map[string]time.Time{"vol-new": {}})
	if !q.reserve("/dev/sdf", "vol-other") || q.reserve("/dev/sdg", "vol-other") {
		t.Fatal("a reconcile with nothing changed did not free the stale name and keep the attached one")
	}
}

// THE NAMES THIS STORE REMEMBERS ARE CHECKED AGAINST EC2 ONCE ALL ARE TAKEN: a name kept
// by a volume whose detach was never seen to finish is free in fact, and an attach takes it
// rather than refusing every name.
func TestRememberedNamesAreCheckedAgainstEC2WhenAllAreTaken(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s, _, attach, _ := localTestStore(&now)
	for _, name := range attachmentPoints {
		if !s.queue.reserve(name, "vol-long-gone") {
			t.Fatalf("could not stage %s as held", name)
		}
	}

	attachedHere(t, s, "vol-new")
	if !slices.Equal(attach.asked, []string{"/dev/sdf"}) {
		t.Fatalf("names asked for: %v, want /dev/sdf once the stale names were checked against EC2", attach.asked)
	}
}

// A DETACH WHOSE CALLER GIVES UP WHILE IT WAITS ITS TURN ASKS NOTHING OF EC2, and leaves
// the turn free.
func TestADetachThatGaveUpWaitingItsTurnLeavesItFree(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		q := newAttachQueue()
		done := q.attachStarted("vol-new")
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := q.detachTurn(ctx); err == nil {
			t.Fatal("a detach whose caller gave up was given its turn")
		}
		done()
		end, err := q.detachTurn(t.Context())
		if err != nil {
			t.Fatalf("the turn after: %v", err)
		}
		end()
	})
}
