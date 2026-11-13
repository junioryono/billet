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

				// NO LONGER THAN detachYield, while the attach still waits.
				time.Sleep(detachYield)
				synctest.Wait()
				select {
				case err := <-detached:
					if err != nil {
						t.Fatalf("the detach: %v", err)
					}
				default:
					t.Fatalf("no detach %s after it began waiting, with the attach still out", detachYield+5*time.Second)
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

// A DETACH NOT SEEN TO FINISH IS WAITED FOR BY THE NEXT ONE, which asks for its own only
// after: the store never has two in EC2's queue. A volume whose own detach was not seen to
// finish is waited for again, never asked to detach twice.
func TestADetachNotSeenToFinishIsWaitedForByTheNext(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s, _, attach, _ := localTestStore(&now)
	attachedHere(t, s, "vol-a")
	attachedHere(t, s, "vol-b")

	attach.awaitBlocks = true
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := s.detachLocal(ctx, "vol-a"); err == nil {
		t.Fatal("a detach whose end was not seen answered success")
	}
	attach.awaitBlocks = false
	if got := s.queue.settling(); got != "vol-a" {
		t.Fatalf("the unsettled detach is %q, want vol-a", got)
	}

	if err := s.detachLocal(t.Context(), "vol-b"); err != nil {
		t.Fatalf("the next detach: %v", err)
	}
	if !slices.Equal(attach.awaited, []string{"vol-a", "vol-b"}) || !slices.Equal(attach.detached, []string{"vol-a", "vol-b"}) {
		t.Fatalf("awaited %v and detached %v; want vol-a's end seen before vol-b was asked", attach.awaited, attach.detached)
	}
	if got := s.queue.settling(); got != "" {
		t.Fatalf("%q is still unsettled", got)
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
