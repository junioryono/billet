package main

import (
	"strings"
	"testing"
)

func TestServerUpgradeProbeCannotAdvertiseEvenInDryRun(t *testing.T) {
	err := cmdServer(t.Context(), newLifecycle(func() {}),
		[]string{"--dry-run", "--upgrade-probe"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("server dry-run upgrade probe error = %v, want mutual-exclusion refusal", err)
	}
}

func TestNodeUpgradeProbeCannotEnroll(t *testing.T) {
	err := cmdNode(t.Context(), newLifecycle(func() {}),
		[]string{"--enroll", "--upgrade-probe"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("node enrollment upgrade probe error = %v, want mutual-exclusion refusal", err)
	}
}

// THE HOLD FLAG MEANS NOTHING WITHOUT THE PROBE, and a steady-state service that
// was accidentally told to hold would be one whose meaning nobody could read
// from its ExecStart, so it is refused rather than ignored.
func TestServerHoldWithoutProbeIsRefused(t *testing.T) {
	err := cmdServer(t.Context(), newLifecycle(func() {}), []string{"--upgrade-probe-hold"})
	if err == nil || !strings.Contains(err.Error(), "--upgrade-probe-hold needs --upgrade-probe") {
		t.Fatalf("server hold without probe error = %v, want the refusal naming both flags", err)
	}
}

func TestNodeHoldWithoutProbeIsRefused(t *testing.T) {
	err := cmdNode(t.Context(), newLifecycle(func() {}), []string{"--upgrade-probe-hold"})
	if err == nil || !strings.Contains(err.Error(), "--upgrade-probe-hold needs --upgrade-probe") {
		t.Fatalf("node hold without probe error = %v, want the refusal naming both flags", err)
	}
}
