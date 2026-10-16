package firecracker

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// refusingPeers is a VMM client whose every connection is answered, as far as the
// kernel says, by somebody other than the microVM's VMM. It records the uid each
// client was built for.
type refusingPeers struct {
	mu   sync.Mutex
	uids []int
}

func (r *refusingPeers) client(socket string, uid int) *vmmAPI {
	r.mu.Lock()
	r.uids = append(r.uids, uid)
	r.mu.Unlock()

	return newVMMAPIDialing(socket, func(net.Conn) error { return errVMMPeer })
}

func (r *refusingPeers) builtFor() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]int(nil), r.uids...)
}

// patchedDrives counts the runtime drive patches a fake VMM received.
func patchedDrives(vmm *fakeVMM) int {
	vmm.mu.Lock()
	defer vmm.mu.Unlock()

	n := 0

	for _, call := range vmm.puts {
		if call.method == http.MethodPatch && strings.HasPrefix(call.path, "/drives/") {
			n++
		}
	}

	return n
}

// A RUNNING GUEST'S SOCKET ANSWERED BY ANOTHER VMM IS NEITHER RUNNING NOR STOPPED.
// The guest has run, so its socket could be a link to another lease's; List reports
// the doubt as an error rather than describing that other microVM as this one, and
// asks for the microVM's own uid.
func TestListRefusesAMicroVMWhoseSocketAnotherVMMAnswers(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	peers := &refusingPeers{}
	h.p.apiAs = peers.client

	if _, err := h.p.List(t.Context()); !errors.Is(err, errVMMPeer) {
		t.Fatalf("List = %v, want errVMMPeer", err)
	}

	if uids := peers.builtFor(); len(uids) == 0 || uids[0] != 900000 {
		t.Errorf("the checked client was built for uids %v, want the microVM's 900000", uids)
	}
}

// A CACHE DRIVE IS PATCHED INTO THIS MICROVM'S VMM OR NOT AT ALL, attaching and
// detaching alike.
func TestACacheDriveIsNeverPatchedIntoAnotherVMM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	var vmm *fakeVMM

	h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }

	spec := aSpec()
	spec.CacheEndpoint = "http://172.20.0.1:7718"
	spec.CacheToken = strings.Repeat("c", 64)
	spec.BuildKitCacheMountLimit = 4 << 30

	inst, err := h.p.Launch(t.Context(), spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	before := patchedDrives(vmm)

	peers := &refusingPeers{}
	h.p.apiAs = peers.client

	if err := h.p.AttachVolume(t.Context(), inst.ID, 2, "/dev/rbd9"); !errors.Is(err, errVMMPeer) {
		t.Errorf("AttachVolume = %v, want errVMMPeer", err)
	}

	if err := h.p.DetachVolume(t.Context(), inst.ID, 2, "/dev/rbd9"); !errors.Is(err, errVMMPeer) {
		t.Errorf("DetachVolume = %v, want errVMMPeer", err)
	}

	if after := patchedDrives(vmm); after != before {
		t.Errorf("%d drive patches reached a socket another VMM answers", after-before)
	}

	for _, uid := range peers.builtFor() {
		if uid != 900000 {
			t.Errorf("a cache client was built for uid %d, not the microVM's 900000", uid)
		}
	}
}

// A NEW VMM ANSWERED BY SOMEBODY ELSE FAILS THE LAUNCH AT ONCE, not after the boot
// wait: polling again cannot change whose process listens.
func TestALaunchWhoseSocketAnotherVMMAnswersFailsAtOnce(t *testing.T) {
	t.Parallel()

	peers := &refusingPeers{}
	h := newHarness(t, WithBootWait(time.Minute))
	h.p.apiAs = peers.client
	h.onJailer = func(id string) { h.serveVMM(t, id) }

	started := time.Now()

	if _, err := h.p.Launch(t.Context(), aSpec()); !errors.Is(err, errVMMPeer) {
		t.Fatalf("Launch = %v, want errVMMPeer", err)
	}

	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("the launch took %s to give up on a socket another VMM answers", took)
	}
}
