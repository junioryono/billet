package firecracker

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// A RESTORE LOADS ONLY THE GOLDEN IT WAS TOLD TO EXPECT. Evidence that is missing, that
// names another builder, disk or other bytes, and a snapshot another deployment
// published, are each refused before anything is claimed: no jail, no clone, no VMM.
func TestARestoreOfAGoldenItWasNotToldToExpectIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		want   func(Evidence) Evidence
		record func(*compatibility)
	}{
		{name: "no evidence at all", want: func(Evidence) Evidence { return Evidence{} }},
		{name: "evidence with no memory digest", want: func(e Evidence) Evidence {
			e.Memory = ""

			return e
		}},
		{name: "another deployment", want: func(e Evidence) Evidence {
			e.Deployment = otherTestDeployment

			return e
		}},
		{name: "another builder", want: func(e Evidence) Evidence {
			e.Builder = "0000000000000000000000000000dead"

			return e
		}},
		{name: "another system disk", want: func(e Evidence) Evidence {
			e.Disk = goldenDiskPrefix + "0000000000000000000000000000dead@" + fakeGeneration

			return e
		}},
		{name: "other state", want: func(e Evidence) Evidence {
			e.State = digestOf("other state")

			return e
		}},
		{name: "other memory", want: func(e Evidence) Evidence {
			e.Memory = digestOf("other memory")

			return e
		}},
		{
			// EMPTY IS NOT A VALUE THAT MATCHES: a record with no memory digest and
			// evidence with none agree, and are still no evidence of the memory.
			name: "a record and evidence that both lack the memory's digest",
			want: func(e Evidence) Evidence {
				e.Memory = ""

				return e
			},
			record: func(c *compatibility) { c.MemoryDigest = "" },
		},
		{
			name:   "a snapshot published under the name with other memory",
			want:   func(e Evidence) Evidence { return e },
			record: func(c *compatibility) { c.MemoryDigest = digestOf("other memory") },
		},
		{
			// THE RECORD'S DEPLOYMENT IS JUDGED AGAINST THIS NODE'S, NOT ONLY THE
			// CALLER'S: evidence that agrees with a foreign record is still refused.
			name: "another deployment's snapshot, with evidence that agrees",
			want: func(e Evidence) Evidence {
				e.Deployment = otherTestDeployment

				return e
			},
			record: func(c *compatibility) { c.Deployment = otherTestDeployment },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			dir := publish(t, h, "gold-1", 0o444)

			if tc.record != nil {
				writeCompatibility(t, h, dir, tc.record)
			}

			_, vmm, err := h.restoreExpecting(t, "gold-1", tc.want(h.theEvidence()))
			if !errors.Is(err, errSnapshotNotExpected) {
				t.Fatalf("Restore = %v, want errSnapshotNotExpected", err)
			}

			if vmm != nil {
				t.Error("a refused restore started a VMM")
			}

			if _, _, cloned := h.disk.goldenCalls(); len(cloned) != 0 {
				t.Errorf("a refused restore cloned %v", cloned)
			}

			if _, err := os.Lstat(h.p.jailFor(aRestoreSpec().Name).dir()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused restore claimed a jail: %v", err)
			}
		})
	}
}

// THE EVIDENCE IS CHECKED AGAIN WHERE THE SNAPSHOT IS LINKED. The name was read before
// anything was claimed; a snapshot whose record changes under it before the link is
// refused there, and nothing of it is loaded.
func TestARestoreChecksTheEvidenceAgainWhereItLinksTheSnapshot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)

	h.disk.mu.Lock()
	h.disk.onCloneGolden = func() {
		writeCompatibility(t, h, dir, func(c *compatibility) { c.MemoryDigest = digestOf("other memory") })
	}
	h.disk.mu.Unlock()

	_, vmm, err := h.restoreWith(t, "gold-1")
	if !errors.Is(err, errSnapshotNotExpected) {
		t.Fatalf("Restore = %v, want errSnapshotNotExpected from the second check", err)
	}

	// THE CLONE HAPPENED, so the refusal came from the check after it, not before.
	if _, _, cloned := h.disk.goldenCalls(); len(cloned) != 1 {
		t.Fatalf("the restore cloned %v: the change was not staged between the two checks", cloned)
	}

	assertNothingLoaded(t, h, vmm)
}

// A RECORD AN EARLIER BILLET WROTE SAYS WHAT TO DO: it carries no evidence, so it is
// refused, and the refusal says to capture the golden again.
func TestARecordOfAnEarlierFormatIsRefusedWithWhatToDo(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	dir := publish(t, h, "gold-1", 0o444)
	writeCompatibility(t, h, dir, func(c *compatibility) { c.Format = 2 })

	_, _, err := h.restoreWith(t, "gold-1")
	if !errors.Is(err, errSnapshotIncompatible) || !strings.Contains(err.Error(), "capture the golden again") {
		t.Fatalf("Restore = %v, want errSnapshotIncompatible saying to capture the golden again", err)
	}
}

// A GOLDEN'S DISK IS NAMED AFTER THE BUILDER'S LEASE, NOT THE SNAPSHOT, so two
// captures given one snapshot name, on nodes sharing a cache pool, never claim one
// disk.
func TestAGoldensDiskIsNamedAfterItsBuildersLease(t *testing.T) {
	t.Parallel()

	got, err := goldenNameFor(theInstance)
	if err != nil || got != goldenDiskPrefix+theLease {
		t.Fatalf("goldenNameFor(%s) = %q, %v, want %q", theInstance, got, err, goldenDiskPrefix+theLease)
	}

	for _, id := range []string{
		"gold-1", "billet-x", "billet-9d7ab98c551bceec9a81e5da847f683",
		"billet-9d7ab98c551bceec9a81e5da847f6837a", "billet-9D7AB98C551BCEEC9A81E5DA847F6837",
		"billet-9d7ab98c551bceec9a81e5da847f68@7", "billet-9d7ab98c551bceec9a81e5da847f68/7",
	} {
		if got, err := goldenNameFor(id); err == nil {
			t.Errorf("goldenNameFor(%q) = %q, want a refusal: it is not a lease's microVM", id, got)
		}
	}
}
