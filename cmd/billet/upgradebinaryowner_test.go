package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE INSTALLED BINARY IS ROOT'S, WHATEVER THE ARCHIVE SAID. A release archive
// records every entry as its builder's account (uid 1001 on the hosted runner
// that builds billet), and tar run as root restores recorded owners by default,
// so the staged candidate came out 1001's and the copy that installs it kept its
// source's owner. MEASURED on a fleet (2026-09-22): after an automatic rollout
// both hosts' /usr/bin/billet were 1001:1001 0755, a root-executed binary any
// account given that uid could replace, and the converge guard refused the host
// ("/usr/bin/billet is owned by uid 1001, want 0").
func TestTheInstalledBinaryIsRootsWhateverTheStagedFileSays(t *testing.T) {
	calls := recordChowns(t)

	dir := t.TempDir()
	staged := filepath.Join(dir, "staged", "billet")
	installed := filepath.Join(dir, "usr-bin", "billet")

	for _, d := range []string{filepath.Dir(staged), filepath.Dir(installed)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(staged, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ownedByTheBuilder(t, staged)

	previous := installedBinary
	installedBinary = installed
	t.Cleanup(func() { installedBinary = previous })

	host := &systemdHost{staged: staged}
	if err := host.InstallCandidate(t.Context()); err != nil {
		t.Fatalf("InstallCandidate: %v", err)
	}

	want := chownCall{path: installed + ".billet-upgrade", uid: 0, gid: 0}
	if !slices.Contains(*calls, want) {
		t.Fatalf("the installed binary was handed %+v; want its staging file given root:root", *calls)
	}
}

// AND A ROLLBACK PUTS THE PREVIOUS BINARY BACK AS ROOT'S TOO: the preserved copy
// of a host already hit is itself 1001's, while a preserved configuration keeps
// its own owner, root:<service group>, which the unprivileged server needs.
func TestARestoredBinaryIsRootsAndARestoredConfigKeepsItsOwner(t *testing.T) {
	calls := recordChowns(t)

	dir := t.TempDir()
	installed := filepath.Join(dir, "usr-bin", "billet")

	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}

	previous := installedBinary
	installedBinary = installed
	t.Cleanup(func() { installedBinary = previous })

	recovery := t.TempDir()
	preserved := filepath.Join(recovery, "preserved")

	if err := os.MkdirAll(preserved, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(preserved, "billet"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ownedByTheBuilder(t, filepath.Join(preserved, "billet"))

	host := &systemdHost{}
	if err := host.RestorePreserved(t.Context(), recovery); err != nil {
		t.Fatalf("RestorePreserved: %v", err)
	}

	want := chownCall{path: installed + ".billet-upgrade", uid: 0, gid: 0}
	if !slices.Contains(*calls, want) {
		t.Fatalf("the restored binary was handed %+v; want its staging file given root:root", *calls)
	}

	// THE CONFIGURATION'S RULE IS UNCHANGED: a copy keeps its source's owner.
	config := filepath.Join(dir, "billet.yaml")
	from := filepath.Join(dir, "billet.yaml.preserved")

	if err := os.WriteFile(from, []byte("server: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	*calls = nil

	if err := copyFile(from, config); err != nil {
		t.Fatal(err)
	}

	uid, gid := ownerOf(t, from)
	if len(*calls) != 1 || (*calls)[0].uid != uid || (*calls)[0].gid != gid {
		t.Fatalf("a configuration copy handed out %+v; want its source's owner %d:%d", *calls, uid, gid)
	}
}

// ownedByTheBuilder makes a fixture read as the release builder's (uid 1001)
// when the test runs as root, where an inherited owner of 0 would let a copy
// that kept its source's owner pass; unprivileged, the test's own uid is already
// not root's and the assertion is meaningful as it is.
func ownedByTheBuilder(t *testing.T, path string) {
	t.Helper()

	if os.Getuid() != 0 {
		return
	}

	if err := os.Chown(path, 1001, 1001); err != nil {
		t.Fatal(err)
	}
}

// AND stageCandidate UNPACKS ONLY THROUGH candidateUnpack: no other tar is run
// in the transaction's file, so the flag cannot be bypassed by an inline command.
func TestTheTransactionUnpacksOnlyThroughCandidateUnpack(t *testing.T) {
	raw, err := os.ReadFile("hostupgrade.go")
	if err != nil {
		t.Fatal(err)
	}

	source := string(raw)

	if strings.Count(source, `"tar"`) != 1 {
		t.Errorf("hostupgrade.go names tar %d times; want once, in candidateUnpack", strings.Count(source, `"tar"`))
	}

	start := strings.Index(source, "func stageCandidate(")
	if start < 0 {
		t.Fatal("hostupgrade.go has no stageCandidate")
	}

	end := strings.Index(source[start:], "\n}\n")
	if end < 0 || !strings.Contains(source[start:start+end], "candidateUnpack(ctx, archive, dir)") {
		t.Error("stageCandidate does not unpack through candidateUnpack")
	}
}

// THE CANDIDATE IS UNPACKED WITHOUT THE ARCHIVE'S OWNERS, so even the staged file
// is never the builder's. --no-same-owner is GNU tar's and bsdtar's spelling.
func TestTheCandidateIsUnpackedWithoutTheArchivesOwners(t *testing.T) {
	cmd := candidateUnpack(t.Context(), "/tmp/archive.tar.gz", "/tmp/dir")

	if !slices.Contains(cmd.Args, "--no-same-owner") {
		t.Fatalf("the candidate is unpacked with %q, which keeps the archive's recorded owners when run as root", cmd.Args)
	}

	if want := []string{"-xzf", "/tmp/archive.tar.gz", "-C", "/tmp/dir", "billet"}; !slices.Equal(cmd.Args[len(cmd.Args)-len(want):], want) {
		t.Fatalf("the unpack is %q, want it to end %q", cmd.Args, want)
	}
}
