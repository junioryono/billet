package ceph

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestACompletedGenerationRecordsItsComponentsSorted(t *testing.T) {
	raw, device := stageRaw(t, "content")
	f := &importFake{device: device}

	gen, err := importClient(t, f).ImportCompletedGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "", "7", &GenerationComponents{
			LeftOut:   []string{"google-chrome", "android-sdk"},
			Installed: map[string]string{"google-chrome": "155.0.8059.84"},
		}, importAt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	want := `{"left_out":["android-sdk","google-chrome"],"installed":{"google-chrome":"155.0.8059.84"}}`
	if !f.ranWith("image-meta", "set", ComponentsKey+"."+gen, want) {
		t.Fatalf("the components were not recorded as %s; billet ran %v", want, f.calls)
	}
}

// AN IMAGE THAT LEFT NOTHING OUT RECORDS NOTHING, so it reads as every generation
// imported before images left anything out.
func TestAGenerationThatLeftNothingOutRecordsNoComponents(t *testing.T) {
	raw, device := stageRaw(t, "content")
	f := &importFake{device: device}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "", "7", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	for _, call := range f.calls {
		if strings.Contains(strings.Join(call, " "), ComponentsKey) {
			t.Fatalf("an image that left nothing out recorded components: %v", call)
		}
	}
}

// A GENERATION WHOSE COMPONENTS CANNOT BE RECORDED IS WITHDRAWN, with every field
// written before it, because one that does not say what it left out reads as one
// that left nothing out.
func TestAGenerationWhoseComponentsCannotBeRecordedIsWithdrawn(t *testing.T) {
	const generation = "g20260815041709"

	raw, device := stageRaw(t, "content")
	f := &importFake{device: device, failMetaSetKey: ComponentsKey + "." + generation}

	_, err := importClient(t, f).ImportCompletedGeneration(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "vmlinux-6.1.155-ea1d42638d13", "7",
		&GenerationComponents{LeftOut: []string{"codeql"}}, importAt)
	if err == nil || !strings.Contains(err.Error(), "its components could not be recorded") {
		t.Fatalf("got %v, want the import refused for its components", err)
	}
	if !f.ranWith("snap", "rm", "ubuntu-2404-x64@"+generation) {
		t.Fatalf("the generation was not withdrawn; billet ran %v", f.calls)
	}
	for _, key := range []string{KernelKey, RunnerVersionKey, GuestContractKey, ComponentsKey} {
		if !f.ranWith("image-meta", "remove", key+"."+generation) {
			t.Errorf("the withdrawn generation kept %s; billet ran %v", key, f.calls)
		}
	}
}

func TestAComponentsRecordIsRefusedUnlessItReadsBackAsItself(t *testing.T) {
	for name, tc := range map[string]struct {
		record GenerationComponents
		want   string
	}{
		"nothing left out": {GenerationComponents{}, "names nothing left out"},
		"a bad name":       {GenerationComponents{LeftOut: []string{"Google"}}, `"Google" is not a component name`},
		"left out twice":   {GenerationComponents{LeftOut: []string{"codeql", "codeql"}}, "left out twice"},
		"installed, not left out": {GenerationComponents{LeftOut: []string{"codeql"},
			Installed: map[string]string{"firefox": "1"}}, "installed but was not left out"},
		"a bad version": {GenerationComponents{LeftOut: []string{"codeql"},
			Installed: map[string]string{"codeql": "2.26 4"}}, `"2.26 4" is not a version of codeql`},
	} {
		t.Run(name, func(t *testing.T) {
			raw, device := stageRaw(t, "content")
			f := &importFake{device: device}

			_, err := importClient(t, f).ImportCompletedGeneration(t.Context(), "ubuntu-2404-x64",
				raw, "x86_64", "2.336.0", "", "7", &tc.record, importAt)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
			if len(f.calls) != 0 {
				t.Fatalf("a refused record still reached the cluster: %v", f.calls)
			}
		})
	}
}

// A RECORD THAT DOES NOT PARSE IS AN ERROR, never an absence, because an absence
// reads as an image that left nothing out.
func TestAComponentsRecordThatDoesNotParseIsAnError(t *testing.T) {
	for name, value := range map[string]string{
		"not json":      "left out: codeql",
		"unknown field": `{"left_out":["codeql"],"installed":{},"pinned":true}`,
		"invalid":       `{"left_out":[],"installed":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := componentsClient(t, func(context.Context, string, []string) ([]byte, error) {
				return []byte(value), nil
			})

			_, found, err := c.Components(t.Context(), "ubuntu-2404-x64", "g20260815041709")
			if err == nil || found {
				t.Fatalf("got found=%v err=%v, want an error", found, err)
			}
		})
	}
}

func TestAMissingComponentsRecordIsAnImageThatLeftNothingOut(t *testing.T) {
	c := componentsClient(t, func(context.Context, string, []string) ([]byte, error) {
		return nil, errors.New("rbd: failed to get metadata: (2) No such file or directory")
	})

	got, found, err := c.Components(t.Context(), "ubuntu-2404-x64", "g20260815041709")
	if err != nil || found || len(got.LeftOut) != 0 {
		t.Fatalf("got %+v found=%v err=%v, want no record", got, found, err)
	}
}

// A COMPLETION ROOT IS A SNAPSHOT THAT IS NOT A GENERATION, by its name, made
// atomically by rbd, carrying the kernel and contract a launch reads and its owner.
func TestACompletionRootIsImportedUnderItsOwnPrefix(t *testing.T) {
	raw, device := stageRaw(t, "content")
	f := &importFake{device: device}

	snap, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0",
		"vmlinux-6.1.155-ea1d42638d13", "7", "epyc-1", importAt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if snap != "c20260815041709" || !IsCompletionRoot(snap) {
		t.Fatalf("the completion root is %q", snap)
	}
	if _, isGeneration := ParseGeneration(snap); isGeneration {
		t.Fatalf("the completion root %s parses as a generation", snap)
	}
	for key, value := range map[string]string{
		KernelKey: "vmlinux-6.1.155-ea1d42638d13", GuestContractKey: "7", CompletionRootOwnerKey: "epyc-1",
	} {
		if !f.ranWith("image-meta", "set", key+"."+snap, value) {
			t.Errorf("%s was not recorded for the completion root; billet ran %v", key, f.calls)
		}
	}

	if _, err := importClient(t, &importFake{device: device}).ImportCompletionRoot(t.Context(),
		"ubuntu-2404-x64", raw, "x86_64", "2.336.0", "", "7", " ", importAt); err == nil {
		t.Fatal("a completion root with no owner was imported")
	}
}

// NO TIER BOOTS A COMPLETION ROOT, named exactly or not; only the completion's own
// resolver reaches one, and only one speaking the contract.
func TestOnlyTheCompletionResolverReachesACompletionRoot(t *testing.T) {
	c := componentsClient(t, func(_ context.Context, _ string, args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), GuestContractKey+".c20260815041709") {
			return []byte("7\n"), nil
		}

		return nil, errors.New("rbd: (2) No such file or directory")
	})

	if _, err := c.ResolveGeneration(t.Context(), "ubuntu-2404-x64@c20260815041709", "7"); err == nil ||
		!strings.Contains(err.Error(), "is a completion root") {
		t.Fatalf("ResolveGeneration = %v, want the completion root refused", err)
	}

	got, err := c.ResolveCompletionRoot(t.Context(), "ubuntu-2404-x64@c20260815041709", "7")
	if err != nil || got != "ubuntu-2404-x64@c20260815041709" {
		t.Fatalf("ResolveCompletionRoot = %q, %v", got, err)
	}
	for ref, contract := range map[string]string{
		"ubuntu-2404-x64@g20260815041709": "7",
		"ubuntu-2404-x64@c20260815041709": "8",
		"ubuntu-2404-x64":                 "7",
	} {
		if _, err := c.ResolveCompletionRoot(t.Context(), ref, contract); err == nil {
			t.Errorf("ResolveCompletionRoot(%s, %s) was accepted", ref, contract)
		}
	}
}

// A NODE REMOVES ONLY THE COMPLETION ROOTS IT MADE: another node's may be booting
// that node's completion, and one whose owner cannot be read is not this node's.
func TestRemoveCompletionRootsRemovesOnlyTheOwnersRoots(t *testing.T) {
	var removed, cleared []string
	c := componentsClient(t, func(_ context.Context, _ string, args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "snap ls"):
			return []byte(`[{"name":"g20260815041709"},{"name":"c20260815041710"},` +
				`{"name":"c20260815041711"},{"name":"c20260815041712"}]`), nil
		case strings.Contains(joined, "image-meta list"):
			// c...10 is epyc-1's, c...11 epyc-2's, c...12 has no owner, and c...13 is
			// an owner key epyc-1 recorded for a snapshot never made.
			return []byte("There are 4 metadata on this image:\n\nKey  Value\n" +
				CompletionRootOwnerKey + ".c20260815041710  epyc-1\n" +
				CompletionRootOwnerKey + ".c20260815041711  epyc-2\n" +
				CompletionRootOwnerKey + ".c20260815041713  epyc-1\n" +
				KernelKey + ".c20260815041710  vmlinux-6.1.155-ea1d42638d13\n"), nil
		case strings.Contains(joined, "snap rm"):
			removed = append(removed, args[len(args)-1])
		case strings.Contains(joined, "image-meta remove") && strings.Contains(joined, CompletionRootOwnerKey):
			cleared = append(cleared, args[len(args)-1])
		}

		return nil, nil
	})

	if err := c.RemoveCompletionRoots(t.Context(), "ubuntu-2404-x64", "epyc-1"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ubuntu-2404-x64@c20260815041710"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want only epyc-1's root", removed)
	}
	// THE KEY FOR A SNAPSHOT NOT LISTED IS LEFT: it may be an import of this owner's
	// between its owner write and its snapshot.
	if want := []string{CompletionRootOwnerKey + ".c20260815041710"}; !slices.Equal(cleared, want) {
		t.Fatalf("cleared owner keys %v, want only epyc-1's listed root's", cleared)
	}
}

// A COMPLETION ROOT'S OWNER IS RECORDED BEFORE ITS SNAPSHOT EXISTS, so a crash
// between the two leaves a key the owner clears, never a root nobody reclaims; a
// snapshot that could not be made takes its owner key away again.
func TestACompletionRootsOwnerIsRecordedBeforeItsSnapshot(t *testing.T) {
	raw, device := stageRaw(t, "content")
	f := &importFake{device: device}

	snap, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0", "",
		"7", "epyc-1", importAt)
	if err != nil {
		t.Fatal(err)
	}

	owner, created := -1, -1
	for i, call := range f.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "image-meta set") && strings.Contains(joined, CompletionRootOwnerKey+"."+snap) {
			owner = i
		}
		if strings.Contains(joined, "snap create") {
			created = i
		}
	}
	if owner < 0 || created < 0 || owner > created {
		t.Fatalf("the owner was recorded at call %d and the snapshot made at %d; billet ran %v", owner, created, f.calls)
	}

	for name, tc := range map[string]struct {
		fake      importFake
		ownerGone bool
		withdrawn bool
	}{
		"refused before it applied":     {importFake{failSnapCreate: true}, true, false},
		"applied, then its answer lost": {importFake{failSnapCreateApplied: true}, true, true},
		"applied, and nothing can say so": {importFake{failSnapCreateApplied: true,
			failSnapListAfterCreate: true}, false, false},
		// ENOENT FROM THE LISTING OF AN IMAGE THAT EXISTS is not an absent snapshot.
		"applied, and its listing answers ENOENT": {importFake{infoJSON: `{"size": 8388608}`,
			failSnapCreateApplied: true, enoentListAfterCreate: true}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := tc.fake
			f.device = device

			if _, err := importClient(t, &f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0",
				"", "7", "epyc-1", importAt); err == nil {
				t.Fatal("a completion root whose create failed was reported as made")
			}
			if got := f.ranWith("image-meta", "remove", CompletionRootOwnerKey+"."+snap); got != tc.ownerGone {
				t.Fatalf("the owner was removed: %v, want %v; billet ran %v", got, tc.ownerGone, f.calls)
			}
			if got := f.ranWith("snap", "rm", "ubuntu-2404-x64@"+snap); got != tc.withdrawn {
				t.Fatalf("the snapshot was withdrawn: %v, want %v; billet ran %v", got, tc.withdrawn, f.calls)
			}
		})
	}
}

// A COMPLETION-ROOT NAME ALREADY TAKEN IS REFUSED BEFORE ANYTHING IS WRITTEN, so a
// second root made in the same second cannot overwrite the first's owner and then
// take it away.
func TestACompletionRootNameAlreadyTakenIsRefused(t *testing.T) {
	const root = "c20260815041709"

	for name, f := range map[string]*importFake{
		"by a snapshot":   {snapshots: []string{root}},
		"by an owner key": {metadata: "Key  Value\n" + CompletionRootOwnerKey + "." + root + "  epyc-2\n"},
	} {
		t.Run(name, func(t *testing.T) {
			raw, device := stageRaw(t, "content")
			f.device = device

			_, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64", "2.336.0",
				"", "7", "epyc-1", importAt)
			if err == nil || !strings.Contains(err.Error(), "is already taken") {
				t.Fatalf("got %v, want the taken name refused", err)
			}
			if f.ranWith("image-meta", "set", CompletionRootOwnerKey+"."+root) || f.ranWith("snap", "create") {
				t.Fatalf("a taken name was written to; billet ran %v", f.calls)
			}
			// REFUSED BEFORE THE HEAD IS TOUCHED: ensureHead begins with `info` on the
			// image, and the write with `device map`.
			if f.ranWith("info", "ubuntu-2404-x64") || f.ranWith("device", "map") {
				t.Fatalf("a taken name still reached the head; billet ran %v", f.calls)
			}
		})
	}
}

// AN OWNER KEY WHOSE SNAPSHOT IS GONE IS CLEARED BY THE NEXT RESERVATION, whoever
// owned it, because under the publish lock no import is between its key and its
// snapshot; a key whose snapshot exists is kept.
func TestAReservationClearsOwnerKeysWhoseSnapshotIsGone(t *testing.T) {
	const live, mine, theirs = "c20260801000000", "c20260802000000", "c20260803000000"

	raw, device := stageRaw(t, "content")
	f := &importFake{
		device:    device,
		snapshots: []string{live},
		metadata: "Key  Value\n" +
			CompletionRootOwnerKey + "." + live + "  epyc-2\n" +
			CompletionRootOwnerKey + "." + mine + "  epyc-1\n" +
			CompletionRootOwnerKey + "." + theirs + "  epyc-3\n",
	}

	if _, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "vmlinux-6.1.155-ea1d42638d13", "7", "epyc-1", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	for _, key := range []string{KernelKey, RunnerVersionKey, GuestContractKey, CompletionRootOwnerKey} {
		for _, gone := range []string{mine, theirs} {
			if !f.ranWith("image-meta", "remove", "ubuntu-2404-x64", key+"."+gone) {
				t.Errorf("%s of %s, whose snapshot is gone, was kept; billet ran %v", key, gone, f.calls)
			}
		}
		if f.ranWith("image-meta", "remove", "ubuntu-2404-x64", key+"."+live) {
			t.Errorf("%s of %s, whose snapshot exists, was removed", key, live)
		}
	}
}

// A LISTING THAT ANSWERS ENOENT FOR AN IMAGE THAT EXISTS PROVES NO SNAPSHOT GONE:
// rbd answers so when a snapshot is removed while it lists, and a reservation that
// read it as none would clear the metadata of every root still there.
func TestAReservationOnAListingThatAnswersENOENTClearsNothing(t *testing.T) {
	const live = "c20260801000000"

	raw, device := stageRaw(t, "content")
	f := &importFake{
		device:         device,
		infoJSON:       `{"size": 8388608}`,
		snapshots:      []string{live},
		snapListENOENT: true,
		metadata:       "Key  Value\n" + CompletionRootOwnerKey + "." + live + "  epyc-1\n",
	}

	_, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "vmlinux-6.1.155-ea1d42638d13", "7", "epyc-1", importAt)
	if err == nil || !strings.Contains(err.Error(), "list the snapshots") {
		t.Fatalf("got %v, want the import refused on a listing that could not be read", err)
	}
	if f.ranWith("image-meta", "remove", "ubuntu-2404-x64") || f.ranWith("snap", "create") {
		t.Fatalf("an unreadable listing was acted on; billet ran %v", f.calls)
	}
}

// GENERATIONS READS A LISTING'S ENOENT THE SAME WAY: none for an absent image, and
// an error for one that exists, never an empty answer a caller would act on.
func TestGenerationsOnAListingThatAnswersENOENT(t *testing.T) {
	present := &importFake{infoJSON: `{"size": 8388608}`, snapListENOENT: true}
	if gens, err := importClient(t, present).Generations(t.Context(), "ubuntu-2404-x64"); err == nil {
		t.Fatalf("got %v and no error, want a listing that could not be read reported", gens)
	}

	absent := &importFake{snapListENOENT: true}
	gens, err := importClient(t, absent).Generations(t.Context(), "ubuntu-2404-x64")
	if err != nil || len(gens) != 0 {
		t.Fatalf("got %v, %v, want an absent image to have no generations", gens, err)
	}
}

// A LISTING'S ENOENT IS ABSENCE ONLY WHEN `info` SAYS SO: an image that is not there
// yet is a first import and proceeds, and an `info` that fails otherwise cannot
// tell, so the import is refused with nothing cleared.
func TestAListingThatAnswersENOENTIsAbsenceOnlyWhenTheImageIs(t *testing.T) {
	const orphan = "c20260801000000"

	for name, tc := range map[string]struct {
		failOn    string
		published bool
	}{
		"the image is absent":              {"", true},
		"whether it exists cannot be read": {"info", false},
	} {
		t.Run(name, func(t *testing.T) {
			raw, device := stageRaw(t, "content")
			f := &importFake{
				device:         device,
				failOn:         tc.failOn,
				snapListENOENT: true,
				metadata:       "Key  Value\n" + CompletionRootOwnerKey + "." + orphan + "  epyc-1\n",
			}

			_, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
				"2.336.0", "vmlinux-6.1.155-ea1d42638d13", "7", "epyc-1", importAt)
			if tc.published {
				if err != nil {
					t.Fatalf("a first import on an absent image was refused: %v", err)
				}
				if !f.ranWith("snap", "create") {
					t.Fatalf("a first import made no snapshot; billet ran %v", f.calls)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), "whether the image exists could not be read") {
				t.Fatalf("got %v, want the import refused on an image that cannot be read", err)
			}
			if f.ranWith("image-meta", "remove", "ubuntu-2404-x64") || f.ranWith("snap", "create") {
				t.Fatalf("an unreadable image was acted on; billet ran %v", f.calls)
			}
		})
	}
}

// A COMPLETION ROOT THAT COULD NOT BE WITHDRAWN KEEPS ITS METADATA, owner included:
// its absence is confirmed on every snapshot, not on the generations, which leave
// a completion root out and would have read it as gone.
func TestACompletionRootThatCouldNotBeWithdrawnKeepsItsOwner(t *testing.T) {
	const root = "c20260815041709"

	raw, device := stageRaw(t, "content")
	f := &importFake{
		device:         device,
		failMetaSetKey: GuestContractKey + "." + root,
		failSnapRemove: true,
	}

	_, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "x86_64",
		"2.336.0", "vmlinux-6.1.155-ea1d42638d13", "7", "epyc-1", importAt)
	if err == nil || !strings.Contains(err.Error(), "could not be withdrawn") {
		t.Fatalf("got %v, want the root reported as still there", err)
	}
	for _, key := range []string{KernelKey, RunnerVersionKey, GuestContractKey, CompletionRootOwnerKey} {
		if f.ranWith("image-meta", "remove", key+"."+root) {
			t.Errorf("rollback removed %s from a completion root that still exists", key)
		}
	}
}

// A COMPLETION ROOT'S KERNEL IS KEPT WHILE THE ROOT EXISTS, though the root is not a
// generation and the caller's list leaves it out.
func TestACompletionRootsKernelIsNeededWhileItExists(t *testing.T) {
	c := componentsClient(t, func(_ context.Context, _ string, args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "snap ls"):
			return []byte(`[{"name":"g20260815041709"},{"name":"c20260815041710"}]`), nil
		case strings.Contains(joined, KernelKey+".g20260815041709"):
			return []byte("vmlinux-old\n"), nil
		case strings.Contains(joined, KernelKey+".c20260815041710"):
			return []byte("vmlinux-new\n"), nil
		}

		return nil, errors.New("rbd: (2) No such file or directory")
	})

	needed, _, err := c.NeededKernels(t.Context(), "ubuntu-2404-x64",
		[]Generation{{Name: "g20260815041709"}})
	if err != nil {
		t.Fatal(err)
	}
	if !needed["vmlinux-old"] || !needed["vmlinux-new"] {
		t.Fatalf("needed %v, want the generation's kernel and the completion root's", needed)
	}
}

func componentsClient(t *testing.T, run runner) *Client {
	t.Helper()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}
