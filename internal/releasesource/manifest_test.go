package releasesource

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// good is a manifest that passes, so a test can break exactly one thing.
//
// A BUILDER RATHER THAN A FIXTURE FILE, because every test here is about one
// field being wrong and a shared literal makes it impossible to see which.
func good() *Manifest {
	return &Manifest{
		Schema:        SchemaV1,
		Version:       "v0.4.0",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		BuiltAt:       time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		Wire:          Range{Min: 12, Max: 13},
		LedgerSchema:  35,
		GuestContract: "billet-guest-1",
		Actions:       "v0.4.0",
		RollbackTo:    "v0.3.26",
		Artifacts: []Artifact{{
			Name: "billet_0.4.0_linux_amd64.tar.gz", OS: "linux", Arch: "amd64",
			Kind: KindArchive, Size: 9_000_000,
			SHA256: strings.Repeat("a", 64),
		}},
	}
}

func mustJSON(t *testing.T, m *Manifest) []byte {
	t.Helper()

	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return body
}

func TestAGoodManifestParses(t *testing.T) {
	t.Parallel()

	m, err := ParseManifest(mustJSON(t, good()))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	if m.Version != "v0.4.0" {
		t.Errorf("version is %q", m.Version)
	}
}

// A SCHEMA THIS BUILD DOES NOT READ IS REFUSED, AND ALONE.
//
// Every other check reads fields whose meaning the schema defines, so reporting
// them against a layout this build does not understand would be describing a
// document by rules that do not apply to it. The diagnostic has to name the
// upgrade path, because the release that would fix the reader is the one it
// cannot read.
func TestAnUnknownSchemaIsRefusedOnItsOwn(t *testing.T) {
	t.Parallel()

	m := good()
	m.Schema = SchemaV1 + 1
	// Also break something else, to prove the schema refusal is returned alone.
	m.Version = "not-a-tag"

	_, err := ParseManifest(mustJSON(t, m))
	if err == nil {
		t.Fatal("a manifest in an unreadable schema parsed")
	}

	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("the refusal does not name the schema: %v", err)
	}

	if strings.Contains(err.Error(), "not a billet release tag") {
		t.Errorf("the refusal reported a field whose meaning the unknown schema defines: %v", err)
	}
}

// A FIELD THIS BUILD DOES NOT KNOW IS REFUSED.
//
// A release describing a constraint through a key added after this binary shipped
// would otherwise be installed as though the constraint were absent. That is the
// opposite of the node wire's rule and deliberately so: the wire negotiates a
// version both sides agreed on, while a manifest is a take-it-or-leave-it
// document about whether this binary may be replaced.
func TestAnUnknownFieldIsRefused(t *testing.T) {
	t.Parallel()

	body := mustJSON(t, good())
	augmented := strings.Replace(string(body), `{`, `{"requires_attestation":true,`, 1)

	if _, err := ParseManifest([]byte(augmented)); err == nil {
		t.Fatal("a manifest carrying an unknown constraint parsed as though it were absent")
	}
}

func TestManifestRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{{
		name:   "version is not a tag",
		mutate: func(m *Manifest) { m.Version = "0.4.0" },
		want:   "not a billet release tag",
	}, {
		// UNANCHORED WOULD ACCEPT THIS, and the version decides which release a
		// rollout believes it installed.
		name:   "version has a suffix",
		mutate: func(m *Manifest) { m.Version = "v0.4.0-evil" },
		want:   "not a billet release tag",
	}, {
		name:   "rollback names this release",
		mutate: func(m *Manifest) { m.RollbackTo = m.Version },
		want:   "restore the binary that just failed",
	}, {
		// CONTRADICTORY IS ITS OWN ANSWER, not "absent" — the defect the node wire
		// once had, in the document that describes the wire.
		name:   "wire range is impossible",
		mutate: func(m *Manifest) { m.Wire = Range{Min: 14, Max: 12} },
		want:   "not a range a build can speak",
	}, {
		name:   "wire range is absent",
		mutate: func(m *Manifest) { m.Wire = Range{} },
		want:   "not a range a build can speak",
	}, {
		name:   "no ledger schema",
		mutate: func(m *Manifest) { m.LedgerSchema = 0 },
		want:   "names no ledger schema",
	}, {
		name:   "no guest contract",
		mutate: func(m *Manifest) { m.GuestContract = "  " },
		want:   "names no guest contract",
	}, {
		name:   "actions tag is not a release",
		mutate: func(m *Manifest) { m.Actions = "main" },
		want:   "bundled actions resolve to one tag",
	}, {
		name:   "no artifacts",
		mutate: func(m *Manifest) { m.Artifacts = nil },
		want:   "nothing to install",
	}, {
		// A NAME IS JOINED TO A URL AND TO A DIRECTORY, so a separator or a parent
		// reference writes outside the directory the caller chose.
		name:   "artifact escapes its directory",
		mutate: func(m *Manifest) { m.Artifacts[0].Name = "../../etc/cron.d/x" },
		want:   "not a plain file name",
	}, {
		name:   "artifact digest is not a sha256",
		mutate: func(m *Manifest) { m.Artifacts[0].SHA256 = "deadbeef" },
		want:   "which is not a sha256",
	}, {
		name:   "artifact size is absent",
		mutate: func(m *Manifest) { m.Artifacts[0].Size = 0 },
		want:   "outside 1..",
	}, {
		name:   "artifact size is beyond the bound",
		mutate: func(m *Manifest) { m.Artifacts[0].Size = MaxArtifactBytes + 1 },
		want:   "outside 1..",
	}, {
		name:   "artifact kind is unknown",
		mutate: func(m *Manifest) { m.Artifacts[0].Kind = "pkg" },
		want:   "does not know how to install",
	}, {
		// TWO ENTRIES SHARING A NAME would have a reader verify one digest and
		// install whichever the loop reached last.
		name: "two artifacts share a name",
		mutate: func(m *Manifest) {
			second := m.Artifacts[0]
			second.Arch = "arm64"
			m.Artifacts = append(m.Artifacts, second)
		},
		want: "is named twice",
	}, {
		// AND TWO CLAIMING ONE PLATFORM, from the other direction: a selector that
		// finds two candidates has no defensible way to choose.
		name: "two artifacts claim one platform",
		mutate: func(m *Manifest) {
			second := m.Artifacts[0]
			second.Name = "billet_0.4.0_linux_amd64.alt.tar.gz"
			m.Artifacts = append(m.Artifacts, second)
		},
		want: "no way to choose between them",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := good()
			tc.mutate(m)

			_, err := ParseManifest(mustJSON(t, m))
			if err == nil {
				t.Fatalf("this manifest was accepted: %+v", m)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q:\n%v", tc.want, err)
			}
		})
	}
}

// A MANIFEST LARGER THAN THE BOUND IS NOT PARSED AT ALL. It arrives over the
// network before anything about the far end has been proven.
func TestAnOversizeManifestIsRefusedBeforeParsing(t *testing.T) {
	t.Parallel()

	if _, err := ParseManifest(make([]byte, MaxManifestBytes+1)); err == nil {
		t.Fatal("an oversize manifest was parsed")
	}
}

// AN ABSENT PLATFORM IS AN ERROR, NOT A ZERO ARTIFACT. Returning an empty
// Artifact would have the caller verify an empty digest against no bytes and
// report success.
func TestSelectingAnUnpublishedPlatformIsAnError(t *testing.T) {
	t.Parallel()

	m := good()

	if _, err := m.Select("darwin", "arm64", KindArchive); err == nil {
		t.Fatal("selecting a platform the release does not publish returned an artifact")
	}

	if _, err := m.Select("linux", "amd64", KindArchive); err != nil {
		t.Errorf("selecting a published platform failed: %v", err)
	}
}

// THE FIRST RELEASE HAS NOTHING BEHIND IT, and refusing that would mean no
// release could ever be the first.
func TestAnEmptyRollbackTargetIsAllowed(t *testing.T) {
	t.Parallel()

	m := good()
	m.RollbackTo = ""

	if _, err := ParseManifest(mustJSON(t, m)); err != nil {
		t.Errorf("a release with no rollback target was refused: %v", err)
	}
}

// THE WRITER'S SCHEMA MAY NOT RUN AHEAD OF THE READER'S.
//
// Publishing a layout in the same change that teaches the reader about it is a
// flag day: every deployment in the field cannot read the release, and the thing
// that would fix them is the release they can no longer read.
func TestTheWrittenSchemaIsOneThisBuildCanRead(t *testing.T) {
	t.Parallel()

	if !readableSchemas[SchemaVersion] {
		t.Fatalf("this build writes schema %d and cannot read it", SchemaVersion)
	}
}

// AND SO IS THE MANIFEST. It already refused unknown fields; what it did not
// refuse was a second document after the first, which the decoder ignores
// entirely.
func TestAManifestRefusesTrailingContent(t *testing.T) {
	t.Parallel()

	body := mustJSON(t, good())
	trailing := append(append([]byte{}, body...), []byte(`{"schema":1}`)...)

	if _, err := ParseManifest(trailing); err == nil {
		t.Error("a manifest with a second document appended was accepted; the signature " +
			"covers those bytes and the reader did not")
	}
}
