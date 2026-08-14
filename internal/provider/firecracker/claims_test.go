package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// ONE NAME, ONE WINNER, EVEN WHEN EVERY LAUNCH ASKS AT ONCE.
//
// A uid per microVM is the isolation boundary between guests on one host: two VMMs
// sharing an account can read and signal each other through the very chroot the
// jailer exists to separate. So "two launches both believed they had it" is not a
// tidiness bug, it is the property being silently absent.
//
// AND CONCURRENT LAUNCHES CONTEND SYSTEMATICALLY RATHER THAN OCCASIONALLY, because
// allocation always starts at the bottom of the range — every launch on a quiet host
// asks for the same name first. That is what made this worth a test with real
// goroutines rather than an argument.
func TestConcurrentLaunchesNeverShareAUIDOrADevice(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	const racers = 24

	// Wider than the racers, so a collision is the only thing that can make two of
	// them agree on a name — not exhaustion.
	h.p.cfg.JailUIDCount = racers * 4

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		held  []resources
		fails []error
	)

	for i := range racers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			j := h.p.jailFor(provider.InstanceName(fmt.Sprintf("%032x", i+1)))

			if err := h.p.claim(j); err != nil {
				mu.Lock()
				fails = append(fails, err)
				mu.Unlock()

				return
			}

			res, err := h.p.claimResources(j)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				fails = append(fails, err)

				return
			}

			held = append(held, res)
		}()
	}

	wg.Wait()

	if err := errors.Join(fails...); err != nil {
		t.Fatalf("a launch could not claim what it needed: %v", err)
	}

	uids := map[int]bool{}
	taps := map[string]bool{}

	for _, res := range held {
		if uids[res.UID] {
			t.Errorf("uid %d was handed to two microVMs at once, so both run as one account "+
				"and each can reach the other's jail", res.UID)
		}

		if taps[res.Tap] {
			t.Errorf("device name %s was handed to two microVMs at once", res.Tap)
		}

		uids[res.UID], taps[res.Tap] = true, true
	}

	if len(held) != racers {
		t.Errorf("%d launches claimed and %d resources came back", racers, len(held))
	}
}

// AND A CLAIM IS NEVER VISIBLE BEFORE IT SAYS WHOSE IT IS.
//
// This is the invariant the concurrency above rests on, asserted directly because a
// race test can only ever fail to find a window. The claim is staged with its content
// and linked into place, so the instant the name exists it already names its jail —
// which is what makes the reaper safe. The predecessor created the name first and
// wrote the content second, and the reaper (correctly) frees a claim with nothing in
// it, so a launch could reap a claim another launch was in the middle of making.
func TestAClaimAlwaysNamesItsJailTheMomentItExists(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	j := h.p.jailFor(theInstance)
	if err := h.p.claim(j); err != nil {
		t.Fatalf("claim: %v", err)
	}

	res, err := h.p.claimResources(j)
	if err != nil {
		t.Fatalf("claimResources: %v", err)
	}

	for _, claim := range []string{
		filepath.Join(h.p.claimsDir("uids"), strconv.Itoa(res.UID)),
		filepath.Join(h.p.claimsDir("taps"), res.Tap),
	} {
		raw, err := os.ReadFile(claim)
		if err != nil {
			t.Fatalf("read the claim %s: %v", claim, err)
		}

		if got := strings.TrimSpace(string(raw)); got != j.id {
			t.Errorf("the claim %s names %q and the jail is %q", claim, got, j.id)
		}
	}
}

// A TEARDOWN ONLY RELEASES WHAT IS STILL ITS OWN.
//
// Teardown removes the jail BEFORE releasing claims, deliberately, so that no claim
// is freed while a VMM might still be using it. That ordering has a consequence: in
// the gap, another launch may legitimately reap this claim — its jail is gone, which
// is exactly the reaper's condition — and take the name. A release that deleted by
// NAME then removes a live claim, and the launch after that takes a uid a running
// microVM is using.
//
// So this stages the aftermath directly: the claim exists and names somebody else.
func TestReleasingAClaimSomebodyElseNowHoldsLeavesItAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	first := h.p.jailFor(provider.InstanceName(fmt.Sprintf("%032x", 1)))
	if err := h.p.claim(first); err != nil {
		t.Fatalf("claim: %v", err)
	}

	res, err := h.p.claimResources(first)
	if err != nil {
		t.Fatalf("claimResources: %v", err)
	}

	// The successor: it reaped the claim and took the same name, which is the
	// legitimate outcome once the first jail is gone.
	const successor = "billet-ffffffffffffffffffffffffffffffff"

	uidClaim := filepath.Join(h.p.claimsDir("uids"), strconv.Itoa(res.UID))
	tapClaim := filepath.Join(h.p.claimsDir("taps"), res.Tap)

	for _, claim := range []string{uidClaim, tapClaim} {
		if err := os.WriteFile(claim, []byte(successor+"\n"), 0o600); err != nil {
			t.Fatalf("stage the successor's claim: %v", err)
		}
	}

	if err := h.p.releaseResources(res, first.id); err != nil {
		t.Fatalf("releaseResources: %v", err)
	}

	for _, claim := range []string{uidClaim, tapClaim} {
		raw, err := os.ReadFile(claim)
		if err != nil {
			t.Fatalf("the release deleted a claim that belonged to another microVM (%s): %v",
				claim, err)
		}

		if got := strings.TrimSpace(string(raw)); got != successor {
			t.Errorf("the claim %s now says %q and its owner wrote %q", claim, got, successor)
		}
	}
}

// A DEVICE NAME IS NOT GIVEN BACK WHILE THE DEVICE IS STILL THERE.
//
// This is how a host wedges itself permanently. If teardown cannot delete the tap but
// releases its NAME anyway, the device stays attached to the bridge and nothing
// enumerates orphan devices — so the next launch draws the same lowest-free name,
// `ip tuntap add` refuses it because the kernel already has it, that launch fails and
// hands the name straight back, and every launch after it fails identically. The host
// stops being able to start a microVM at all until an operator deletes the device by
// hand.
//
// Keeping the claim is what lets a later teardown find the device and finish.
func TestADeviceNameIsHeldUntilTheDeviceIsActuallyGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	inst, _ := h.launch(t)

	j := h.p.jailFor(inst.Name)

	res, err := resourcesOf(j)
	if err != nil {
		t.Fatalf("resourcesOf: %v", err)
	}

	// The kernel refusing to remove the device is the case; everything else about
	// teardown proceeds, which is what makes the name available to be given away.
	h.mu.Lock()
	h.refuse = func(_ string, args []string) error {
		if len(args) > 1 && args[0] == "link" && args[1] == "del" {
			return errors.New("RTNETLINK answers: Device or resource busy")
		}

		return nil
	}
	h.mu.Unlock()

	if err := h.p.Destroy(t.Context(), inst.Name); err == nil {
		t.Fatal("Destroy reported success though it could not remove the network device")
	}

	claim := filepath.Join(h.p.claimsDir("taps"), res.Tap)

	if _, err := os.Stat(claim); err != nil {
		t.Errorf("the device %s is still attached to the bridge and its name was returned to "+
			"the pool: every launch that draws it will now fail until an operator removes it "+
			"by hand (%v)", res.Tap, err)
	}

	// AND THE UID IS STILL RELEASED, because nothing is wrong with it. Holding it
	// back would shrink the range on every failure of an unrelated resource.
	if _, err := os.Stat(filepath.Join(h.p.claimsDir("uids"), strconv.Itoa(res.UID))); err == nil {
		t.Error("the uid was held back even though only the network device failed to go")
	}
}
