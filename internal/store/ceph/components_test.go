package ceph

import (
	"context"
	"errors"
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

// A COMPLETION ROOT IS THE ONLY THING THIS REMOVES, so a caller holding the wrong
// name cannot delete the image every tier boots.
func TestRemoveCompletionRootRemovesOnlyACompletionRoot(t *testing.T) {
	var calls [][]string
	c := componentsClient(t, func(_ context.Context, _ string, args []string) ([]byte, error) {
		calls = append(calls, args)

		return nil, nil
	})

	for _, name := range []string{"ubuntu-2404-x64", CompletionRootSuffix, "ubuntu-2404-x64.completion", ""} {
		if err := c.RemoveCompletionRoot(t.Context(), name); err == nil {
			t.Errorf("RemoveCompletionRoot(%q) was accepted", name)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused name still reached rbd: %v", calls)
	}

	if err := c.RemoveCompletionRoot(t.Context(), CompletionRoot("ubuntu-2404-x64")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-p billet-images snap purge ubuntu-2404-x64.completion-root",
		"-p billet-images rm ubuntu-2404-x64.completion-root",
	}
	if len(calls) != len(want) {
		t.Fatalf("rbd ran %v, want %v", calls, want)
	}
	for i := range want {
		if !strings.HasSuffix(strings.Join(calls[i], " "), want[i]) {
			t.Fatalf("rbd ran %v, want %v", calls, want)
		}
	}
}

func TestRemovingAnAbsentCompletionRootIsNotAnError(t *testing.T) {
	c := componentsClient(t, func(context.Context, string, []string) ([]byte, error) {
		return nil, errors.New("rbd: error opening image: (2) No such file or directory")
	})
	if err := c.RemoveCompletionRoot(t.Context(), CompletionRoot("ubuntu-2404-x64")); err != nil {
		t.Fatalf("an absent completion root was an error: %v", err)
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
