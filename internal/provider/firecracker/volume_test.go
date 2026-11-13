package firecracker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
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
		var flushed []string
		var patchesAtFlush int
		h.p.flushDisk = func(device string) error {
			flushed = append(flushed, device)
			patchesAtFlush = cache0Patches(vmm)

			return nil
		}
		if err := h.p.DetachVolume(t.Context(), id, 0, "/dev/nvme9n1"); err != nil {
			t.Fatalf("DetachVolume: %v", err)
		}
		if len(flushed) != 1 || flushed[0] != "/dev/nvme9n1" {
			t.Fatalf("flushed %q, want the volume's device once", flushed)
		}
		if patchesAtFlush != 1 || cache0Patches(vmm) != 2 {
			t.Errorf("cache0 had %d PATCH(es) when it was flushed and %d after; want the attach's, then the detach's",
				patchesAtFlush, cache0Patches(vmm))
		}
	})

	t.Run("a flush that fails", func(t *testing.T) {
		t.Parallel()
		h, vmm, id := launched(t)
		h.p.flushDisk = func(string) error { return errors.New("no such device") }
		if err := h.p.DetachVolume(t.Context(), id, 0, "/dev/nvme9n1"); err != nil {
			t.Fatalf("a detach whose flush failed: %v", err)
		}
		if cache0Patches(vmm) != 2 {
			t.Errorf("cache0 had %d PATCH(es); a failed flush must still detach", cache0Patches(vmm))
		}
	})

	t.Run("the caller's time runs out, and the retry waits on the same flush", func(t *testing.T) {
		t.Parallel()
		h, vmm, id := launched(t)
		release, finished := make(chan struct{}), make(chan struct{})
		var mu sync.Mutex
		flushes := 0
		h.p.flushDisk = func(string) error {
			mu.Lock()
			flushes++
			mu.Unlock()
			<-release
			close(finished)

			return nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := h.p.DetachVolume(ctx, id, 0, "/dev/nvme9n1")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a detach whose flush outlasted its caller answered %v", err)
		}
		if cache0Patches(vmm) != 1 {
			t.Errorf("cache0 had %d PATCH(es); a detach that gave up must not replace the drive",
				cache0Patches(vmm))
		}

		retried := make(chan error, 1)
		go func() { retried <- h.p.DetachVolume(t.Context(), id, 0, "/dev/nvme9n1") }()
		time.Sleep(50 * time.Millisecond)
		select {
		case err := <-retried:
			t.Fatalf("the retry finished while the first flush still ran: %v", err)
		default:
		}
		close(release)
		<-finished
		if err := <-retried; err != nil {
			t.Fatalf("the retry: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if flushes != 1 || cache0Patches(vmm) != 2 {
			t.Errorf("%d flush(es) and %d PATCH(es); want the one flush, waited on, then the detach",
				flushes, cache0Patches(vmm))
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
