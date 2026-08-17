package firecracker

import (
	"net/http"
	"strings"
	"testing"

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
	for _, want := range []string{spec.CacheEndpoint, spec.CacheToken, "4294967296"} {
		if !strings.Contains(metadataJSON, want) {
			t.Errorf("metadata does not carry %q", want)
		}
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
