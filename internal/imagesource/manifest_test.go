package imagesource

import (
	"bytes"
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

// PADDED WITH JSON WHITESPACE, NOT NUL BYTES. The first version of this test
// filled the buffer with zeroes, which is invalid JSON -- so the decoder
// rejected it whether or not the size bound existed, and the test agreed with
// the bug it was written to prevent. Leading whitespace is legal JSON, so this
// document is refused ONLY by the bound.
func TestParseManifestRefusesAnOversizeDocument(t *testing.T) {
	body := mustJSON(t, validManifest())

	padded := append(bytes.Repeat([]byte(" "), MaxManifestBytes+1), body...)

	if _, err := ParseManifest(padded); err == nil {
		t.Fatal("an oversize document was parsed")
	}

	// AND THE SAME DOCUMENT UNDER THE BOUND IS ACCEPTED, which is what proves the
	// refusal above came from the size and not from the padding.
	small := append(bytes.Repeat([]byte(" "), 16), body...)

	if _, err := ParseManifest(small); err != nil {
		t.Fatalf("a whitespace-padded manifest within the bound was refused: %v", err)
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

// `Decoder.More()` REPORTS ON ELEMENTS INSIDE AN ARRAY OR OBJECT, not on a
// second top-level value. Measured: it returns false for `{...}}` and `{...}]`,
// so the first version of this check accepted both. Only a second COMPLETE
// value made it true.
func TestParseManifestRefusesEveryKindOfTrailingContent(t *testing.T) {
	body := string(mustJSON(t, validManifest()))

	for _, suffix := range []string{
		"}",
		"]",
		body,
		"null",
		"\n{",
		",",
	} {
		t.Run(fmt.Sprintf("%q", suffix), func(t *testing.T) {
			if _, err := ParseManifest([]byte(body + suffix)); err == nil {
				t.Fatalf("a manifest followed by %q was accepted as one document", suffix)
			}
		})
	}

	// Trailing whitespace is not content and must still be accepted.
	if _, err := ParseManifest([]byte(body + "\n  \t\n")); err != nil {
		t.Errorf("trailing whitespace was treated as content: %v", err)
	}
}

// encoding/json TAKES THE LAST OF A DUPLICATED KEY AND MATCHES FIELD NAMES
// CASE-INSENSITIVELY, and DisallowUnknownFields raises nothing for either. For a
// document whose purpose is to be signed and agreed upon, that is a parser
// differential: one signed byte string reads as two different manifests.
func TestParseManifestRefusesAmbiguousKeys(t *testing.T) {
	body := string(mustJSON(t, validManifest()))

	dup := strings.Replace(body, `{"schema"`, `{"schema":99,"schema"`, 1)
	if dup == body {
		t.Fatal("could not duplicate a key in the fixture")
	}

	if _, err := ParseManifest([]byte(dup)); err == nil {
		t.Error("a manifest carrying one key twice was accepted")
	}

	// THE CASE THAT DisallowUnknownFields CANNOT SEE.
	miscased := strings.Replace(body, `"runner_version"`, `"Runner_Version"`, 1)
	if miscased == body {
		t.Fatal("could not recase a key in the fixture")
	}

	if _, err := ParseManifest([]byte(miscased)); err == nil {
		t.Error("a manifest whose key differs only in case was accepted; json would " +
			"match it onto the real field and a second reader might not")
	}

	// A VALUE that happens to equal a field name is not a key and must be fine.
	valued := strings.Replace(body, `"arch": "x86_64"`, `"arch": "schema"`, 1)
	if valued != body {
		if _, err := ParseManifest([]byte(valued)); err == nil {
			t.Error("a value equal to a field name should not be usable, but it should be " +
				"refused as an arch, not as a duplicate key")
		}
	}
}

// APFS's DEFAULT IS CASE-FOLDING, so "ROOTFS" and "rootfs" are one file in the
// staging directory: the second download replaces the first, and each digest
// check then passes against whichever landed last.
func TestParseManifestRefusesNamesThatCollideOnACaseFoldingFilesystem(t *testing.T) {
	m := validManifest()
	m.Rootfs.Name = "guest.img"
	m.Kernel.Name = "GUEST.IMG"

	if _, err := ParseManifest(mustJSON(t, m)); err == nil {
		t.Fatal("two asset names differing only in case were accepted; on a case-folding " +
			"filesystem they are one file")
	}
}

// THE FIELD REACHES OPERATOR-FACING ERRORS, so an unbounded string from the
// network can carry newlines and terminal control sequences into a console.
func TestParseManifestConstrainsTheIdentityStrings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"contract with a newline", func(m *Manifest) { m.GuestContract = "2\nrm -rf /" }},
		{"contract with an escape", func(m *Manifest) { m.GuestContract = "2\x1b[2J" }},
		{"contract that is not a number", func(m *Manifest) { m.GuestContract = "two" }},
		{"runner version that is prose", func(m *Manifest) { m.RunnerVersion = "recent" }},
		{"runner version with a newline", func(m *Manifest) { m.RunnerVersion = "2.336.0\nx" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)

			if _, err := ParseManifest(mustJSON(t, m)); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

// UTC, AS THE FIELD CLAIMS. Ages are computed from this across machines in
// different zones, and an offset that survives into those comparisons is the bug
// that shows up once at a daylight-saving boundary and never reproduces.
func TestParseManifestRefusesANonUTCBuildTime(t *testing.T) {
	m := validManifest()
	m.BuiltAt = time.Date(2026, 8, 14, 6, 18, 44, 0, time.FixedZone("somewhere", 5*3600))

	if _, err := ParseManifest(mustJSON(t, m)); err == nil {
		t.Fatal("a build time carrying a zone offset was accepted")
	}
}

// Usable IS A GATE, and a gate that assumes somebody else validated is not one.
// Manifest is exported with exported fields, so a caller can build one as a
// literal or mutate the one ParseManifest returned.
func TestUsableRevalidatesRatherThanTrustingItsCaller(t *testing.T) {
	hand := &Manifest{GuestContract: "2", Arch: "x86_64"}

	if err := hand.Usable("2", "x86_64"); err == nil {
		t.Fatal("a hand-built manifest with no assets passed the usability gate")
	}

	m := validManifest()

	if err := (&m).Usable("2", "x86_64"); err != nil {
		t.Fatalf("a valid manifest was refused: %v", err)
	}

	// Mutated after parsing, which is the other way a caller gets here.
	m.Rootfs.SHA256 = "not a digest"

	if err := (&m).Usable("2", "x86_64"); err == nil {
		t.Fatal("a manifest mutated after validation passed the gate")
	}
}
