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

	gen, err := importClient(t, f).ImportCompletedGeneration(t.Context(), "ubuntu-2404-x64", raw,
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

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw,
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

	_, err := importClient(t, f).ImportCompletedGeneration(t.Context(), "ubuntu-2404-x64", raw,
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
				raw, "2.336.0", "", "7", &tc.record, importAt)
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

	snap, err := importClient(t, f).ImportCompletionRoot(t.Context(), "ubuntu-2404-x64", raw, "2.336.0",
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
		"ubuntu-2404-x64", raw, "2.336.0", "", "7", " ", importAt); err == nil {
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
	var removed []string
	c := componentsClient(t, func(_ context.Context, _ string, args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "snap ls"):
			return []byte(`[{"name":"g20260815041709"},{"name":"c20260815041710"},` +
				`{"name":"c20260815041711"},{"name":"c20260815041712"}]`), nil
		case strings.Contains(joined, CompletionRootOwnerKey+".c20260815041710"):
			return []byte("epyc-1\n"), nil
		case strings.Contains(joined, CompletionRootOwnerKey+".c20260815041711"):
			return []byte("epyc-2\n"), nil
		case strings.Contains(joined, "image-meta get"):
			return nil, errors.New("rbd: (2) No such file or directory")
		case strings.Contains(joined, "snap rm"):
			removed = append(removed, args[len(args)-1])
		}

		return nil, nil
	})

	if err := c.RemoveCompletionRoots(t.Context(), "ubuntu-2404-x64", "epyc-1"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ubuntu-2404-x64@c20260815041710"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want only epyc-1's root", removed)
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
