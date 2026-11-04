package firecracker

import (
	"testing"
)

// ONLY A TIER THAT MAY BE ATTACHED TO HAS THE DEVICE AND THE WORD, so every other guest has nothing
// listening and one thing fewer in its VMM.
func TestOnlyAnAttachableGuestHasAVsock(t *testing.T) {
	t.Parallel()

	for _, asked := range []bool{false, true} {
		h := newHarness(t)

		var vmm *fakeVMM

		h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }

		spec := aSpec()
		spec.GuestAttach = asked
		if asked {
			spec.GuestHold = "5m0s"
		}

		if _, err := h.p.Launch(t.Context(), spec); err != nil {
			t.Fatalf("Launch: %v", err)
		}

		body, ok := vmm.bodyPut("/vsock")
		if ok != asked {
			t.Errorf("asked %t: the vsock was configured: %t", asked, ok)
		}

		// JSON's numbers come back as floats.
		if asked && (body["uds_path"] != "/run/vsock.sock" || body["guest_cid"] != float64(guestVsockCID)) {
			t.Errorf("the vsock was configured as %v", body)
		}

		md, ok := vmm.bodyPut("/mmds")
		if !ok {
			t.Fatal("metadata was not configured")
		}

		leaves := metadataLeaves(t, "", md)

		if _, present := leaves["/latest/meta-data/billet/guest-attach"]; present != asked {
			t.Errorf("asked %t: the metadata says guest-attach: %t", asked, present)
		}

		if got, present := leaves["/latest/meta-data/billet/guest-attach-hold"]; present != asked || (asked && got != "5m0s") {
			t.Errorf("asked %t: the metadata says guest-attach-hold: %q, %t", asked, got, present)
		}
	}
}
