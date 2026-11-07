package tart

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// asASIF makes the stub's clones name an ASIF disk, the format whose start is gated.
func asASIF(t *testing.T, s *stub) {
	t.Helper()

	if err := os.WriteFile(s.cloneASIF, []byte("asif\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gateWatcher wraps the provider's question of whether a starting guest answers, asking
// it really, through the stub's `tart exec`, and records at each asking whether a listing
// could have taken the start gate then.
type gateWatcher struct {
	mu    sync.Mutex
	names []string
	free  []string
}

func watchGate(p *Provider) *gateWatcher {
	w := &gateWatcher{}

	p.guestUp = func(ctx context.Context, name string) (bool, error) {
		w.mu.Lock()
		w.names = append(w.names, name)

		if p.startGate.TryLock() {
			p.startGate.Unlock()

			w.free = append(w.free, name)
		}
		w.mu.Unlock()

		return p.guestAnswers(ctx, name)
	}

	return w
}

func (w *gateWatcher) snapshot() (names, free []string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]string(nil), w.names...), append([]string(nil), w.free...)
}

// AN ASIF LAUNCH WAITS FOR ITS GUEST WITH LISTINGS HELD OFF, from before `tart run`.
// `tart list` sizes an ASIF disk with `diskutil image info`, and one landing on the attach
// failed the start every time it was tried (a fleet Mac, macOS 27.2, 2026-11-07).
func TestAnASIFLaunchWaitsForItsGuestWithListingsHeldOff(t *testing.T) {
	s := newStub(t)
	asASIF(t, s)

	p := newProvider(t, s)
	w := watchGate(p)

	heldAtSpawn := false
	p.afterRunStart = func(string) {
		if p.startGate.TryLock() {
			p.startGate.Unlock()

			return
		}

		heldAtSpawn = true
	}

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !heldAtSpawn {
		t.Error("the start gate was free when `tart run` was spawned, so a listing could have run from its first instant")
	}

	names, free := w.snapshot()
	if len(names) == 0 {
		t.Fatal("an ASIF launch never waited for its guest, so nothing kept a listing out of its start")
	}

	if len(free) > 0 {
		t.Errorf("the launch waited for %v's guest with the start gate free", free)
	}

	// THE GUEST WAS ASKED THROUGH `tart exec`, which sizes no disk.
	if !strings.Contains(s.argv(t), "exec billet-lease1 /bin/sh -c :") {
		t.Errorf("the guest was not asked through `tart exec`:\n%s", s.argv(t))
	}
}

// A RAW DISK IS ONLY STAT'ED by a listing, so its start waits for nothing and takes no
// gate: the provider never asks its guest before the proof it always made.
func TestARawLaunchIsNotGated(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	w := watchGate(p)

	p.afterRunStart = func(string) { t.Error("a raw disk's start went through the gated path") }

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if names, _ := w.snapshot(); len(names) > 0 {
		t.Errorf("a raw launch waited for its guest under the gate: %v", names)
	}
}

// AND SO DOES A VERIFICATION'S PROBE OF AN ASIF IMAGE, which boots the same way.
func TestAnASIFProbeWaitsForItsGuestWithListingsHeldOff(t *testing.T) {
	s, p := verifiedStub(t)
	asASIF(t, s)

	w := watchGate(p)

	mustVerify(t, p)

	names, free := w.snapshot()

	probed := false

	for _, name := range names {
		if strings.HasPrefix(name, "billetprobe-") {
			probed = true
		}
	}

	if !probed {
		t.Fatalf("the probe never waited for its guest (asked about %v)", names)
	}

	if len(free) > 0 {
		t.Errorf("the probe waited for %v's guest with the start gate free", free)
	}
}

// A LISTING DOES NOT RUN WHILE A START IS BEFORE ITS GUEST: it waits, and past the window
// it refuses rather than reading the disk under the attach.
func TestAListingWaitsOutAStartAndRefusesPastItsWindow(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	p.startWindow = 100 * time.Millisecond

	p.startGate.RLock()

	_, err := p.list(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no answering guest") {
		t.Fatalf("list during a start = %v, want a refusal naming the start", err)
	}

	if _, err := p.listRows(t.Context()); err == nil {
		t.Fatal("listRows ran during a start")
	}

	if strings.Contains(s.argv(t), "list") {
		t.Fatalf("`tart list` ran while a start held the gate:\n%s", s.argv(t))
	}

	p.startGate.RUnlock()

	if _, err := p.list(t.Context()); err != nil {
		t.Fatalf("list once the start was past its guest's answer: %v", err)
	}
}

// A `tart run` THAT ENDS BEFORE ITS GUEST ANSWERS FAILS THE START AT ONCE, with tart's
// reason, rather than after the whole window.
func TestARunThatEndsFailsTheStartAtOnce(t *testing.T) {
	s := newStub(t)
	asASIF(t, s)

	p := newProvider(t, s)
	p.startWindow = time.Minute

	// THE STUB'S exec ANSWERS FOR ANY NAME, so the guest of a run that failed is made
	// not to: a real `tart exec` of a VM that is not running fails.
	p.guestUp = func(context.Context, string) (bool, error) { return false, nil }

	if err := os.WriteFile(s.runFails, []byte("Internal Virtualization error.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	took := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "never started") ||
		!strings.Contains(err.Error(), "Internal Virtualization error") {
		t.Fatalf("Launch = %v, want the VM never started, with tart's reason", err)
	}

	if took > 20*time.Second {
		t.Errorf("a run that ended took %s to fail the start; it must not wait out the window", took)
	}
}

// A CALLER THAT GIVES UP DOES NOT OPEN THE GATE: the run is detached and still attaching,
// so the gate stays the start's until its guest answers.
func TestACancelledStartKeepsListingsOutUntilItsGuestAnswers(t *testing.T) {
	s := newStub(t)
	asASIF(t, s)

	p := newProvider(t, s)
	p.startWindow = time.Minute

	answer := make(chan struct{})
	p.guestUp = func(ctx context.Context, _ string) (bool, error) {
		select {
		case <-answer:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	p.afterRunStart = func(string) { cancel() }

	if _, err := p.Launch(ctx, validSpec("billet-lease1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Launch cancelled during its start = %v, want the cancellation", err)
	}

	if p.startGate.TryLock() {
		p.startGate.Unlock()
		close(answer)
		t.Fatal("the cancelled launch released the start gate while its run was still before its guest's answer")
	}

	close(answer)

	deadline := time.Now().Add(20 * time.Second)
	for !p.startGate.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("the gate was never released after the guest answered")
		}

		time.Sleep(10 * time.Millisecond)
	}

	p.startGate.Unlock()
}

// AN ASKING THAT HANGS DOES NOT OUTLAST THE WINDOW: each is bounded by what is left of it.
func TestAGuestThatNeverAnswersEndsTheStartAtItsWindow(t *testing.T) {
	s := newStub(t)
	asASIF(t, s)

	p := newProvider(t, s)
	p.startWindow = 300 * time.Millisecond
	p.guestUp = func(ctx context.Context, _ string) (bool, error) {
		<-ctx.Done()

		return false, ctx.Err()
	}

	start := time.Now()
	_, err := p.Launch(t.Context(), validSpec("billet-lease1"))
	took := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "never started") {
		t.Fatalf("Launch = %v, want the VM never started", err)
	}

	if took > 8*time.Second {
		t.Errorf("a guest that never answered held the start for %s; the window is %s", took, p.startWindow)
	}

	if !p.startGate.TryLock() {
		t.Fatal("the gate is still held after the start's window ended")
	}

	p.startGate.Unlock()
}

// THE GUEST IS ASKED THROUGH `tart exec`, and an exec that fails is a guest not yet
// answering, with its reason kept.
func TestTheGuestIsAskedThroughExec(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	s.vm(t, "billet-lease1", "running", testOwner)

	if got, err := p.guestAnswers(t.Context(), "billet-lease1"); err != nil || !got {
		t.Fatalf("guestAnswers of an answering guest = %v, %v", got, err)
	}

	if err := os.WriteFile(s.execFails, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := p.guestAnswers(t.Context(), "billet-lease1")
	if got || err == nil || !strings.Contains(err.Error(), "Guest Agent") {
		t.Fatalf("guestAnswers of a guest whose agent is not up = %v, %v; want not yet, with the reason", got, err)
	}
}
