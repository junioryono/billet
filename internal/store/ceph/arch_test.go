package ceph

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// learnClient is importClient holding a generation-map lock of its own, which
// LearnArchitectures needs before it maps anything.
func learnClient(t *testing.T, f *importFake) *Client {
	t.Helper()

	c := importClient(t, f)
	c.mapLock = filepath.Join(t.TempDir(), "ceph-generation-maps.lock")

	return c
}

// elfHead is the first bytes of a little-endian 64-bit ELF executable for the
// given machine number, as far as elfMachine reads.
func elfHead(machine uint16) []byte {
	head := make([]byte, 64)
	copy(head, "\x7fELF")
	head[4], head[5] = 2, 1 // ELFCLASS64, ELFDATA2LSB
	head[18], head[19] = byte(machine), byte(machine>>8)

	return head
}

// archMeta is an `image-meta list` table of the given rows.
func archMeta(rows ...string) string {
	return "Key  Value\n" + strings.Join(rows, "\n") + "\n"
}

func TestTheELFMachineIsReadFromTheHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		head []byte
		want string
	}{
		{"x86-64", elfHead(62), "x86_64"},
		{"aarch64", elfHead(183), "aarch64"},
		{"big-endian aarch64", func() []byte {
			h := elfHead(0)
			h[5], h[18], h[19] = 2, 0, 183

			return h
		}(), "aarch64"},
	} {
		got, err := elfMachine(tc.head)
		if err != nil || got != tc.want {
			t.Errorf("%s: elfMachine = %q, %v, want %q", tc.name, got, err, tc.want)
		}
	}

	// COULD-NOT-TELL, NEVER A GUESS: debugfs answers 0 for a path it could not
	// find, so nothing, a stray complaint, a short read, another machine and an
	// unknown byte order are each refused.
	for name, head := range map[string][]byte{
		"nothing":      nil,
		"not an ELF":   []byte("File not found by ext2_lookup while dumping"),
		"a short read": elfHead(62)[:12],
		"riscv":        elfHead(243),
		"a damaged magic": func() []byte {
			h := elfHead(62)
			h[0] = 0

			return h
		}(),
		"no byte order": func() []byte { h := elfHead(62); h[5] = 0; return h }(),
	} {
		if got, err := elfMachine(head); err == nil {
			t.Errorf("%s: elfMachine = %q, want a refusal", name, got)
		}
	}
}

// AN IMPORT RECORDS ITS ARCHITECTURE on the generation it makes, first among the
// generation's fields.
func TestAnImportRecordsTheMachineItsImageWasBuiltFor(t *testing.T) {
	t.Parallel()

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device}

	gen, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-arm64", raw, "aarch64",
		"2.336.0", "vmlinux-6.1.155-ea1d42638d13", "7", importAt)
	if err != nil {
		t.Fatalf("ImportGeneration: %v", err)
	}

	if !f.ranWith("image-meta", "set", "ubuntu-2404-arm64", ArchKey+"."+gen, "aarch64") {
		t.Fatalf("the import did not record its architecture; billet ran %v", f.calls)
	}

	sets := slices.IndexFunc(f.calls, func(call []string) bool {
		return matchesCall(call, []string{"image-meta", "set"})
	})
	if sets < 0 || !slices.Contains(f.calls[sets], ArchKey+"."+gen) {
		t.Errorf("the first field recorded was not the architecture: %v", f.calls[sets])
	}
}

// AND AN IMPORT THAT CANNOT SAY WHAT IT IS IS REFUSED, before anything is written.
func TestAnImportNamingNoMachineIsRefused(t *testing.T) {
	t.Parallel()

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, " ",
		"2.336.0", "", "7", importAt)
	if err == nil || !strings.Contains(err.Error(), "needs the machine") {
		t.Fatalf("ImportGeneration = %v, want a refusal naming the missing machine", err)
	}

	if f.ranWith("device", "map") || f.ranWith("lock", "add") {
		t.Errorf("a refused import touched the cluster; billet ran %v", f.calls)
	}
}

// ONE ARCHITECTURE AN IMAGE: an import is refused unless every generation the
// image holds is known to be of its own, judged under the publish lock and before
// the head is touched.
func TestAnImportIsRefusedUnlessEveryGenerationIsKnownToBeOfItsMachine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		snaps    []string
		meta     string
		want     string
		accepted bool
	}{
		{
			name: "another machine's", snaps: []string{"g20260801000000", "g20260802000000"},
			meta: archMeta(ArchKey+".g20260801000000  x86_64", ArchKey+".g20260802000000  aarch64"),
			want: "g20260802000000 (aarch64)",
		},
		{
			name: "one that records none", snaps: []string{"g20260801000000", "g20260802000000"},
			meta: archMeta(ArchKey + ".g20260801000000  x86_64"),
			want: "billet images unpromote",
		},
		{
			name: "all of its own", snaps: []string{"g20260801000000"},
			meta:     archMeta(ArchKey + ".g20260801000000  x86_64"),
			accepted: true,
		},
		{name: "none at all", accepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, device := stageRaw(t, "content")
			f := &importFake{device: device, infoJSON: `{"size": 8388608}`, snapshots: tc.snaps,
				metadata: tc.meta}

			_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
				"2.336.0", "", "7", importAt)

			if tc.accepted {
				if err != nil {
					t.Fatalf("an import into an image of its own machine was refused: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ImportGeneration = %v, want a refusal naming %q", err, tc.want)
			}

			if f.ranWith("device", "map") || f.ranWith("snap", "create") {
				t.Errorf("a refused import wrote into the image; billet ran %v", f.calls)
			}

			if !f.ranWith("lock", "add") {
				t.Errorf("the image's generations were not judged under the publish lock; billet ran %v",
					f.calls)
			}
		})
	}
}

// @VERIFIED NEVER RESOLVES TO ANOTHER MACHINE'S GENERATION, and one that records
// none resolves as it always did.
func TestVerifiedNeverResolvesToAnotherMachinesGeneration(t *testing.T) {
	t.Parallel()

	const older, newer, newest = "g20260801000000", "g20260802000000", "g20260803000000"

	for _, tc := range []struct {
		name string
		meta []string
		want string
	}{
		{
			name: "the newest is another machine's",
			meta: []string{ArchKey + "." + older + "  x86_64", ArchKey + "." + newer + "  x86_64",
				ArchKey + "." + newest + "  aarch64"},
			want: newer,
		},
		{
			name: "the newest records none",
			meta: []string{ArchKey + "." + older + "  x86_64", ArchKey + "." + newer + "  aarch64"},
			want: newest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rows := slices.Concat(tc.meta, []string{
				VerifiedKey + "." + older + "  2026-08-01T00:00:00Z", GuestContractKey + "." + older + "  7",
				VerifiedKey + "." + newer + "  2026-08-02T00:00:00Z", GuestContractKey + "." + newer + "  7",
				VerifiedKey + "." + newest + "  2026-08-03T00:00:00Z", GuestContractKey + "." + newest + "  7",
			})
			f := &importFake{snapshots: []string{older, newer, newest}, metadata: archMeta(rows...)}
			c := importClient(t, f)
			c.machine = "x86_64"

			got, err := c.ResolveGeneration(t.Context(), "ubuntu-2404-x64@verified", "7")
			if err != nil || got != "ubuntu-2404-x64@"+tc.want {
				t.Fatalf("@verified resolved to %q, %v, want %s", got, err, tc.want)
			}
		})
	}
}

// NOR DOES AN EXACT GENERATION: pinning another machine's is refused by name, and
// a pinned generation that records none, or this host's, is returned unchanged.
func TestAnExactGenerationOfAnotherMachineIsRefused(t *testing.T) {
	t.Parallel()

	f := &importFake{metadata: archMeta(ArchKey+".g20260801000000  aarch64",
		ArchKey+".g20260802000000  x86_64")}
	c := importClient(t, f)
	c.machine = "x86_64"

	_, err := c.ResolveGeneration(t.Context(), "ubuntu-2404-x64@g20260801000000", "7")
	if !errors.Is(err, ErrOtherMachine) || !strings.Contains(err.Error(), "was built for aarch64 and this host is x86_64") {
		t.Fatalf("ResolveGeneration = %v, want ErrOtherMachine naming both machines", err)
	}

	for _, ref := range []string{"ubuntu-2404-x64@g20260802000000", "ubuntu-2404-x64@g20260803000000"} {
		if got, err := c.ResolveGeneration(t.Context(), ref, "7"); err != nil || got != ref {
			t.Errorf("ResolveGeneration(%s) = %q, %v, want it unchanged", ref, got, err)
		}
	}
}

// AND A COMPLETION ROOT OF ANOTHER MACHINE IS NOT BOOTED to complete an image.
func TestACompletionRootOfAnotherMachineIsRefused(t *testing.T) {
	t.Parallel()

	f := &importFake{contract: "7", metadata: archMeta(ArchKey + ".c20260801000000  aarch64")}
	c := importClient(t, f)
	c.machine = "x86_64"

	_, err := c.ResolveCompletionRoot(t.Context(), "ubuntu-2404-x64@c20260801000000", "7")
	if err == nil || !strings.Contains(err.Error(), "was built for aarch64") {
		t.Fatalf("ResolveCompletionRoot = %v, want a refusal naming its machine", err)
	}
}

// A GENERATION THAT RECORDS NO ARCHITECTURE IS READ FROM ITSELF: mapped read-only,
// its probe binary's ELF header read, and what was read recorded under the publish
// lock. One that cannot be read stays unknown and says why; one that already
// records one is not mapped.
func TestLearnArchitecturesReadsEachGenerationThatRecordsNone(t *testing.T) {
	t.Parallel()

	const known, readable, unreadable = "g20260801000000", "g20260802000000", "g20260803000000"

	const device = "/dev/rbd7"
	f := &importFake{device: device, snapshots: []string{known, readable, unreadable},
		metadata: archMeta(ArchKey + "." + known + "  x86_64")}
	c := learnClient(t, f)

	var probed []string

	c.readProbe = func(_ context.Context, dev string) ([]byte, error) {
		probed = append(probed, dev)
		if len(probed) == 1 {
			return elfHead(183), nil
		}

		return []byte("File not found by ext2_lookup"), nil
	}

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err != nil {
		t.Fatalf("LearnArchitectures: %v", err)
	}

	if len(learned) != 2 || learned[0].Generation != readable || learned[0].Machine != "aarch64" ||
		learned[0].Err != nil || !learned[0].Recorded || learned[1].Generation != unreadable ||
		learned[1].Err == nil || learned[1].Recorded {
		t.Fatalf("learned %+v, want %s read and recorded as aarch64 and %s unreadable", learned,
			readable, unreadable)
	}

	for _, gen := range []string{readable, unreadable} {
		if !f.ranWith("device", "map", "billet-images/ubuntu-2404-x64@"+gen, "--read-only") {
			t.Errorf("%s was not mapped read-only; billet ran %v", gen, f.calls)
		}
	}

	if f.ranWith("device", "map", "billet-images/ubuntu-2404-x64@"+known) {
		t.Errorf("a generation that records its architecture was mapped")
	}

	if f.appliedMetadata[ArchKey+"."+unreadable] {
		t.Errorf("an unreadable generation's architecture was recorded: %v", f.appliedMetadata)
	}

	// THE VALUE READ, WRITTEN WHILE THE PUBLISH LOCK IS HELD.
	set := slices.IndexFunc(f.calls, func(call []string) bool {
		return matchesCall(call, []string{"image-meta", "set", "ubuntu-2404-x64", ArchKey + "." + readable})
	})
	added := slices.IndexFunc(f.calls, func(call []string) bool { return matchesCall(call, []string{"lock", "add"}) })
	removed := slices.IndexFunc(f.calls, func(call []string) bool { return matchesCall(call, []string{"lock", "rm"}) })

	if set < 0 || f.calls[set][len(f.calls[set])-1] != "aarch64" {
		t.Fatalf("%s's architecture was not recorded as aarch64; billet ran %v", readable, f.calls)
	}

	if added < 0 || removed < 0 || added > set || set > removed {
		t.Errorf("the record was not written while the publish lock was held (lock add at %d, "+
			"set at %d, lock rm at %d)", added, set, removed)
	}
}

// AND A GENERATION REAPED WHILE IT WAS READ IS NOT DESCRIBED AFTERWARDS, which
// would leave a key nothing reaps.
func TestLearnArchitecturesRecordsNothingForAGenerationGoneSinceItWasRead(t *testing.T) {
	t.Parallel()

	const gone = "g20260802000000"

	const device = "/dev/rbd7"
	f := &importFake{device: device, snapshots: []string{gone}}
	c := learnClient(t, f)
	c.readProbe = func(context.Context, string) ([]byte, error) {
		f.snapshots = nil

		return elfHead(62), nil
	}

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err != nil || len(learned) != 1 || learned[0].Machine != "x86_64" || learned[0].Recorded ||
		learned[0].NotRecorded == nil || !strings.Contains(learned[0].NotRecorded.Error(), "removed") {
		t.Fatalf("LearnArchitectures = %+v, %v, want it read, not recorded, and saying why", learned, err)
	}

	if f.appliedMetadata[ArchKey+"."+gone] {
		t.Error("a generation reaped while it was read was described afterwards")
	}
}

// NOR IS ONE ANOTHER NODE RECORDED WHILE THIS ONE READ IT overwritten: the record
// that is there was written under the same lock, by a reader of the same bytes or
// by the import itself.
func TestLearnArchitecturesKeepsARecordWrittenWhileItRead(t *testing.T) {
	t.Parallel()

	const gen = "g20260802000000"

	f := &importFake{device: "/dev/rbd7", snapshots: []string{gen}}
	c := learnClient(t, f)
	c.readProbe = func(context.Context, string) ([]byte, error) {
		f.metadata = archMeta(ArchKey + "." + gen + "  aarch64")

		return elfHead(62), nil
	}

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err != nil || len(learned) != 1 || learned[0].Recorded || learned[0].NotRecorded == nil ||
		!strings.Contains(learned[0].NotRecorded.Error(), "recorded as aarch64") {
		t.Fatalf("LearnArchitectures = %+v, %v, want it not recorded, saying why", learned, err)
	}

	if f.appliedMetadata[ArchKey+"."+gen] {
		t.Error("a record written while the generation was read was overwritten")
	}
}

// A MAP THAT FAILED IS COULD-NOT-TELL for that generation alone.
func TestLearnArchitecturesKeepsAGenerationItCouldNotMapUnknown(t *testing.T) {
	t.Parallel()

	f := &importFake{snapshots: []string{"g20260802000000"}, failOn: "device map",
		failErr: errors.New("rbd: map failed: (110) Connection timed out")}
	c := learnClient(t, f)

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err != nil || len(learned) != 1 || learned[0].Err == nil || learned[0].Machine != "" {
		t.Fatalf("LearnArchitectures = %+v, %v, want the generation unknown with its reason", learned, err)
	}

	if f.ranWith("image-meta", "set") || f.ranWith("lock", "add") {
		t.Errorf("nothing was read and something was recorded; billet ran %v", f.calls)
	}
}

// WITHOUT THE HOST'S GENERATION-MAP LOCK NOTHING IS MAPPED, SWEPT OR LEARNED: another
// process could unmap the device between the map and the read, and a device reused
// for its next map would be read as this generation.
func TestWithoutTheHostsMapLockNothingIsMappedOrSwept(t *testing.T) {
	t.Parallel()

	f := &importFake{device: "/dev/rbd7", snapshots: []string{"g20260802000000"}}
	c := importClient(t, f)
	c.readProbe = func(context.Context, string) ([]byte, error) { return elfHead(62), nil }

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err != nil || len(learned) != 1 || !errors.Is(learned[0].Err, errNoMapLock) {
		t.Fatalf("LearnArchitectures = %+v, %v, want the generation unknown for want of the lock", learned, err)
	}

	if err := c.SweepGenerationMaps(t.Context()); !errors.Is(err, errNoMapLock) {
		t.Errorf("SweepGenerationMaps = %v, want it refused for want of the lock", err)
	}

	if f.ranWith("device") || f.ranWith("image-meta", "set") {
		t.Errorf("a client without the lock touched a mapping or recorded something; billet ran %v", f.calls)
	}
}

// ANOTHER NODE'S PUBLISH LOCK DOES NOT FAIL THE PULL: what was read goes unrecorded
// and says why, and the import judges again under the lock it takes itself.
func TestLearnArchitecturesLeavesUnrecordedWhatAHeldLockKeepsOut(t *testing.T) {
	t.Parallel()

	f := &importFake{device: "/dev/rbd7", snapshots: []string{"g20260802000000"},
		lockHeld: fmt.Sprintf("billet-build-otherhost-123-%d", time.Now().Add(-time.Minute).Unix())}
	c := learnClient(t, f)
	c.readProbe = func(context.Context, string) ([]byte, error) { return elfHead(62), nil }

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err != nil {
		t.Fatalf("a held publish lock failed the learning: %v", err)
	}

	if len(learned) != 1 || learned[0].Machine != "x86_64" || learned[0].Recorded ||
		learned[0].NotRecorded == nil || !strings.Contains(learned[0].NotRecorded.Error(), "publish lock") {
		t.Fatalf("learned %+v, want it read and not recorded for the held lock", learned)
	}

	if f.ranWith("image-meta", "set") {
		t.Errorf("something was recorded without the publish lock; billet ran %v", f.calls)
	}
}

// AN IMPORT'S ANSWER CAN BE HAD BEFORE ANYTHING IS DOWNLOADED, from the same rule.
func TestCheckImportArchAnswersAsTheImportWould(t *testing.T) {
	t.Parallel()

	f := &importFake{infoJSON: `{"size": 8388608}`, snapshots: []string{"g20260801000000"},
		metadata: archMeta(ArchKey + ".g20260801000000  aarch64")}
	c := importClient(t, f)

	if err := c.CheckImportArch(t.Context(), "ubuntu-2404-x64", "x86_64"); err == nil ||
		!strings.Contains(err.Error(), "one image holds one architecture") {
		t.Fatalf("CheckImportArch = %v, want the refusal an import would give", err)
	}

	if err := c.CheckImportArch(t.Context(), "ubuntu-2404-x64", "aarch64"); err != nil {
		t.Errorf("an import of the image's own machine was refused: %v", err)
	}

	if f.ranWith("lock", "add") {
		t.Errorf("the advisory check took the publish lock; billet ran %v", f.calls)
	}
}

// AN ARCHITECTURE THAT COULD NOT BE RECORDED STOPS THE IMPORT BEFORE THE SNAPSHOT IS
// MADE, and the write, which may have committed, is taken back.
func TestAnImportWhoseArchitectureCannotBeRecordedMakesNothing(t *testing.T) {
	t.Parallel()

	const generation = "g20260815041709"

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device, failMetaSetKey: ArchKey + "." + generation}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "", "7", importAt)
	if err == nil || !strings.Contains(err.Error(), "its architecture could not be recorded") {
		t.Fatalf("ImportGeneration = %v, want the import refused for its architecture", err)
	}

	if f.ranWith("snap", "create") {
		t.Errorf("a snapshot was made without its architecture; billet ran %v", f.calls)
	}

	if f.appliedMetadata[ArchKey+"."+generation] {
		t.Error("the architecture written before the failure was left behind")
	}
}

// WHAT A HELD LOCK KEPT FROM BEING RECORDED, THIS CLIENT'S IMPORT RECORDS UNDER ITS
// OWN: the pull read the generation as its machine, and the import, holding the
// publish lock and finding it still unrecorded, writes it before it judges.
func TestAnImportRecordsWhatItsPullReadAndCouldNotRecord(t *testing.T) {
	t.Parallel()

	const old = "g20260801000000"

	raw, device := stageRaw(t, "content")
	f := &importFake{device: "/dev/rbd7", infoJSON: `{"size": 8388608}`, snapshots: []string{old},
		lockHeld: fmt.Sprintf("billet-build-otherhost-123-%d", time.Now().Add(-time.Minute).Unix())}
	c := learnClient(t, f)
	c.readProbe = func(context.Context, string) ([]byte, error) { return elfHead(62), nil }

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err != nil || len(learned) != 1 || learned[0].Recorded || learned[0].NotRecorded == nil {
		t.Fatalf("LearnArchitectures = %+v, %v, want it read and kept from being recorded", learned, err)
	}

	if err := c.CheckImportArch(t.Context(), "ubuntu-2404-x64", "x86_64"); err != nil {
		t.Errorf("the pre-check refused a generation this client read as its own machine: %v", err)
	}

	if f.ranWith("image-meta", "set") {
		t.Fatalf("the pre-check recorded something without the publish lock; billet ran %v", f.calls)
	}

	// THE IMPORT'S CALLS ALONE from here, so its own lock is the one the write is
	// held to, not the learning's refused attempt; and whether that lock is held at
	// the instant of the write is asked inside it.
	f.lockHeld, f.device, f.calls, f.deadlines = "", device, nil, nil

	heldAtWrite := false
	f.onMetaSet = func(key string) {
		if key == ArchKey+"."+old {
			heldAtWrite = f.lockTaken != ""
		}
	}

	if _, err := c.ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0", "", "7",
		importAt); err != nil {
		t.Fatalf("an import into an image this client read was refused: %v", err)
	}

	set := slices.IndexFunc(f.calls, func(call []string) bool {
		return matchesCall(call, []string{"image-meta", "set", "ubuntu-2404-x64", ArchKey + "." + old})
	})
	added := slices.IndexFunc(f.calls, func(call []string) bool { return matchesCall(call, []string{"lock", "add"}) })

	if set < 0 || f.calls[set][len(f.calls[set])-1] != "x86_64" {
		t.Fatalf("the import did not record what was read; billet ran %v", f.calls)
	}

	if added < 0 || added > set || !heldAtWrite {
		t.Errorf("the import recorded what was read without holding the publish lock (lock add at %d, "+
			"set at %d, held at the write: %v)", added, set, heldAtWrite)
	}

	if f.lockTaken != "" {
		t.Error("the import's publish lock is still held after it returned")
	}
}

// AND WHAT IT READ AS ANOTHER MACHINE'S REFUSES THE IMPORT, as a record would.
func TestAnImportIsRefusedWhatItsPullReadAsAnotherMachine(t *testing.T) {
	t.Parallel()

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device, infoJSON: `{"size": 8388608}`, snapshots: []string{"g20260801000000"}}
	c := learnClient(t, f)
	c.rememberRead("ubuntu-2404-x64", "g20260801000000", "aarch64", 1)

	if err := c.CheckImportArch(t.Context(), "ubuntu-2404-x64", "x86_64"); err == nil ||
		!strings.Contains(err.Error(), "g20260801000000 (aarch64)") {
		t.Fatalf("CheckImportArch = %v, want the generation read as aarch64 refused", err)
	}

	if _, err := c.ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0", "", "7",
		importAt); err == nil || !strings.Contains(err.Error(), "g20260801000000 (aarch64)") {
		t.Fatalf("ImportGeneration = %v, want the generation read as aarch64 refused", err)
	}

	if f.ranWith("device", "map") || f.ranWith("snap", "create") {
		t.Errorf("a refused import wrote into the image; billet ran %v", f.calls)
	}
}

// AND WHAT IT READ OF A SNAPSHOT SINCE REMOVED AND MADE AGAIN UNDER THE SAME NAME
// SAYS NOTHING OF THE NEW ONE: the import is refused as could-not-tell, and the
// replacement is not given the old snapshot's machine.
func TestARememberedReadDoesNotDescribeASnapshotMadeAgainUnderItsName(t *testing.T) {
	t.Parallel()

	const gen = "g20260801000000"

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device, infoJSON: `{"size": 8388608}`, snapshots: []string{gen},
		snapIDs: map[string]int{gen: 9}}
	c := learnClient(t, f)
	c.rememberRead("ubuntu-2404-x64", gen, "x86_64", 4)

	_, err := c.ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0", "", "7", importAt)
	if err == nil || !strings.Contains(err.Error(), "not recorded and was not read on this host") {
		t.Fatalf("ImportGeneration = %v, want the replacement refused as unread", err)
	}

	if f.ranWith("image-meta", "set", "ubuntu-2404-x64", ArchKey+"."+gen) {
		t.Errorf("the replacement was given what was read of the snapshot before it; billet ran %v", f.calls)
	}
}

// A CREATE WHOSE OUTCOME CANNOT BE READ KEEPS THE ARCHITECTURE IT RECORDED: the
// snapshot may exist, and one without its record would resolve on any host.
func TestACreateWhoseOutcomeCannotBeReadKeepsItsArchitecture(t *testing.T) {
	t.Parallel()

	const generation = "g20260815041709"

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device, failSnapCreateApplied: true, failSnapListAfterCreate: true}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "", "7", importAt)
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("ImportGeneration = %v, want the import refused as could-not-tell", err)
	}

	if !f.appliedMetadata[ArchKey+"."+generation] || f.ranWith("image-meta", "remove", ArchKey+"."+generation) {
		t.Error("the architecture of a snapshot that may exist was taken away")
	}
}

// A PUBLISH LOCK TAKEN TO RECORD AND NOT RELEASED IS SAID, beside what was recorded:
// every publisher in the deployment meets it next, and the entries still say what
// was written.
func TestLearnArchitecturesSaysAPublishLockItCouldNotRelease(t *testing.T) {
	t.Parallel()

	f := &importFake{device: "/dev/rbd7", snapshots: []string{"g20260802000000"},
		lockRmErr: errors.New("rbd: lock rm timed out")}
	c := learnClient(t, f)
	c.readProbe = func(context.Context, string) ([]byte, error) { return elfHead(62), nil }

	learned, err := c.LearnArchitectures(t.Context(), "ubuntu-2404-x64")
	if err == nil || !strings.Contains(err.Error(), "was not released") {
		t.Fatalf("LearnArchitectures = %v, want the unreleased lock said", err)
	}

	if len(learned) != 1 || !learned[0].Recorded || learned[0].NotRecorded != nil {
		t.Errorf("learned %+v, want the record that was written said as written", learned)
	}
}

// A RECORD BEATS A READ: what this client read never overwrites, or outvotes,
// what another node recorded for the same generation meanwhile.
func TestARecordedArchitectureBeatsWhatThisClientRead(t *testing.T) {
	t.Parallel()

	const gen = "g20260801000000"

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device, infoJSON: `{"size": 8388608}`, snapshots: []string{gen},
		metadata: archMeta(ArchKey + "." + gen + "  aarch64")}
	c := learnClient(t, f)
	c.rememberRead("ubuntu-2404-x64", gen, "x86_64", 1)

	if err := c.CheckImportArch(t.Context(), "ubuntu-2404-x64", "x86_64"); err == nil ||
		!strings.Contains(err.Error(), gen+" (aarch64)") {
		t.Errorf("CheckImportArch = %v, want the recorded aarch64 to refuse", err)
	}

	if _, err := c.ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0", "", "7",
		importAt); err == nil || !strings.Contains(err.Error(), gen+" (aarch64)") {
		t.Errorf("ImportGeneration = %v, want the recorded aarch64 to refuse", err)
	}

	if f.ranWith("image-meta", "set", "ubuntu-2404-x64", ArchKey+"."+gen) {
		t.Errorf("another node's record was overwritten with this client's read; billet ran %v", f.calls)
	}
}
