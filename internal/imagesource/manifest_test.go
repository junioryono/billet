package imagesource

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// validManifest is the shape everything else deviates from by one field, so a
// failing case names exactly one cause.
func validManifest() Manifest {
	return Manifest{
		Schema:        SchemaVersion,
		GuestContract: "2",
		Arch:          "x86_64",
		RunnerVersion: "2.336.0",
		BuiltAt:       time.Date(2026, 8, 14, 6, 18, 44, 0, time.UTC),
		Rootfs: Asset{
			Name:        "rootfs.img.zst",
			SHA256:      strings.Repeat("a", 64),
			Size:        397_000_000,
			Compression: "zstd",
		},
		Kernel: Asset{
			Name:    "vmlinux",
			SHA256:  strings.Repeat("b", 64),
			Size:    49_283_072,
			Version: "6.1.155",
		},
	}
}

func mustJSON(t *testing.T, m Manifest) []byte {
	t.Helper()

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return data
}

func TestParseManifestAcceptsAWellFormedDocument(t *testing.T) {
	m, err := ParseManifest(mustJSON(t, validManifest()))
	if err != nil {
		t.Fatalf("a valid manifest was refused: %v", err)
	}

	if m.RunnerVersion != "2.336.0" {
		t.Errorf("runner version = %q, want 2.336.0", m.RunnerVersion)
	}

	if m.Kernel.Version != "6.1.155" {
		t.Errorf("kernel version = %q, want 6.1.155", m.Kernel.Version)
	}
}

// A NAME IS THE FIELD THAT REACHES THE FILESYSTEM, so it gets the most cases.
// Each of these, accepted, would write or fetch outside the directory chosen by
// the caller.
func TestParseManifestRefusesNamesThatAreNotPlainFiles(t *testing.T) {
	for _, name := range []string{
		"../../etc/passwd",
		"sub/rootfs.img",
		"/etc/shadow",
		"..",
		".",
		".hidden",
		"",
		"rootfs.img\n",
		"rootfs.img\x00",
		"root fs.img",
		"-rf",
		strings.Repeat("a", 129),
	} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			m := validManifest()
			m.Rootfs.Name = name

			if _, err := ParseManifest(mustJSON(t, m)); err == nil {
				t.Fatalf("%q was accepted as an asset name; it would be joined to a base "+
					"url and to a staging directory", name)
			}
		})
	}
}

func TestParseManifestRefusesADigestThatIsNotOne(t *testing.T) {
	for _, digest := range []string{
		"",
		"not-a-digest",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.ToUpper(strings.Repeat("a", 64)), // uppercase: compared byte-wise elsewhere
		strings.Repeat("g", 64),
	} {
		t.Run(fmt.Sprintf("%q", digest), func(t *testing.T) {
			m := validManifest()
			m.Rootfs.SHA256 = digest

			if _, err := ParseManifest(mustJSON(t, m)); err == nil {
				t.Fatalf("%q was accepted as a digest, so nothing downloaded could be "+
					"proven to be what was published", digest)
			}
		})
	}
}

func TestParseManifestRefusesASchemaItDoesNotKnow(t *testing.T) {
	for _, schema := range []int{0, SchemaVersion + 1, -1} {
		m := validManifest()
		m.Schema = schema

		_, err := ParseManifest(mustJSON(t, m))
		if err == nil {
			t.Fatalf("schema %d was accepted", schema)
		}

		if !strings.Contains(err.Error(), "schema") {
			t.Errorf("schema %d refused without saying why: %v", schema, err)
		}
	}
}

// THE FIELD A NEWER PUBLISHER WOULD ADD. Dropping it silently is how a reader
// imports something it did not understand.
func TestParseManifestRefusesUnknownFields(t *testing.T) {
	data := mustJSON(t, validManifest())

	widened := strings.Replace(string(data), `{"schema"`, `{"encryption":"aes","schema"`, 1)
	if widened == string(data) {
		t.Fatal("could not add a field to the fixture")
	}

	if _, err := ParseManifest([]byte(widened)); err == nil {
		t.Fatal("a manifest carrying a field this build does not know was accepted")
	}
}

func TestParseManifestRefusesTrailingContent(t *testing.T) {
	data := mustJSON(t, validManifest())

	two := append(append([]byte{}, data...), data...)

	if _, err := ParseManifest(two); err == nil {
		t.Fatal("two concatenated manifests were accepted as one")
	}
}

func TestParseManifestRefusesAnOversizeDocument(t *testing.T) {
	if _, err := ParseManifest(make([]byte, MaxManifestBytes+1)); err == nil {
		t.Fatal("an oversize document was parsed")
	}
}

func TestParseManifestRefusesEmpty(t *testing.T) {
	if _, err := ParseManifest(nil); err == nil {
		t.Fatal("an empty document was accepted")
	}
}

func TestParseManifestRefusesSizesItCannotActOn(t *testing.T) {
	for _, size := range []int64{0, -1, MaxAssetBytes + 1} {
		m := validManifest()
		m.Rootfs.Size = size

		if _, err := ParseManifest(mustJSON(t, m)); err == nil {
			t.Fatalf("a published size of %d was accepted", size)
		}
	}
}

// AN UNKNOWN COMPRESSION MUST NOT DEGRADE TO "NONE", because the difference is
// whether the bytes are run through a decompressor.
func TestParseManifestRefusesCompressionItCannotUnpack(t *testing.T) {
	m := validManifest()
	m.Rootfs.Compression = "lz4"

	_, err := ParseManifest(mustJSON(t, m))
	if err == nil {
		t.Fatal("an unknown compression was accepted, and would have been treated as raw bytes")
	}

	if !strings.Contains(err.Error(), "lz4") {
		t.Errorf("the refusal does not name the compression: %v", err)
	}
}

func TestParseManifestAcceptsNoCompression(t *testing.T) {
	m := validManifest()
	m.Rootfs.Compression = ""

	if _, err := ParseManifest(mustJSON(t, m)); err != nil {
		t.Fatalf("an uncompressed asset was refused: %v", err)
	}
}

// STAGED SIDE BY SIDE, so one name for both would have the second overwrite the
// first — and the digest check would then pass against whichever landed last.
func TestParseManifestRefusesTwoAssetsWithOneName(t *testing.T) {
	m := validManifest()
	m.Kernel.Name = m.Rootfs.Name

	_, err := ParseManifest(mustJSON(t, m))
	if err == nil {
		t.Fatal("the rootfs and the kernel were published under one name and it was accepted")
	}

	if !strings.Contains(err.Error(), m.Rootfs.Name) {
		t.Errorf("the refusal does not name the collision: %v", err)
	}
}

func TestParseManifestRefusesMissingIdentity(t *testing.T) {
	for _, tc := range []struct {
		field  string
		mutate func(*Manifest)
	}{
		{"guest contract", func(m *Manifest) { m.GuestContract = "  " }},
		{"runner version", func(m *Manifest) { m.RunnerVersion = "" }},
		{"arch", func(m *Manifest) { m.Arch = "" }},
		{"arch", func(m *Manifest) { m.Arch = "x86 64" }},
		{"built at", func(m *Manifest) { m.BuiltAt = time.Time{} }},
	} {
		m := validManifest()
		tc.mutate(&m)

		if _, err := ParseManifest(mustJSON(t, m)); err == nil {
			t.Errorf("a manifest with no usable %s was accepted", tc.field)
		}
	}
}

// Usable IS SEPARATE FROM Validate, and this is the difference: a well-formed
// manifest for another architecture is not a defect, it is simply not for here.
func TestUsableSeparatesWellFormedFromApplicable(t *testing.T) {
	m := validManifest()

	if err := m.Validate(); err != nil {
		t.Fatalf("the fixture is not valid: %v", err)
	}

	if err := m.Usable("2", "x86_64"); err != nil {
		t.Fatalf("the image should be usable here: %v", err)
	}

	if err := m.Usable("3", "x86_64"); err == nil {
		t.Error("an image speaking a different guest contract was accepted")
	}

	if err := m.Usable("2", "aarch64"); err == nil {
		t.Error("an image built for another architecture was accepted")
	}
}

// THE CONTRACT IS COMPARED, NEVER ORDERED. A newer contract is not backward
// compatible by default, and treating it as "at least" turns a clean refusal
// into microVMs that boot and never report.
func TestUsableDoesNotTreatANewerContractAsCompatible(t *testing.T) {
	m := validManifest()
	m.GuestContract = "3"

	if err := m.Usable("2", "x86_64"); err == nil {
		t.Fatal("a newer guest contract was accepted by a build that speaks an older one")
	}
}
