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
//
// AGAINST A REAL FILESYSTEM, because the answer depends on resolving symlinks and
// a path that does not exist cannot be resolved at all.
func TestConfiguredKernelNameIdentifiesOnlyManagedKernels(t *testing.T) {
	managed := t.TempDir()
	elsewhere := t.TempDir()

	kernel := filepath.Join(managed, "vmlinux-6.1.155-ea1d42638d13")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("stage: %v", err)
	}

	outside := filepath.Join(elsewhere, "vmlinux-6.1.155-ea1d42638d13")
	if err := os.WriteFile(outside, []byte("other"), 0o644); err != nil {
		t.Fatalf("stage: %v", err)
	}

	withKernel := func(path string) *config.Config {
		return &config.Config{Node: &config.NodeConfig{
			Firecracker: &config.FirecrackerConfig{KernelImage: path},
		}}
	}

	if got := configuredKernelName(withKernel(kernel), managed); got != "vmlinux-6.1.155-ea1d42638d13" {
		t.Errorf("a kernel inside the managed directory resolved to %q", got)
	}

	// OUTSIDE IS NOT THIS CODE'S BUSINESS, and answering with a bare base name would
	// be actively wrong: the reaper would protect a same-named file it does manage,
	// and verification would record a pairing every launch resolves elsewhere. Note
	// this file has the SAME NAME as the managed one, which is the case a base-name
	// comparison gets wrong.
	for _, other := range []string{outside, "", filepath.Join(managed, "missing")} {
		if got := configuredKernelName(withKernel(other), managed); got != "" {
			t.Errorf("%q resolved to %q; it is not a kernel in the managed directory", other, got)
		}
	}

	// A NODE WITH NO FIRECRACKER SECTION must not panic; a server-only config reaches
	// the reaper through the same command.
	if got := configuredKernelName(&config.Config{}, managed); got != "" {
		t.Errorf("a config with no node section resolved to %q", got)
	}

	if got := configuredKernelName(&config.Config{Node: &config.NodeConfig{}}, managed); got != "" {
		t.Errorf("a node with no firecracker section resolved to %q", got)
	}
}

// A SYMLINK IS RESOLVED TO WHAT IT POINTS AT, which is the whole reason this uses
// EvalSymlinks rather than Abs.
//
// A stable name like `current` pointing at one kernel could otherwise be recorded
// as a generation's pairing and then retargeted -- proving kernel A and booting
// kernel B, silently, which is precisely the mismatch the pairing exists to
// prevent.
func TestConfiguredKernelNameResolvesSymlinksToTheirTarget(t *testing.T) {
	managed := t.TempDir()

	target := filepath.Join(managed, "vmlinux-6.1.155-ea1d42638d13")
	if err := os.WriteFile(target, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("stage: %v", err)
	}

	link := filepath.Join(managed, "current")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cfg := &config.Config{Node: &config.NodeConfig{
		Firecracker: &config.FirecrackerConfig{KernelImage: link},
	}}

	got := configuredKernelName(cfg, managed)

	if got == "current" {
		t.Fatal("the symlink was recorded by its own name; retargeting it later would " +
			"prove one kernel and boot another")
	}

	if got != "vmlinux-6.1.155-ea1d42638d13" {
		t.Errorf("resolved to %q, want the file the link points at", got)
	}
}

// A TRAILING SLASH OR A RELATIVE SEGMENT IN EITHER PATH still matches.
func TestConfiguredKernelNameResolvesBothSides(t *testing.T) {
	managed := t.TempDir()

	kernel := filepath.Join(managed, "vmlinux-6.1.155-ea1d42638d13")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("stage: %v", err)
	}

	cfg := &config.Config{Node: &config.NodeConfig{
		Firecracker: &config.FirecrackerConfig{
			KernelImage: filepath.Join(managed, "..", filepath.Base(managed),
				"vmlinux-6.1.155-ea1d42638d13"),
		},
	}}

	if got := configuredKernelName(cfg, managed+"/"); got != "vmlinux-6.1.155-ea1d42638d13" {
		t.Errorf("a path with a relative segment resolved to %q", got)
	}
}
