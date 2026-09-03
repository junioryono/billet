package releasesource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dist stages a directory that looks like the one GoReleaser writes.
func dist(t *testing.T, names ...string) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range names {
		// Distinct bytes per file, so a builder that hashed the wrong one shows up
		// as a digest collision rather than passing.
		body := []byte("artifact " + name)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}

	return dir
}

func buildRequest(dir string) BuildRequest {
	return BuildRequest{
		Dist:          dir,
		Version:       "v0.4.0",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		BuiltAt:       time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		Wire:          Range{Min: 12, Max: 13},
		LedgerSchema:  35,
		GuestContract: "10",
		RollbackTo:    "v0.3.26",
	}
}

// WHAT THE PUBLISHER WRITES IS WHAT THE READER ACCEPTS.
//
// The point of building the manifest in Go rather than in jq is that the writer
// and the reader are the same type; this is the assertion that makes that worth
// anything. A release cannot ship a manifest its own fleet would refuse.
func TestBuildProducesAManifestTheReaderAccepts(t *testing.T) {
	t.Parallel()

	dir := dist(t,
		"billet_0.4.0_linux_amd64.tar.gz",
		"billet_0.4.0_linux_arm64.tar.gz",
		"billet_0.4.0_darwin_arm64.tar.gz",
		"billet_0.4.0_linux_amd64.deb",
		"billet_0.4.0_linux_amd64.rpm",
	)

	m, err := Build(buildRequest(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	body, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := ParseManifest(body); err != nil {
		t.Fatalf("the manifest this build produced is one billet refuses: %v", err)
	}

	if len(m.Artifacts) != 5 {
		t.Fatalf("Build found %d artifacts, want 5: %+v", len(m.Artifacts), m.Artifacts)
	}

	a, err := m.Select("darwin", "arm64", KindArchive)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	if a.Name != "billet_0.4.0_darwin_arm64.tar.gz" {
		t.Errorf("darwin/arm64 resolved to %q", a.Name)
	}
}

// FILES THAT ARE NOT ARTIFACTS ARE SKIPPED, NOT GUESSED AT.
//
// checksums.txt and GoReleaser's metadata live in the same directory, and a
// manifest that named them would have an updater try to install a list of hashes.
func TestBuildIgnoresWhatIsNotAnArtifact(t *testing.T) {
	t.Parallel()

	dir := dist(t,
		"billet_0.4.0_linux_amd64.tar.gz",
		"checksums.txt",
		"metadata.json",
		"config.yaml",
	)

	m, err := Build(buildRequest(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, a := range m.Artifacts {
		if !strings.HasPrefix(a.Name, "billet_") {
			t.Errorf("Build named %q as an installable artifact", a.Name)
		}
	}

	if len(m.Artifacts) != 1 {
		t.Errorf("Build found %d artifacts, want 1: %+v", len(m.Artifacts), m.Artifacts)
	}
}

// THE DIGESTS COME FROM THE FILES. Reading them out of checksums.txt would make
// the manifest a restatement of another document's opinion, and the signature
// over it would vouch for that restatement rather than for the bytes.
func TestBuildHashesTheBytesItPublishes(t *testing.T) {
	t.Parallel()

	dir := dist(t, "billet_0.4.0_linux_amd64.tar.gz")

	first, err := Build(buildRequest(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	path := filepath.Join(dir, "billet_0.4.0_linux_amd64.tar.gz")
	if err := os.WriteFile(path, []byte("different bytes entirely"), 0o600); err != nil {
		t.Fatalf("rewrite the artifact: %v", err)
	}

	second, err := Build(buildRequest(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if first.Artifacts[0].SHA256 == second.Artifacts[0].SHA256 {
		t.Error("Build reported the same digest for different bytes")
	}

	if second.Artifacts[0].Size != int64(len("different bytes entirely")) {
		t.Errorf("Build reported size %d", second.Artifacts[0].Size)
	}
}

// TWO BUILDS OF ONE TREE PRODUCE THE SAME ORDER. Directory order is not stable,
// and a manifest whose bytes move for no reason cannot be compared against a
// previous one by digest.
func TestBuildOrdersArtifactsDeterministically(t *testing.T) {
	t.Parallel()

	dir := dist(t,
		"billet_0.4.0_linux_arm64.tar.gz",
		"billet_0.4.0_darwin_arm64.tar.gz",
		"billet_0.4.0_linux_amd64.tar.gz",
	)

	m, err := Build(buildRequest(dir))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for i := 1; i < len(m.Artifacts); i++ {
		if m.Artifacts[i-1].Name > m.Artifacts[i].Name {
			t.Fatalf("artifacts are not ordered: %q before %q",
				m.Artifacts[i-1].Name, m.Artifacts[i].Name)
		}
	}
}

// A BUILD THAT PRODUCED NOTHING INSTALLABLE IS A FAILURE, not an empty manifest.
//
// A release publishing no artifacts passes every per-artifact check there is,
// because there are none — the same shape as a grep gate that matches nothing and
// reports success.
func TestBuildRefusesADistWithNoArtifacts(t *testing.T) {
	t.Parallel()

	if _, err := Build(buildRequest(dist(t, "checksums.txt"))); err == nil {
		t.Fatal("a release publishing nothing installable produced a manifest")
	}
}
