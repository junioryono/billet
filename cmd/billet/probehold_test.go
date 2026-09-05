package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A PROBE NOT TOLD TO HOLD SAYS NOTHING AND DOES NOT WAIT. Under `billet
// host-upgrade` the candidate is a plain child and the parent waits for it to
// exit; a probe that held there hung every self-upgrade at the probe step (the
// rollout rehearsal, 2026-09-05). And it prints no readiness line, because to the
// parent that line means "stop me", which is the one thing this probe must never
// say. The environment does not decide any of this: a node's detached updater
// inherits its unit's notify socket, so the socket is set here on purpose.
func TestAProbeNotToldToHoldNeitherSpeaksNorWaits(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/nonexistent/billet-probe-test.sock")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	returned := make(chan struct{})

	var printed string

	go func() {
		printed = capture(t, func() { holdProbe(ctx, false, serverProbeReadyLine) })
		close(returned)
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case <-returned:
	case <-timer.C:
		t.Fatal("holdProbe waited without the hold flag; a parent waiting for exit would hang")
	}

	if strings.Contains(printed, upgradeProbeReady) {
		t.Fatalf("a probe not told to hold printed the readiness line, which a parent reads as "+
			"\"stop me\": %q", printed)
	}
}

// A PROBE TOLD TO HOLD ANNOUNCES ITSELF AND STAYS UP UNTIL STOPPED. The Ansible
// host role runs the probe as its unit's own ExecStart under Type=notify, passes
// the flag, and stops the unit itself; a probe that exited on its own there would
// leave the unit dead where the role expects it active, and one that held without
// the line would leave a v0.9.0 parent waiting on a line that never came.
func TestAProbeToldToHoldSpeaksAndWaitsToBeStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	returned := make(chan struct{})

	var printed string

	go func() {
		printed = capture(t, func() { holdProbe(ctx, true, serverProbeReadyLine) })
		close(returned)
	}()

	settle := time.NewTimer(200 * time.Millisecond)
	defer settle.Stop()

	select {
	case <-returned:
		t.Fatal("holdProbe returned with the hold flag before anything stopped it")
	case <-settle.C:
	}

	cancel()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case <-returned:
	case <-timer.C:
		t.Fatal("holdProbe did not return after its context was cancelled")
	}

	if !strings.Contains(printed, serverProbeReadyLine) {
		t.Fatalf("a holding probe did not print the readiness line: %q", printed)
	}
}
