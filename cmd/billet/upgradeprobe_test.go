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
