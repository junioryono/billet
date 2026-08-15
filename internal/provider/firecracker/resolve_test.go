package firecracker

import (
	"errors"
	"strings"
	"testing"
)

// A TIER MAY NAME A MOVING TARGET; A LAUNCH MUST NAME AN ARTIFACT.
//
// `@verified` means "the newest generation proved to boot", which is the whole point
// — it is how a fleet takes up a new image without anybody editing config on every
// node. But a launch that passed that word through would leave the one question that
// matters after a bad night, "which image did this job actually run", answerable only
// by guessing what was newest at the time.
//
// So it is resolved before anything uses it, and everything downstream — the clone,
// the log line, the instance — carries the concrete generation.
func TestALaunchRecordsTheGenerationRatherThanTheAlias(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	h.disk.mu.Lock()
	h.disk.resolved = "ubuntu-2404-x64@g20260814145813"
	h.disk.mu.Unlock()

	spec := aSpec()
	spec.Image = "ubuntu-2404-x64@verified"

	h.onJailer = func(id string) { h.serveVMM(t, id) }

	if _, err := h.p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// THE CLONE IS THE EVIDENCE. It is what the guest actually boots, so a root disk
	// taken from the resolved generation is the property; anything that recorded the
	// generation and then cloned something else would be worse than not resolving.
	got := h.disk.clonedFrom()

	if strings.Contains(got, "@"+"verified") {
		t.Errorf("the root disk was cloned from the alias (%q), so what this job booted "+
			"cannot be recovered afterwards", got)
	}

	if !strings.Contains(got, "g20260814145813") {
		t.Errorf("the root disk was cloned from %q rather than the resolved generation", got)
	}
}

// AND A TIER THAT PINS A GENERATION GETS EXACTLY THAT, unresolved.
//
// Pinning is a decision. A resolver that second-guessed it — by preferring something
// newer, or something verified — would make a pinned tier mean something other than
// what it says, which is the one property pinning is for.
func TestAPinnedGenerationIsNotSecondGuessed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.onJailer = func(id string) { h.serveVMM(t, id) }

	spec := aSpec()
	spec.Image = "ubuntu-2404-x64@g20260101000000"

	if _, err := h.p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := h.disk.clonedFrom(); !strings.Contains(got, "g20260101000000") {
		t.Errorf("a pinned generation was cloned from %q instead", got)
	}
}

// AND A LAUNCH THAT CANNOT BE RESOLVED FAILS BEFORE IT SPENDS ANYTHING.
//
// `@verified` with nothing verified is the state a fleet is in after every recent
// generation failed to boot. Booting "something" then would be the worst available
// answer: the point of the marker is that nothing unproven is ever started.
func TestAnUnresolvableImageFailsWithoutLeavingAJailBehind(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	h.disk.mu.Lock()
	h.disk.resolveErr = errors.New("no generation has passed verification")
	h.disk.mu.Unlock()

	spec := aSpec()
	spec.Image = "ubuntu-2404-x64@verified"

	if _, err := h.p.Launch(t.Context(), spec); err == nil {
		t.Fatal("a launch whose image could not be resolved reported success")
	}

	// NOTHING STARTED, which is the same property every other refusal in checkSpec
	// has: a launch that cannot proceed must not leave a jail, a uid or a device.
	if cmds := h.commands(); len(cmds) != 0 {
		t.Errorf("an unresolvable launch still ran %v", cmds)
	}
}
