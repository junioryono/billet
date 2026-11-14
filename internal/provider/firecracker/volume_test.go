package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

func TestACacheEnabledGuestBootsWithFiveReplaceableDriveSlots(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	var vmm *fakeVMM
	h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }

	spec := aSpec()
	spec.CacheEndpoint = "http://172.20.0.1:7718"
	spec.CacheToken = strings.Repeat("a", 64)
	spec.BuildKitCacheMountLimit = 4 << 30
	spec.ActionsProxy = "http://billet:session@172.20.0.1:7719"
	spec.ActionsCAPEM = "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----"

	if _, err := h.p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	for slot := range provider.MaxVolumes {
		path := "/drives/" + provider.VolumeSlotID(slot)
		body, ok := vmm.bodyPut(path)
		if !ok {
			t.Errorf("cache slot %d was not configured", slot)

			continue
		}

		if body["path_on_host"] != "/"+provider.VolumeSlotID(slot) {
			t.Errorf("cache slot %d uses host path %v", slot, body["path_on_host"])
		}
	}

	metadataBody, ok := vmm.bodyPut("/mmds")
	if !ok {
		t.Fatal("metadata was not configured")
	}

	metadataJSON := renderJSON(t, metadataBody)
	for _, want := range []string{
		spec.CacheEndpoint, spec.CacheToken, "4294967296", spec.ActionsProxy, "BEGIN CERTIFICATE",
	} {
		if !strings.Contains(metadataJSON, want) {
			t.Errorf("metadata does not carry %q", want)
		}
	}
	if strings.Contains(metadataJSON, "cache-ready-token") {
		t.Fatal("metadata carries a readiness pseudo-secret inside a root-controlled guest")
	}
}

// THE GUEST IS TOLD TO START ITS MONITOR BY ONE OPTIONAL STRING LEAF, present only
// when the node asks for it. Optional, so the guest contract does not move: an agent
// that predates the key reads none and starts nothing.
func TestTheGuestIsToldToStartItsMonitorOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	const key = "/latest/meta-data/billet/guest-monitor"

	for _, asked := range []bool{true, false} {
		h := newHarness(t)
		var vmm *fakeVMM
		h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }

		spec := aSpec()
		spec.CacheEndpoint = "http://172.20.0.1:7718"
		spec.CacheToken = strings.Repeat("c", 64)
		spec.BuildKitCacheMountLimit = 4 << 30
		spec.GuestMonitor = asked

		if _, err := h.p.Launch(t.Context(), spec); err != nil {
			t.Fatalf("Launch: %v", err)
		}

		body, ok := vmm.bodyPut("/mmds")
		if !ok {
			t.Fatal("metadata was not configured")
		}

		value, present := metadataLeaves(t, "", body)[key]
		switch {
		case asked && (!present || value != "1"):
			t.Errorf("a guest asked to monitor has %s = %q (present %t), want \"1\"", key, value, present)
		case !asked && present:
			t.Errorf("a guest not asked to monitor carries %s = %q", key, value)
		}
	}

	if GuestContract != "10" {
		t.Errorf("the guest contract is %q; an optional key is not a contract change", GuestContract)
	}
}

func TestAReservedDriveCanBeReplacedAndDetachedAfterBoot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	var vmm *fakeVMM
	h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }

	spec := aSpec()
	spec.CacheEndpoint = "http://172.20.0.1:7718"
	spec.CacheToken = strings.Repeat("b", 64)
	spec.BuildKitCacheMountLimit = 4 << 30

	inst, err := h.p.Launch(t.Context(), spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := h.p.AttachVolume(t.Context(), inst.ID, 2, "/dev/rbd9"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	if err := h.p.DetachVolume(t.Context(), inst.ID, 2, "/dev/rbd9"); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}

	vmm.mu.Lock()
	defer vmm.mu.Unlock()

	var patches []recordedPut
	for _, call := range vmm.puts {
		if call.method == http.MethodPatch && call.path == "/drives/cache2" {
			patches = append(patches, call)
		}
	}

	if len(patches) != 2 {
		t.Fatalf("cache2 received %d runtime patches, want attach and detach", len(patches))
	}

	if patches[0].body["path_on_host"] != "/cache2" ||
		patches[1].body["path_on_host"] != "/cache2" {
		t.Errorf("runtime patches named unexpected paths: %+v", patches)
	}
}

// cache0Patches counts the runtime PATCHes a VMM received for drive cache0.
func cache0Patches(vmm *fakeVMM) int {
	vmm.mu.Lock()
	defer vmm.mu.Unlock()

	n := 0
	for _, call := range vmm.puts {
		if call.method == http.MethodPatch && call.path == "/drives/cache0" {
			n++
		}
	}

	return n
}

// syncedDisk is an open disk whose write-back runs sync, or nothing, counting its
// write-backs and closes when given counters.
type syncedDisk struct {
	sync   func() error
	synced *atomic.Int32
	closes *atomic.Int32
}

func (d syncedDisk) Sync() error {
	if d.synced != nil {
		d.synced.Add(1)
	}
	if d.sync != nil {
		return d.sync()
	}

	return nil
}

func (d syncedDisk) Close() error {
	if d.closes != nil {
		d.closes.Add(1)
	}

	return nil
}

// forgotten waits, bounded, until the provider holds no write-back in flight. Its
// worker marks one done and forgets it under one lock, so an empty map is a joined one.
func forgotten(t *testing.T, p *Provider) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		p.flushMu.Lock()
		left := len(p.flushing)
		p.flushMu.Unlock()
		if left == 0 {
			return
		}
	}
	t.Error("a write-back was still in flight ten seconds after it was released")
}

// A DETACH WRITES THE DEVICE BACK BEFORE IT REPLACES THE DRIVE, because the VMM's
// close of the device writes back every dirty page before it answers (#871). A flush
// that fails leaves the write-back to the close and still detaches; a caller whose
// time runs out while the flush runs gets its error and no detach.
func TestADetachWritesTheDeviceBackBeforeItReplacesTheDrive(t *testing.T) {
	t.Parallel()

	launched := func(t *testing.T) (*harness, *fakeVMM, string) {
		t.Helper()
		h := newHarness(t)
		var vmm *fakeVMM
		h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }
		spec := aSpec()
		spec.CacheEndpoint = "http://172.20.0.1:7718"
		spec.CacheToken = strings.Repeat("b", 64)
		spec.BuildKitCacheMountLimit = 4 << 30
		inst, err := h.p.Launch(t.Context(), spec)
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if err := h.p.AttachVolume(t.Context(), inst.ID, 0, "/dev/nvme9n1"); err != nil {
			t.Fatalf("AttachVolume: %v", err)
		}

		return h, vmm, inst.ID
	}

	t.Run("flushed first", func(t *testing.T) {
		t.Parallel()
		h, vmm, id := launched(t)
		var opened []string
		var closes atomic.Int32
		patchesAtFlush, closedAtPatch := -1, int32(-1)
		h.p.openDisk = func(device string) (diskHandle, error) {
			opened = append(opened, device)

			return syncedDisk{closes: &closes, sync: func() error {
				patchesAtFlush = cache0Patches(vmm)

				return nil
			}}, nil
		}
		vmm.mu.Lock()
		vmm.onPatch = func() { closedAtPatch = closes.Load() }
		vmm.mu.Unlock()
		if err := h.p.DetachVolume(t.Context(), id, 0, "/dev/nvme9n1"); err != nil {
			t.Fatalf("DetachVolume: %v", err)
		}
		if len(opened) != 1 || opened[0] != "/dev/nvme9n1" {
			t.Fatalf("opened %q, want the volume's device once", opened)
		}
		if patchesAtFlush != 1 || cache0Patches(vmm) != 2 {
			t.Errorf("cache0 had %d PATCH(es) when it was written back and %d after; want the attach's, then the detach's",
				patchesAtFlush, cache0Patches(vmm))
		}
		if closedAtPatch != 1 || closes.Load() != 1 {
			t.Errorf("the device was closed %d time(s) by the detach's PATCH and %d after; want once, before it",
				closedAtPatch, closes.Load())
		}
	})

	// AN OPEN THAT FAILS LEAVES THE WRITE-BACK TO THE VMM'S CLOSE; A WRITE-BACK THAT
	// FAILS LOST WRITES. Both still detach, and only the second says so.
	for name, failure := range map[string]struct{ open, sync error }{
		"an open that fails":      {open: errors.New("no such device")},
		"a write-back that fails": {sync: errors.New("input/output error")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, vmm, id := launched(t)
			var closes atomic.Int32
			h.p.openDisk = func(string) (diskHandle, error) {
				if failure.open != nil {
					return nil, failure.open
				}

				return syncedDisk{closes: &closes, sync: func() error { return failure.sync }}, nil
			}
			err := h.p.DetachVolume(t.Context(), id, 0, "/dev/nvme9n1")
			switch {
			case failure.sync != nil && !errors.Is(err, provider.ErrWritesLost):
				t.Fatalf("a detach whose write-back failed answered %v, want provider.ErrWritesLost", err)
			case failure.sync != nil && !strings.Contains(err.Error(), "input/output error"):
				t.Errorf("the lost writes' answer %q does not carry the fsync's error", err)
			case failure.sync == nil && err != nil:
				t.Fatalf("a detach whose device could not be opened: %v", err)
			}
			if cache0Patches(vmm) != 2 {
				t.Errorf("cache0 had %d PATCH(es); a failed write-back must still detach", cache0Patches(vmm))
			}
			wantCloses := int32(1)
			if failure.open != nil {
				wantCloses = 0
			}
			if closes.Load() != wantCloses {
				t.Errorf("the device was closed %d time(s); want %d", closes.Load(), wantCloses)
			}
		})
	}

	t.Run("a slot that no longer holds the device", func(t *testing.T) {
		t.Parallel()
		h, vmm, id := launched(t)
		h.p.slotHolds = func(string, string) bool { return false }
		h.p.openDisk = func(device string) (diskHandle, error) {
			t.Errorf("opened %s, which the slot does not hold", device)

			return syncedDisk{}, nil
		}
		if err := h.p.DetachVolume(t.Context(), id, 0, "/dev/nvme9n1"); err != nil {
			t.Fatalf("DetachVolume: %v", err)
		}
		if cache0Patches(vmm) != 2 {
			t.Errorf("cache0 had %d PATCH(es); the detach must still be made", cache0Patches(vmm))
		}
	})

	t.Run("the caller's time runs out", func(t *testing.T) {
		t.Parallel()
		h, vmm, id := launched(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var closes atomic.Int32
		h.p.openDisk = func(string) (diskHandle, error) {
			return syncedDisk{closes: &closes, sync: func() error {
				close(entered)
				<-release

				return nil
			}}, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		var helper sync.WaitGroup
		helper.Go(func() {
			select {
			case <-entered:
				cancel()
			case <-ctx.Done():
			}
		})
		// RELEASED AND JOINED HOWEVER THE TEST ENDS.
		t.Cleanup(func() {
			cancel()
			helper.Wait()
			close(release)
			forgotten(t, h.p)
			if closes.Load() != 1 {
				t.Errorf("the write-back left running closed its device %d time(s); want once", closes.Load())
			}
		})
		err := h.p.DetachVolume(ctx, id, 0, "/dev/nvme9n1")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a detach whose write-back outlasted its caller answered %v", err)
		}
		if cache0Patches(vmm) != 1 {
			t.Errorf("cache0 had %d PATCH(es); a detach that gave up must not replace the drive",
				cache0Patches(vmm))
		}
	})
}

// A WRITE-BACK IN FLIGHT IS WAITED ON, NEVER STARTED AGAIN BESIDE IT: a caller that gave
// up leaves its fsync running, and the next caller for that device joins it, so retries
// against a slow device add no descriptors and no blocked threads. Once it ends, the
// next caller opens the device again and starts its own.
func TestAWriteBackInFlightIsWaitedOnNotRepeated(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p := &Provider{log: slog.New(slog.DiscardHandler)}
		release := make(chan struct{})
		// RELEASED HOWEVER THE TEST ENDS, or a failed assertion would leave the bubble
		// waiting on a write-back nothing ends.
		var once sync.Once
		releaseAll := func() { once.Do(func() { close(release) }) }
		defer releaseAll()
		opens := 0
		p.openDisk = func(string) (diskHandle, error) {
			opens++

			return syncedDisk{sync: func() error {
				<-release

				return nil
			}}, nil
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := p.flushBefore(ctx, "/dev/nvme9n1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a caller whose time ran out answered %v", err)
		}

		retried := make(chan error, 1)
		go func() { retried <- p.flushBefore(t.Context(), "/dev/nvme9n1") }()
		synctest.Wait()
		select {
		case err := <-retried:
			t.Fatalf("the retry returned while the first write-back still ran: %v", err)
		default:
		}
		if opens != 1 {
			t.Fatalf("the device was opened %d times while one write-back was in flight", opens)
		}

		releaseAll()
		if err := <-retried; err != nil {
			t.Fatalf("the retry: %v", err)
		}
		synctest.Wait()
		if err := p.flushBefore(t.Context(), "/dev/nvme9n1"); err != nil {
			t.Fatalf("a caller after it ended: %v", err)
		}
		if opens != 2 {
			t.Errorf("the device was opened %d times; want once for the first, waited on, then once after", opens)
		}
	})
}

// A WRITE-BACK'S LOST WRITES REACH EVERY CALLER WAITING ON IT. The kernel tells the
// one fsync, so a caller that joined a write-back already in flight, after the first
// caller's time ran out, is told what that fsync found, never success.
func TestAWriteBackThatFailedTellsEveryCallerWaitingOnIt(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p := &Provider{log: slog.New(slog.DiscardHandler)}
		release := make(chan struct{})
		var once sync.Once
		releaseAll := func() { once.Do(func() { close(release) }) }
		defer releaseAll()
		p.openDisk = func(string) (diskHandle, error) {
			return syncedDisk{sync: func() error {
				<-release

				return errors.New("input/output error")
			}}, nil
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := p.flushBefore(ctx, "/dev/nvme9n1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a caller whose time ran out answered %v", err)
		}

		joined := make(chan error, 1)
		go func() { joined <- p.flushBefore(t.Context(), "/dev/nvme9n1") }()
		synctest.Wait()
		releaseAll()

		if err := <-joined; !errors.Is(err, provider.ErrWritesLost) {
			t.Fatalf("a caller that joined a write-back that failed answered %v, want provider.ErrWritesLost", err)
		}
	})
}

// LOST WRITES NOBODY WAITED TO HEAR ARE TOLD TO THE NEXT CALLER, ONCE. Every caller gave
// up while the fsync ran, and the fsync then failed; the kernel tells that fsync and no
// other, so the next caller is told before anything is opened again, and the one after
// opens and writes back as usual. A different disk that took the name is told nothing.
func TestLostWritesNobodyWaitedForAreToldToTheNextCaller(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		seqThen, seqNow string
		told            bool
	}{
		"the same disk":                 {seqThen: "41", seqNow: "41", told: true},
		"a disk that took its name":     {seqThen: "41", seqNow: "57"},
		"a disk whose number is unread": {seqThen: "41", told: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				p := &Provider{log: slog.New(slog.DiscardHandler)}
				seq := tc.seqThen
				p.diskSeq = func(string) (string, bool) { return seq, seq != "" }
				release := make(chan struct{})
				var once sync.Once
				releaseAll := func() { once.Do(func() { close(release) }) }
				defer releaseAll()
				opens := 0
				p.openDisk = func(string) (diskHandle, error) {
					opens++
					first := opens == 1

					return syncedDisk{sync: func() error {
						if !first {
							return nil
						}
						<-release

						return errors.New("input/output error")
					}}, nil
				}

				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				synctest.Wait()
				if err := p.flushBefore(ctx, "/dev/rbd4"); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("a caller whose time ran out answered %v", err)
				}
				releaseAll()
				synctest.Wait()

				seq = tc.seqNow
				err := p.flushBefore(t.Context(), "/dev/rbd4")
				if told := errors.Is(err, provider.ErrWritesLost); told != tc.told {
					t.Fatalf("the next caller was told %v, want lost writes told %v", err, tc.told)
				}
				wantOpens := map[bool]int{true: 1, false: 2}[tc.told]
				if opens != wantOpens {
					t.Errorf("the device was opened %d time(s), want %d", opens, wantOpens)
				}
				if err := p.flushBefore(t.Context(), "/dev/rbd4"); err != nil {
					t.Errorf("the caller after that was told %v, want the write-back it made", err)
				}
			})
		})
	}
}

// A WRITE-BACK OF THE DISK THAT HAD THE NAME BEFORE IS NO ANSWER FOR THE ONE THAT HAS
// IT NOW: a caller that joins one in flight after the name passed to another disk is
// told neither its loss nor its success, and the loss stays the old disk's, never
// told to the new one.
func TestAWriteBackOfAnotherDiskIsNoAnswer(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p := &Provider{log: slog.New(slog.DiscardHandler)}
		seq := "41"
		p.diskSeq = func(string) (string, bool) { return seq, true }
		release := make(chan struct{})
		var once sync.Once
		releaseAll := func() { once.Do(func() { close(release) }) }
		defer releaseAll()
		opens := 0
		p.openDisk = func(string) (diskHandle, error) {
			opens++
			first := opens == 1

			return syncedDisk{sync: func() error {
				if !first {
					return nil
				}
				<-release

				return errors.New("input/output error")
			}}, nil
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := p.flushBefore(ctx, "/dev/rbd4"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a caller whose time ran out answered %v", err)
		}

		seq = "57"
		joined := make(chan error, 1)
		go func() { joined <- p.flushBefore(t.Context(), "/dev/rbd4") }()
		synctest.Wait()
		releaseAll()
		if err := <-joined; err == nil || errors.Is(err, provider.ErrWritesLost) {
			t.Fatalf("a caller for the disk that took the name was told %v, want neither success nor the "+
				"other disk's loss", err)
		}
		synctest.Wait()
		// THE LOSS IS THE OLD DISK'S, named while its descriptor was held, so the new
		// disk's next caller writes back its own.
		if err := p.flushBefore(t.Context(), "/dev/rbd4"); err != nil || opens != 2 {
			t.Errorf("the new disk's next caller was told %v after %d open(s), want its own write-back, "+
				"a second open", err, opens)
		}
	})
}

// A DETACH THAT FAILED IS NOT ONE THAT LOST WRITES, AND KEEPS THEM FOR THE NEXT.
// ErrWritesLost promises the volume is detached, so a PATCH that failed answers only
// its failure, and the next detach, whose PATCH goes through, is told what was lost.
func TestADetachThatFailedKeepsItsLostWritesForTheNext(t *testing.T) {
	t.Parallel()

	// THE NEXT DETACH IS HANDED THE DEVICE, OR NONE, as one whose proof of the device
	// failed is; either is told.
	for name, retryDevice := range map[string]string{"handed the device": "/dev/nvme9n1", "handed none": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			detachThatFailedThenRetried(t, retryDevice)
		})
	}
}

func detachThatFailedThenRetried(t *testing.T, retryDevice string) {
	t.Helper()

	h := newHarness(t)
	var vmm *fakeVMM
	h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }
	spec := aSpec()
	spec.CacheEndpoint = "http://172.20.0.1:7718"
	spec.CacheToken = strings.Repeat("b", 64)
	spec.BuildKitCacheMountLimit = 4 << 30
	inst, err := h.p.Launch(t.Context(), spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := h.p.AttachVolume(t.Context(), inst.ID, 0, "/dev/nvme9n1"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	failed := false
	h.p.openDisk = func(string) (diskHandle, error) {
		if failed {
			return syncedDisk{}, nil
		}
		failed = true

		return syncedDisk{sync: func() error { return errors.New("input/output error") }}, nil
	}
	h.p.diskSeq = func(string) (string, bool) { return "7", true }
	// THE PLACEHOLDER GOES IN BEFORE THE PATCH, so once the first detach has run the
	// slot no longer holds the device, as a real one does not.
	placed := false
	h.p.slotHolds = func(string, string) bool { return !placed }
	vmm.mu.Lock()
	vmm.failDrivePatches = 1
	vmm.mu.Unlock()

	err = h.p.DetachVolume(t.Context(), inst.ID, 0, "/dev/nvme9n1")
	placed = true
	if err == nil || errors.Is(err, provider.ErrWritesLost) {
		t.Fatalf("a detach whose PATCH failed answered %v, want its failure and not lost writes", err)
	}
	if err := h.p.DetachVolume(t.Context(), inst.ID, 0, retryDevice); !errors.Is(err, provider.ErrWritesLost) {
		t.Fatalf("the next detach answered %v, want the lost writes the failed one heard", err)
	}
	if err := h.p.DetachVolume(t.Context(), inst.ID, 0, retryDevice); errors.Is(err, provider.ErrWritesLost) {
		t.Errorf("a third detach was told the lost writes again: %v", err)
	}
}

// A SLOW OPEN HOLDS NO OTHER DEVICE'S DETACH, AND ONE THAT ENDS AFTER EVERY CALLER GAVE UP
// WRITES NOTHING BACK: a block device's open can wait in the kernel, and once its callers
// have gone the volume may be unmapped and its number another's, so the descriptor is
// closed unused.
func TestASlowOpenHoldsNoOtherDetachAndWritesNothingBackLate(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		p := &Provider{log: slog.New(slog.DiscardHandler)}
		slow := make(chan struct{})
		var once sync.Once
		releaseSlow := func() { once.Do(func() { close(slow) }) }
		defer releaseSlow()
		var slowSynced, slowClosed, otherSynced atomic.Int32
		p.openDisk = func(device string) (diskHandle, error) {
			if device == "/dev/slow" {
				<-slow

				return syncedDisk{synced: &slowSynced, closes: &slowClosed}, nil
			}

			return syncedDisk{synced: &otherSynced}, nil
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := p.flushBefore(ctx, "/dev/slow"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a caller whose device's open outlasted it answered %v", err)
		}
		if err := p.flushBefore(t.Context(), "/dev/other"); err != nil || otherSynced.Load() != 1 {
			t.Fatalf("another device, while one open hung: %v, written back %d time(s)", err, otherSynced.Load())
		}

		releaseSlow()
		synctest.Wait()
		if slowSynced.Load() != 0 || slowClosed.Load() != 1 {
			t.Errorf("an open that ended after its caller gave up was written back %d time(s) and closed %d; "+
				"want closed once, unused", slowSynced.Load(), slowClosed.Load())
		}
	})
}

// deadlineRecorder records the deadline of every PATCH it carries.
type deadlineRecorder struct {
	inner http.RoundTripper
	mu    sync.Mutex
	seen  []time.Duration
}

func (d *deadlineRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if deadline, ok := r.Context().Deadline(); ok && r.Method == http.MethodPatch {
		d.mu.Lock()
		d.seen = append(d.seen, time.Until(deadline))
		d.mu.Unlock()
	}

	return d.inner.RoundTrip(r)
}

// A DETACH'S PATCH HAS WHAT IS LEFT OF ITS CALLER'S TIME, never less than apiTimeout: the
// close it causes writes the device back, and a caller with five minutes gives it five.
func TestADetachsPatchHasTheCallersTime(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.onJailer = func(id string) { h.serveVMM(t, id) }
	spec := aSpec()
	spec.CacheEndpoint = "http://172.20.0.1:7718"
	spec.CacheToken = strings.Repeat("b", 64)
	spec.BuildKitCacheMountLimit = 4 << 30
	inst, err := h.p.Launch(t.Context(), spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := h.p.AttachVolume(t.Context(), inst.ID, 0, "/dev/nvme9n1"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	recorder := &deadlineRecorder{}
	h.p.apiAs = func(socket string, uid int) *vmmAPI {
		api := answeringAsAnyone(socket, uid)
		recorder.inner = api.http.Transport
		api.http.Transport = recorder

		return api
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	if err := h.p.DetachVolume(ctx, inst.ID, 0, "/dev/nvme9n1"); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.seen) != 1 || recorder.seen[0] < 4*time.Minute {
		t.Errorf("the detach's PATCH had %v to answer; want what was left of the caller's five minutes",
			recorder.seen)
	}

	if got := callerTime(t.Context()); got != apiTimeout {
		t.Errorf("no deadline: %s, want apiTimeout", got)
	}
	long, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	if got := callerTime(long); got < 4*time.Minute || got > 5*time.Minute {
		t.Errorf("five minutes left: %s", got)
	}
	short, cancel2 := context.WithTimeout(t.Context(), time.Second)
	defer cancel2()
	if got := callerTime(short); got != apiTimeout {
		t.Errorf("a second left: %s, want apiTimeout", got)
	}
}

func TestNoGuestCanRequestMoreThanFiveVolumes(t *testing.T) {
	t.Parallel()

	spec := aSpec()
	spec.Volumes = make([]provider.VolumeMount, provider.MaxVolumes+1)
	for i := range spec.Volumes {
		spec.Volumes[i] = provider.VolumeMount{Device: "/dev/rbd1"}
	}

	if err := checkSpec(spec); err == nil {
		t.Fatal("checkSpec accepted more cache devices than Firecracker can reserve")
	}
}
