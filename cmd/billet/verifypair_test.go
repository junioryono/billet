package main

import (
	"strings"
	"testing"
)

// A GENERATION THAT ALREADY NAMES A KERNEL BOOTED THAT KERNEL, so verification
// CONFIRMS the pairing rather than replacing it.
//
// The launch path resolves the recorded kernel in preference to the node's
// configuration, so on any normally-pulled generation the thing that just booted
// is the thing already recorded. Writing the configured kernel afterwards would
// prove kernel A works and then record kernel B -- and then publish that through
// @verified, which is worse than not recording at all.
func TestKernelToRecordConfirmsAnExistingPairingInsteadOfReplacingIt(t *testing.T) {
	record, note := kernelToRecord("vmlinux-6.1.155-ea1d42638d13", "vmlinux-other-aaaaaaaaaaaa")

	if record != "" {
		t.Fatalf("verification would have recorded %q over an existing pairing; the launch "+
			"booted the recorded one, so that is what was proved", record)
	}

	if !strings.Contains(note, "vmlinux-6.1.155-ea1d42638d13") {
		t.Errorf("the note does not name the pairing that was confirmed: %q", note)
	}
}

// A GENERATION THAT NAMES NONE BOOTED THE CONFIGURED KERNEL, because that is what
// the launch falls back to -- so that is the pairing this boot proved, and the one
// to record.
func TestKernelToRecordRecordsTheConfiguredKernelWhenNothingIsPaired(t *testing.T) {
	record, _ := kernelToRecord("", "vmlinux-6.1.155-ea1d42638d13")

	if record != "vmlinux-6.1.155-ea1d42638d13" {
		t.Fatalf("recorded %q; an unpaired generation boots the configured kernel and that "+
			"is what this verification proved", record)
	}
}

// NEITHER MEANS THE KERNEL IS OUTSIDE THE MANAGED DIRECTORY. Recording a bare base
// name for a file somewhere else would have every launch look for it in a
// directory it is not in, turning a working configuration into a hard failure on
// the next job.
func TestKernelToRecordRecordsNothingWhenTheKernelIsUnmanaged(t *testing.T) {
	record, note := kernelToRecord("", "")

	if record != "" {
		t.Fatalf("recorded %q for a kernel outside the managed directory", record)
	}

	if note == "" {
		t.Error("said nothing about a generation that stays unpaired")
	}
}
