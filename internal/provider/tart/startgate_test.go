package tart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// gateWatcher wraps a provider's disk-attached answer and records, at each call, whether
// a listing could have taken the start gate then: a call made while it was free is a
// start whose window a `tart list` could have run in.
type gateWatcher struct {
	mu    sync.Mutex
	names []string
	free  []string
}

func watchGate(p *Provider) *gateWatcher {
	w := &gateWatcher{}
	inner := p.diskAttached

	p.diskAttached = func(ctx context.Context, name string) (bool, error) {
		w.mu.Lock()
		w.names = append(w.names, name)

		if p.startGate.TryLock() {
			p.startGate.Unlock()

			w.free = append(w.free, name)
		}
		w.mu.Unlock()

		return inner(ctx, name)
	}

	return w
}

func (w *gateWatcher) asked(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, n := range w.names {
		if n == name {
			return true
		}
	}

	return false
}

// A LAUNCH WAITS FOR ITS VM'S DISK WITH LISTINGS HELD OFF. `tart list` sizes an ASIF disk
// with `diskutil image info`, and one landing on the attach failed the start every time
// it was tried (a fleet Mac, macOS 27.2, 2026-11-07), so the wait for the disk must run
// under the gate a listing needs.
func TestALaunchWaitsForItsDiskWithListingsHeldOff(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	w := watchGate(p)

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !w.asked("billet-lease1") {
		t.Fatal("Launch never waited for the VM's disk to be attached, so nothing kept a listing out of its start")
	}

	if len(w.free) > 0 {
		t.Errorf("Launch waited for %v's disk with the start gate free, so a `tart list` could have run "+
			"while the disk was being attached", w.free)
	}
}

// AND SO DOES A VERIFICATION'S PROBE, which boots the same way.
func TestAVerificationsProbeWaitsForItsDiskWithListingsHeldOff(t *testing.T) {
	_, p := verifiedStub(t)
	w := watchGate(p)

	mustVerify(t, p)

	w.mu.Lock()
	defer w.mu.Unlock()

	probed := false

	for _, name := range w.names {
		if strings.HasPrefix(name, "billetprobe-") {
			probed = true
		}
	}

	if !probed {
		t.Fatalf("the probe never waited for its disk to be attached (asked about %v)", w.names)
	}

	if len(w.free) > 0 {
		t.Errorf("the probe waited for %v's disk with the start gate free", w.free)
	}
}

// A LISTING DOES NOT RUN WHILE A START IS IN ITS DISK'S WINDOW: it waits, and past the
// window it refuses rather than reading the disk under the attach.
func TestAListingWaitsOutAStartAndRefusesPastItsWindow(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	p.startWindow = 100 * time.Millisecond

	p.startGate.RLock()

	_, err := p.list(t.Context())
	if err == nil || !strings.Contains(err.Error(), "still attaching its disk") {
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
		t.Fatalf("list once the start attached: %v", err)
	}
}

// A `tart run` THAT ENDS WITHOUT ATTACHING ITS DISK FAILS THE START AT ONCE, with tart's
// reason, rather than after the whole window.
func TestARunThatEndsFailsTheStartAtOnce(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	p.startWindow = time.Minute

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

// THE REAL ANSWER: attached only when a Virtualization.framework process holds the disk,
// found through lsof without opening it. A holder that is gone is skipped, one that cannot
// be read is could-not-tell, and anything else holding the disk is not the VM.
func TestTheDiskIsAttachedOnlyWhenTheVMServiceHoldsIt(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	p.diskAttached = nil

	dir := t.TempDir()
	answer := filepath.Join(dir, "lsof-answer")
	p.lsof = filepath.Join(dir, "lsof")
	writeExecutable(t, p.lsof, "#!/bin/sh\ncat \""+answer+"\"\n")

	paths := map[int]error{}
	names := map[int]string{}
	p.processPath = func(pid int) (string, error) {
		if err := paths[pid]; err != nil {
			return "", err
		}

		return names[pid], nil
	}

	for _, tc := range []struct {
		name    string
		lsof    string
		holders map[int]string
		errs    map[int]error
		want    bool
		wantErr bool
	}{
		{"nothing holds it", "", nil, nil, false, false},
		{"the VM service holds it", "41\n", map[int]string{41: vmService}, nil, true, false},
		{"only tart holds it", "42\n", map[int]string{42: "/opt/homebrew/bin/tart"}, nil, false, false},
		{"a gone holder beside the VM service", "43\n44\n", map[int]string{44: vmService},
			map[int]error{43: syscall.ESRCH}, true, false},
		{"a holder that cannot be read", "45\n", nil, map[int]error{45: syscall.EPERM}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(answer, []byte(tc.lsof), 0o600); err != nil {
				t.Fatal(err)
			}

			clear(names)
			clear(paths)

			for pid, path := range tc.holders {
				names[pid] = path
			}

			for pid, err := range tc.errs {
				paths[pid] = err
			}

			got, err := p.vmHoldsDisk(t.Context(), "billet-lease1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("vmHoldsDisk = %v, %v; want an error: %v", got, err, tc.wantErr)
			}

			if got != tc.want {
				t.Errorf("vmHoldsDisk = %v, want %v", got, tc.want)
			}

			if tc.wantErr && !errors.Is(err, syscall.EPERM) {
				t.Errorf("vmHoldsDisk = %v, want the unreadable holder's cause", err)
			}
		})
	}
}
