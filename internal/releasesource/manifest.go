// Package releasesource describes what one billet release contains and what has
// to be true about it before any of its bytes replace a running binary.
//
// WHY A MANIFEST AND NOT checksums.txt. A checksum file answers "did these bytes
// arrive intact" and nothing else. Deciding whether a release may replace THIS
// process needs four more facts, and every one of them has to be readable before
// anything is downloaded, let alone installed: which node-wire versions the
// candidate speaks, which ledger schema it expects, which guest contract its
// images must satisfy, and which release it can be rolled back to. A rollout that
// learns any of those after the switch has already stopped the control plane.
//
// WHY THE MANIFEST IS THE ONLY THING THAT NEEDS SIGNING. It carries the digest of
// every artifact, so one signature over it transitively covers every binary and
// package in the release. The large files are then checked with a hash rather
// than public-key arithmetic — and, more importantly, an attacker who can serve a
// manifest can serve digests of bytes they chose, so without the signature every
// other check in this package is a checksum against itself.
package releasesource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The manifest layouts this build knows about.
//
// Bumped when a field changes meaning. A reader refuses a schema it does not know
// rather than interpreting unfamiliar fields, because the failure mode of
// guessing is installing the wrong bytes over a running control plane.
const (
	SchemaV1 = 1
)

// SchemaVersion is the layout this build WRITES.
//
// DELIBERATELY ALLOWED TO LAG WHAT IT READS, and that gap is the whole migration
// path. A reader accepts exactly the schemas it understands, so publishing a new
// layout in the same change that teaches the reader about it is a FLAG DAY: every
// deployment already in the field cannot read the release, and the thing that
// would fix them is the release they can no longer read. Readers learn the new
// schema and ship; only once the fleet carries them does the writer move.
//
// The same rule as imagesource.SchemaVersion, and for a worse reason: a guest
// image a node cannot read is a node that keeps running the old image, while a
// release a controller cannot read is a fleet with no way to update at all.
const SchemaVersion = SchemaV1

// readableSchemas is what this build will parse.
var readableSchemas = map[int]bool{
	SchemaV1: true,
}

// MaxManifestBytes bounds what a reader will parse.
//
// A manifest is a few kilobytes — a dozen artifacts and their digests. The bound
// exists because the document is fetched over the network before anything about
// the far end has been proven, and an unbounded read of an untrusted stream is
// how a fetch becomes a memory exhaustion.
const MaxManifestBytes = 256 << 10

// MaxArtifactBytes bounds one published artifact at 512 MiB.
//
// billet's own binary is around 22MB and an archive of it rather less. This is an
// order of magnitude above anything the release could legitimately contain, and
// still refuses a manifest whose size field would have an updater write until the
// disk filled.
const MaxArtifactBytes int64 = 512 << 20

// MaxArtifacts bounds how many files one release may declare.
//
// Three platforms times an archive, a .deb and an .rpm is nine, plus checksums.
// Fifty is generous against that and refuses a manifest that would turn one
// update into thousands of requests.
const MaxArtifacts = 50

var (
	// digestPattern is a lowercase hex sha256.
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// versionPattern is the semver shape billet's tags take. Anchored, because an
	// unanchored match would accept `v1.2.3-evil` and the version decides which
	// release a rollout believes it is installing.
	versionPattern = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
	// artifactName is a bare filename. Validated as one because it is joined to a
	// base URL to fetch and to a staging directory to write, so a name carrying a
	// separator or a parent reference would write outside the directory the
	// caller chose.
	artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Manifest is one immutable billet release: every artifact it publishes, and
// every fact a deployment needs in order to refuse it.
type Manifest struct {
	// Schema is the layout version of this document.
	Schema int `json:"schema"`

	// Version is the release, as its git tag spells it.
	Version string `json:"version"`

	// Commit is the commit the release was built from, for an operator
	// reconciling a running binary against what it claims to be.
	Commit string `json:"commit"`

	// BuiltAt is when the release was produced, in UTC.
	BuiltAt time.Time `json:"built_at"`

	// Wire is the node-wire range this release speaks.
	//
	// THE FIELD THAT DECIDES WHETHER A ROLLOUT CAN EVEN BEGIN. A candidate whose
	// range does not overlap the range the running control plane speaks cannot be
	// bridged to — there is no version both halves implement, so nodes could not
	// register against it — and that has to be knowable before the binary is
	// replaced rather than after the fleet has fallen off.
	Wire Range `json:"wire"`

	// SchemaVersion is the ledger migration this release expects.
	//
	// Migrations are append-only and a binary refuses a database carrying a
	// version it has never heard of, so a candidate BELOW the installed schema
	// cannot open the ledger it would inherit. That is a rollback the updater has
	// to refuse rather than discover after it has stopped the control plane.
	LedgerSchema int `json:"ledger_schema"`

	// GuestContract is the protocol a guest image's baked agent must speak.
	//
	// A STRING COMPARED FOR EQUALITY, never ordered, for the reason
	// imagesource.Manifest.GuestContract gives: a newer contract is not backward
	// compatible by default, and treating it as "greater than or equal" turns a
	// clean refusal into a guest that boots and never reports.
	GuestContract string `json:"guest_contract"`

	// Actions is the tag billet's bundled composite actions resolve to in this
	// release. It is the release's own version for an ordinary cut; it is carried
	// explicitly so a reader can check that rather than assume it.
	Actions string `json:"actions"`

	// RollbackTo is the release a failed update of this one restores.
	//
	// PART OF THE MANIFEST RATHER THAN DERIVED, because "the previous tag" is not
	// the same question as "a release this one can be rolled back to". A candidate
	// that migrated the ledger cannot be rolled back to a binary that refuses the
	// new schema, and only the release knows that about itself.
	//
	// EMPTY IS A VALUE. The first release billet ever publishes has nothing behind
	// it, and refusing that would mean no release could ever be the first.
	RollbackTo string `json:"rollback_to,omitempty"`

	// Artifacts is every published file, keyed by nothing — a reader selects by
	// os and arch.
	Artifacts []Artifact `json:"artifacts"`
}

// Range is the span of node-wire versions a release speaks, inclusive.
//
// ITS OWN TYPE RATHER THAN nodeapi.Range, because this package must be readable
// by a build whose nodeapi says something different — that is the entire point of
// carrying it. Converting happens at the comparison, where both sides are known.
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Artifact is one downloadable file and the digest that proves it arrived as
// published.
type Artifact struct {
	// Name is the file's name within the release. A BARE FILENAME, validated as
	// one; see artifactName.
	Name string `json:"name"`

	// OS and Arch are Go's spellings, matching the archive names the install
	// script already derives from uname.
	OS   string `json:"os"`
	Arch string `json:"arch"`

	// Kind separates the forms one platform publishes: an archive, a .deb, a
	// .rpm. An updater picks by kind, and an unknown one is refused rather than
	// treated as an archive.
	Kind string `json:"kind"`

	// SHA256 is the digest of the bytes as published, lowercase hex.
	SHA256 string `json:"sha256"`

	// Size is the published length in bytes, carried so a reader can bound the
	// download rather than discovering the length by exhausting a disk. A digest
	// alone cannot do that: it is only checkable after the last byte.
	Size int64 `json:"size"`
}

// The artifact kinds a reader will act on. An unknown kind is refused, because
// installing a .deb the way an archive is unpacked is not a degradation.
const (
	KindArchive = "archive"
	KindDeb     = "deb"
	KindRPM     = "rpm"
)

var knownKinds = map[string]bool{
	KindArchive: true,
	KindDeb:     true,
	KindRPM:     true,
}

// ParseManifest decodes and validates a manifest.
//
// THE ONLY WAY ONE IS PRODUCED FROM BYTES, so no caller can hold an unvalidated
// manifest — which is what lets the update path treat its digests, names and
// sizes as constrained rather than re-checking each at every use.
//
// STRICT DECODING, because a field this build does not know is a field whose
// meaning it cannot honour. A release describing a constraint through a key added
// after this binary shipped would otherwise be installed as though the constraint
// were absent. That is the opposite of the node wire's rule, and deliberately so:
// the wire negotiates a version both sides agreed on, while a manifest is a
// take-it-or-leave-it document about whether this binary may be replaced.
func ParseManifest(body []byte) (*Manifest, error) {
	if len(body) > MaxManifestBytes {
		return nil, fmt.Errorf("releasesource: the manifest is %d bytes, above the %d-byte "+
			"bound; refusing to parse it", len(body), MaxManifestBytes)
	}

	var m Manifest
	if err := decodeExactly(body, &m); err != nil {
		return nil, fmt.Errorf("releasesource: the release manifest could not be read: %w", err)
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}

	return &m, nil
}

// Validate reports everything wrong with a manifest, or nil.
//
// CALLED BEFORE ANY FIELD IS USED, including before the digests are trusted
// enough to check bytes against. The document arrives over the network from a
// service nobody here controls, so every field is an assertion by a stranger
// until it has been through this.
//
// EVERYTHING AT ONCE, rather than the first failure, because an operator fixing a
// hand-written manifest should not discover its problems one release at a time.
func (m *Manifest) Validate() error {
	var problems []error

	report := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if !readableSchemas[m.Schema] {
		// RETURNED ALONE. Every check below reads fields whose meaning is defined
		// by the schema, so reporting them against a layout this build does not
		// understand would be describing a document by rules that do not apply to
		// it.
		return fmt.Errorf("releasesource: this manifest uses schema %d, and this build reads "+
			"%s. A release published in a newer layout cannot be installed by an older "+
			"billet; upgrade through a release this one can read", m.Schema, readableList())
	}

	if !versionPattern.MatchString(m.Version) {
		report("version %q is not a billet release tag", m.Version)
	}

	if m.RollbackTo != "" && !versionPattern.MatchString(m.RollbackTo) {
		report("rollback_to %q is not a billet release tag", m.RollbackTo)
	}

	// A RELEASE CANNOT ROLL BACK TO ITSELF. It reads as harmless and is not: an
	// updater that failed and "restored" the same candidate would report a
	// successful rollback having changed nothing, and the control plane would come
	// back on the binary that had just failed its own verification.
	if m.RollbackTo != "" && m.RollbackTo == m.Version {
		report("rollback_to names this release, so a failed update would restore the "+
			"binary that just failed and report success (%s)", m.Version)
	}

	if strings.TrimSpace(m.Commit) == "" {
		report("the manifest names no commit")
	}

	if m.BuiltAt.IsZero() {
		report("the manifest has no build time")
	}

	if m.Wire.Min <= 0 || m.Wire.Max <= 0 || m.Wire.Min > m.Wire.Max {
		// CONTRADICTORY IS ITS OWN ANSWER, not "absent". A release declaring a
		// minimum above its maximum has described a range it cannot implement, and
		// normalising that to "speaks exactly Max" is the defect the node wire once
		// had: the peer is served a version it has just said it does not support.
		// Both an absent range and an impossible one are refused here.
		report("the wire range %d-%d is not a range a build can speak", m.Wire.Min, m.Wire.Max)
	}

	if m.LedgerSchema <= 0 {
		report("the manifest names no ledger schema, so nothing can tell whether this " +
			"release can open the database it would inherit")
	}

	if strings.TrimSpace(m.GuestContract) == "" {
		report("the manifest names no guest contract, so nothing can tell which images " +
			"this release can boot")
	}

	if !versionPattern.MatchString(m.Actions) {
		report("actions %q is not a billet release tag; a release's bundled actions "+
			"resolve to one tag", m.Actions)
	}

	problems = append(problems, m.validateArtifacts()...)

	return errors.Join(problems...)
}

func (m *Manifest) validateArtifacts() []error {
	var problems []error

	report := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if len(m.Artifacts) == 0 {
		report("the manifest publishes no artifacts, so there is nothing to install")

		return problems
	}

	if len(m.Artifacts) > MaxArtifacts {
		report("the manifest names %d artifacts, above the bound of %d",
			len(m.Artifacts), MaxArtifacts)

		return problems
	}

	// ONE NAME, ONE ARTIFACT. Two entries sharing a name would have a reader
	// verify one digest and install whichever the map or the loop reached last —
	// a manifest that passes every individual check and installs bytes nothing
	// vouched for.
	seen := make(map[string]bool, len(m.Artifacts))

	// AND ONE ARTIFACT PER PLATFORM AND KIND, for the same reason from the other
	// direction: a selector that finds two candidates for linux/amd64 has no
	// defensible way to choose.
	slots := make(map[string]bool, len(m.Artifacts))

	for i := range m.Artifacts {
		a := &m.Artifacts[i]

		switch {
		case !artifactName.MatchString(a.Name):
			report("artifact %d has name %q, which is not a plain file name", i, a.Name)
		case seen[a.Name]:
			report("artifact %q is named twice", a.Name)
		default:
			seen[a.Name] = true
		}

		if !digestPattern.MatchString(a.SHA256) {
			report("artifact %q has digest %q, which is not a sha256", a.Name, a.SHA256)
		}

		if a.Size <= 0 || a.Size > MaxArtifactBytes {
			report("artifact %q is %d bytes, outside 1..%d", a.Name, a.Size, MaxArtifactBytes)
		}

		if !knownKinds[a.Kind] {
			report("artifact %q is kind %q, which this build does not know how to install",
				a.Name, a.Kind)
		}

		if strings.TrimSpace(a.OS) == "" || strings.TrimSpace(a.Arch) == "" {
			report("artifact %q does not say which platform it is for", a.Name)

			continue
		}

		slot := a.OS + "/" + a.Arch + "/" + a.Kind
		if slots[slot] {
			report("two artifacts claim %s; a reader has no way to choose between them", slot)
		}

		slots[slot] = true
	}

	return problems
}

// Select finds the artifact for one platform and kind.
//
// AN ABSENCE IS AN ERROR RATHER THAN A ZERO VALUE. A release that does not
// publish darwin/arm64 is a release a Mac cannot install, and returning an empty
// Artifact would have the caller verify an empty digest against no bytes and
// report success.
func (m *Manifest) Select(goos, goarch, kind string) (*Artifact, error) {
	for i := range m.Artifacts {
		a := &m.Artifacts[i]
		if a.OS == goos && a.Arch == goarch && a.Kind == kind {
			return a, nil
		}
	}

	return nil, fmt.Errorf("releasesource: %s publishes no %s for %s/%s",
		m.Version, kind, goos, goarch)
}

func readableList() string {
	schemas := make([]string, 0, len(readableSchemas))

	for schema := range readableSchemas {
		schemas = append(schemas, strconv.Itoa(schema))
	}

	// SORTED, because a map's order is not stable and a diagnostic that reads
	// differently on each run is one nobody can grep a log for.
	sort.Strings(schemas)

	return strings.Join(schemas, ", ")
}

// decodeExactly reads one JSON document and refuses anything else in the bytes.
//
// TWO CHECKS, AND EACH CLOSES A DIFFERENT HOLE. DisallowUnknownFields refuses a
// field this build does not know, so a document describing a constraint added
// after this binary shipped cannot be acted on as though the constraint were
// absent. The trailing-content check refuses a SECOND document after the first,
// which the decoder otherwise ignores entirely — and a signature covers the whole
// body, so a reader that stopped at the first object would be verifying bytes it
// never looked at.
//
// SHARED BY THE MANIFEST AND THE CHANNEL because they are both signed documents
// fetched over the network, and a strictness that applied to one of them is a
// strictness somebody has to remember to apply to the next one.
func decodeExactly(body []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(into); err != nil {
		return err
	}

	// EOF IS THE ONLY ACCEPTABLE SECOND ANSWER. Anything else is content the
	// signature covered and the reader did not.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("there is more than one document in these bytes; a signature "+
			"covers all of them and a reader that stopped at the first would be verifying "+
			"content it never looked at (second decode: %v)", err)
	}

	return nil
}
