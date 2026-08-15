package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func kernelDirWith(t *testing.T, names ...string) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}

	return dir
}

func present(t *testing.T, dir, name string) bool {
	t.Helper()

	_, err := os.Stat(filepath.Join(dir, name))

	return err == nil
}

// THE FILE A GENERATION STILL NEEDS SURVIVES, and the one nothing names does not.
func TestReapKernelDirRemovesOnlyOrphans(t *testing.T) {
	dir := kernelDirWith(t,
		"vmlinux-6.1.155-ea1d42638d13",
		"vmlinux-6.1.140-bbbbbbbbbbbb",
	)

	removed, err := reapKernelDir(dir,
		map[string]bool{"vmlinux-6.1.155-ea1d42638d13": true}, 2, 0, "", false)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}

	if len(removed) != 1 || removed[0] != "vmlinux-6.1.140-bbbbbbbbbbbb" {
		t.Fatalf("removed %v", removed)
	}

	if !present(t, dir, "vmlinux-6.1.155-ea1d42638d13") {
		t.Error("a kernel a generation still needs was deleted; every microVM booting " +
			"that generation would fail to start")
	}

	if present(t, dir, "vmlinux-6.1.140-bbbbbbbbbbbb") {
		t.Error("the orphan was reported removed but is still there")
	}
}

// A DRY RUN REPORTS AND REMOVES NOTHING. This is what an operator runs first, and
// a dry run that deletes is worse than no dry run at all.
func TestReapKernelDirDryRunDeletesNothing(t *testing.T) {
	dir := kernelDirWith(t, "vmlinux-6.1.140-bbbbbbbbbbbb")

	removed, err := reapKernelDir(dir, map[string]bool{}, 0, 0, "", true)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}

	if len(removed) != 1 {
		t.Fatalf("a dry run reported %v, want the one orphan", removed)
	}

	if !present(t, dir, "vmlinux-6.1.140-bbbbbbbbbbbb") {
		t.Fatal("a DRY RUN deleted the file")
	}
}

// THE REFUSAL FROM THE PLANNER REACHES THE CALLER, and nothing is deleted. An
// empty needed-set while generations exist is metadata that could not be read, and
// acting on it removes the kernel every running tier boots.
func TestReapKernelDirRefusesWhenNothingIsNeededButGenerationsExist(t *testing.T) {
	dir := kernelDirWith(t, "vmlinux-6.1.155-ea1d42638d13")

	_, err := reapKernelDir(dir, map[string]bool{}, 3, 3, "", false)
	if err == nil {
		t.Fatal("reaped every kernel while three generations exist")
	}

	if !present(t, dir, "vmlinux-6.1.155-ea1d42638d13") {
		t.Fatal("the refusal still deleted the file")
	}
}

// A DIRECTORY THAT IS NOT THERE IS NOT AN ERROR. A node that has never pulled has
// no kernel directory, and a reap that fails on that would fail on every fresh
// deployment.
func TestReapKernelDirToleratesAMissingDirectory(t *testing.T) {
	removed, err := reapKernelDir(filepath.Join(t.TempDir(), "never-created"),
		map[string]bool{}, 0, 0, "", false)
	if err != nil {
		t.Fatalf("a node that has never pulled reported an error: %v", err)
	}

	if len(removed) != 0 {
		t.Errorf("removed %v from a directory that does not exist", removed)
	}
}

// FILES THIS DID NOT WRITE ARE LEFT ALONE, including subdirectories -- the kernel
// directory is a real path an operator can put things in.
func TestReapKernelDirLeavesForeignFilesAlone(t *testing.T) {
	dir := kernelDirWith(t, "vmlinux-6.1.140-bbbbbbbbbbbb", "NOTES.md", "vmlinux-custom")

	if err := os.Mkdir(filepath.Join(dir, "backup"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := reapKernelDir(dir, map[string]bool{}, 0, 0, "", false); err != nil {
		t.Fatalf("reap: %v", err)
	}

	for _, name := range []string{"NOTES.md", "vmlinux-custom", "backup"} {
		if !present(t, dir, name) {
			t.Errorf("%q was deleted; this did not write it", name)
		}
	}

	if present(t, dir, "vmlinux-6.1.140-bbbbbbbbbbbb") {
		t.Error("the orphan kernel survived")
	}
}

// THE REPORT NAMES WHAT WENT, because an operator reading a reap needs to be able
// to check it against what a tier boots.
func TestReapKernelDirReportsWhatItRemoved(t *testing.T) {
	dir := kernelDirWith(t, "vmlinux-6.1.140-bbbbbbbbbbbb", "vmlinux-6.1.130-cccccccccccc")

	removed, err := reapKernelDir(dir, map[string]bool{}, 0, 0, "", false)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}

	joined := strings.Join(removed, " ")

	for _, want := range []string{"vmlinux-6.1.140-bbbbbbbbbbbb", "vmlinux-6.1.130-cccccccccccc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not name %q", want)
		}
	}
}

// ONE ANSWER, SHARED BY THE REAPER AND BY VERIFICATION, because they must agree
// about what "managed" means. The reaper protects this file from deletion;
// verification records it as a generation's pairing. If they disagreed, one would
// record a name the other would happily delete.
func TestConfiguredKernelNameIdentifiesOnlyManagedKernels(t *testing.T) {
	managed := "/var/lib/billet/kernels"

	withKernel := func(path string) *config.Config {
		return &config.Config{Node: &config.NodeConfig{
			Firecracker: &config.FirecrackerConfig{KernelImage: path},
		}}
	}

	got := configuredKernelName(withKernel(managed+"/vmlinux-6.1.155-ea1d42638d13"), managed)
	if got != "vmlinux-6.1.155-ea1d42638d13" {
		t.Errorf("a kernel inside the managed directory resolved to %q", got)
	}

	// OUTSIDE THE DIRECTORY IS NOT THIS CODE'S BUSINESS, and answering with a bare
	// base name would be actively wrong: the reaper would protect a same-named file
	// it does manage, and verification would record a pairing every launch resolves
	// to the wrong path.
	for _, outside := range []string{
		"/home/junior/fc-smoke/vmlinux-billet",
		"/opt/other/vmlinux-6.1.155-ea1d42638d13",
		managed + "/nested/vmlinux-6.1.155-ea1d42638d13",
		"",
	} {
		if got := configuredKernelName(withKernel(outside), managed); got != "" {
			t.Errorf("%q resolved to %q; it is not in the managed directory", outside, got)
		}
	}

	// A NODE WITH NO FIRECRACKER SECTION AT ALL must not panic; a server-only
	// config reaches the reaper through the same command.
	if got := configuredKernelName(&config.Config{}, managed); got != "" {
		t.Errorf("a config with no node section resolved to %q", got)
	}

	if got := configuredKernelName(&config.Config{Node: &config.NodeConfig{}}, managed); got != "" {
		t.Errorf("a node with no firecracker section resolved to %q", got)
	}
}

// PATHS ARE COMPARED AFTER RESOLUTION, so a trailing slash or a relative segment
// in either the configured path or the directory still matches.
func TestConfiguredKernelNameResolvesBothSides(t *testing.T) {
	cfg := &config.Config{Node: &config.NodeConfig{
		Firecracker: &config.FirecrackerConfig{
			KernelImage: "/var/lib/billet/kernels/../kernels/vmlinux-6.1.155-ea1d42638d13",
		},
	}}

	if got := configuredKernelName(cfg, "/var/lib/billet/kernels/"); got != "vmlinux-6.1.155-ea1d42638d13" {
		t.Errorf("a path with a relative segment resolved to %q", got)
	}
}
