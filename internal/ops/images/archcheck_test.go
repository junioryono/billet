package images

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// fakeArchChecker answers for the cluster with generations and what each records.
type fakeArchChecker struct {
	generations []string
	arches      map[string]string
	err         error
}

func (f fakeArchChecker) Generations(context.Context, string) ([]ceph.Generation, error) {
	out := make([]ceph.Generation, 0, len(f.generations))
	for _, name := range f.generations {
		gen, _ := ceph.ParseGeneration(name)
		out = append(out, gen)
	}

	return out, nil
}

func (f fakeArchChecker) GenerationArches(context.Context, string) (map[string]string, error) {
	return f.arches, f.err
}

// THE CHECK NAMES EACH GENERATION THAT RECORDS NO ARCHITECTURE AND EACH OF ANOTHER
// MACHINE'S, before an import meets one, and says nothing of an image whose every
// generation is this host's.
func TestCheckNamesGenerationsOfNoOrAnotherArchitecture(t *testing.T) {
	t.Parallel()

	store := fakeArchChecker{
		generations: []string{"g20260801000000", "g20260802000000", "g20260803000000"},
		arches:      map[string]string{"g20260801000000": "x86_64", "g20260803000000": "aarch64"},
	}

	lines := strings.Join(archLines(t.Context(), store, "ubuntu-2404-x64", "x86_64"), "\n")

	for _, want := range []string{
		"record no architecture (g20260802000000)",
		"built for another machine than this x86_64 host (g20260803000000 (aarch64))",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("the check did not say %q; it said:\n%s", want, lines)
		}
	}

	if strings.Contains(lines, "g20260801000000") {
		t.Errorf("the check named a generation of this host's machine:\n%s", lines)
	}

	clean := fakeArchChecker{generations: []string{"g20260801000000"},
		arches: map[string]string{"g20260801000000": "x86_64"}}
	if got := archLines(t.Context(), clean, "ubuntu-2404-x64", "x86_64"); len(got) != 0 {
		t.Errorf("an image of this host's machine alone was reported: %v", got)
	}

	failing := fakeArchChecker{generations: []string{"g20260801000000"}, err: errors.New("rbd timed out")}
	if got := strings.Join(archLines(t.Context(), failing, "ubuntu-2404-x64", "x86_64"), "\n"); !strings.Contains(got, "could not tell") {
		t.Errorf("a record that could not be read was not said: %q", got)
	}
}

// AND IT RUNS FOR EACH IMAGE A TIER ON THIS NODE BOOTS, once per image however many
// references name it.
func TestReportImageArchitecturesReadsEachTiersImage(t *testing.T) {
	restore := openArchChecker
	t.Cleanup(func() { openArchChecker = restore })
	openArchChecker = func(*config.Config) (archChecker, error) {
		return fakeArchChecker{generations: []string{"g20260802000000"}}, nil
	}

	pinned := "  - label: pinned\n    provider: firecracker\n    vcpu: 2\n    memory: 4GiB\n" +
		"    image: ubuntu-2404-x64@g20260802000000\n"

	cfg, err := config.Load(componentsCheckConfig(t, "", pinned))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var out bytes.Buffer
	ReportImageArchitectures(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, cfg)

	if got := strings.Count(out.String(), "images   ubuntu-2404-x64 holds generations that record no "+
		"architecture (g20260802000000)"); got != 1 {
		t.Errorf("the check said it %d times; it said:\n%s", got, out.String())
	}
}
