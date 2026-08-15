package imagesource

import (
	"os"
	"testing"
	"time"
)

// THE MANIFEST IS WRITTEN BY SHELL AND READ BY GO, and nothing else makes the two
// agree.
//
// scripts/write-image-manifest.sh builds the document with jq, from facts the
// build recorded; ParseManifest reads it under rules strict enough to refuse a
// duplicate key or a name that is not a plain file. Those are two independent
// implementations of one format, in two languages, with no shared schema and no
// compiler between them -- so the only thing that can catch a divergence is a real
// document produced by the real pipeline.
//
// THIS FIXTURE IS EXACTLY THAT. It is the manifest from the first green run of
// the guest-image workflow, committed byte-for-byte. If the writer starts emitting
// a field the reader refuses -- or stops emitting one it requires -- this fails,
// which is the only place that failure is cheap. In production it would surface as
// every node in every deployment declining to import anything, with a message
// blaming the publisher.
func TestTheRealPublishedManifestParses(t *testing.T) {
	data, err := os.ReadFile("testdata/published-manifest.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("the manifest produced by scripts/write-image-manifest.sh was refused by the "+
			"reader that has to consume it: %v", err)
	}

	// THE FACTS A NODE ACTS ON, asserted individually so a failure names which one
	// moved rather than reporting that something, somewhere, differs.
	if m.Schema != SchemaVersion {
		t.Errorf("schema = %d, want %d", m.Schema, SchemaVersion)
	}

	if m.Arch != "x86_64" {
		t.Errorf("arch = %q; the writer records `uname -m`, and go's spelling is not that",
			m.Arch)
	}

	if m.GuestContract != "2" {
		t.Errorf("guest_contract = %q, want 2", m.GuestContract)
	}

	if m.Rootfs.Compression != "zstd" {
		t.Errorf("rootfs compression = %q; the workflow packs with zstd and a reader that "+
			"sees %q would hand compressed bytes to the cluster as a filesystem",
			m.Rootfs.Compression, m.Rootfs.Compression)
	}

	if m.Kernel.Version == "" {
		t.Error("the kernel names no version; the writer reads it out of the built binary, " +
			"so an empty one means that extraction stopped working")
	}

	// AND IT IS USABLE BY THE BUILD THAT SHIPS WITH IT, which is the whole question
	// a node asks before spending four hundred megabytes.
	if err := m.Usable("2", "x86_64"); err != nil {
		t.Errorf("the published image is not usable by this build: %v", err)
	}
}

// THE FIXTURE WILL AGE, and that is expected rather than a defect: it records a
// real build from a real moment. This asserts the staleness check reads it as
// stale eventually and fresh at the time it was made, so the fixture going out of
// date cannot quietly turn the check into a no-op.
func TestTheRealManifestExercisesTheStalenessCheckBothWays(t *testing.T) {
	data, err := os.ReadFile("testdata/published-manifest.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if err := m.Stale(m.BuiltAt.Add(time.Hour)); err != nil {
		t.Errorf("an hour after it was built, the image read as stale: %v", err)
	}

	if err := m.Stale(m.BuiltAt.Add(45 * 24 * time.Hour)); err == nil {
		t.Error("forty-five days after it was built, the image did not read as stale")
	}
}
