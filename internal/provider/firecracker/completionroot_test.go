package firecracker

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// completionDisk is a fake disk that also resolves completion roots.
type completionDisk struct {
	*fakeDisk

	asked []string
}

func (d *completionDisk) ResolveCompletionRoot(_ context.Context, ref, contract string) (string, error) {
	d.asked = append(d.asked, ref+" "+contract)

	return ref, nil
}

const aCompletionRoot = "ubuntu-2404-x64@c20260815041709"

// A LAUNCH MARKED CompletionRoot CLONES THE ROOT IT NAMES THROUGH THE COMPLETION
// RESOLVER, never through ResolveGeneration, which refuses a completion root for
// every other launch.
func TestACompletionLaunchClonesItsRootThroughTheCompletionResolver(t *testing.T) {
	h := newHarness(t)
	disk := &completionDisk{fakeDisk: h.disk}
	h.p.disk = disk

	h.mu.Lock()
	h.onJailer = func(id string) { h.serveVMM(t, id) }
	h.mu.Unlock()

	spec := aSpec()
	spec.Image = aCompletionRoot
	spec.CompletionRoot = true

	if _, err := h.p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if want := []string{aCompletionRoot + " " + GuestContract}; !slices.Equal(disk.asked, want) {
		t.Fatalf("the completion resolver was asked %v, want %v", disk.asked, want)
	}
	if h.disk.resolvedContract != "" {
		t.Fatal("a completion launch resolved through ResolveGeneration")
	}
	if len(h.disk.cloned) != 1 || !strings.HasPrefix(h.disk.cloned[0], aCompletionRoot+" -> ") {
		t.Fatalf("cloned %v, want the completion root", h.disk.cloned)
	}
}

// A LAUNCH MARKED CompletionRoot ON A DISK WITH NO COMPLETION ROOTS IS REFUSED, and
// clones nothing, rather than falling back to ResolveGeneration.
func TestACompletionLaunchOnADiskWithoutCompletionRootsIsRefused(t *testing.T) {
	h := newHarness(t)

	spec := aSpec()
	spec.Image = aCompletionRoot
	spec.CompletionRoot = true

	_, err := h.p.Launch(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), "asks for a completion root and this node's disk has none") {
		t.Fatalf("got %v, want the launch refused", err)
	}
	if h.disk.resolvedContract != "" || len(h.disk.cloned) != 0 {
		t.Fatalf("the refused launch resolved (%q) or cloned %v", h.disk.resolvedContract, h.disk.cloned)
	}
}

// AN UNMARKED LAUNCH GOES THROUGH ResolveGeneration, whose refusal of a completion
// root is the store's (TestOnlyTheCompletionResolverReachesACompletionRoot).
func TestAnOrdinaryLaunchResolvesThroughResolveGeneration(t *testing.T) {
	h := newHarness(t)
	disk := &completionDisk{fakeDisk: h.disk}
	h.p.disk = disk

	h.launch(t)

	if h.disk.resolvedContract != GuestContract || len(disk.asked) != 0 {
		t.Fatalf("an ordinary launch resolved through %q and the completion resolver %v",
			h.disk.resolvedContract, disk.asked)
	}
}
