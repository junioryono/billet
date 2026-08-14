// Package imagesource describes where a published guest image comes from and
// what has to be true about it before any of its bytes reach the cluster.
//
// WHY A MANIFEST AND NOT JUST A CHECKSUM FILE. A node has to decide whether it
// can USE an image before it spends four hundred megabytes finding out. Two
// facts settle that — which guest contract the image speaks, and which
// architecture it was built for — and both have to be readable from a document
// small enough to fetch on every check. A SHA256SUMS file carries neither, so a
// node holding only checksums learns that an image is unusable after
// downloading it, every time, forever.
//
// WHY THE MANIFEST IS THE ONLY THING THAT NEEDS SIGNING. It carries the digest
// of every asset, so a signature over the manifest transitively covers the
// image and the kernel. One signature, one verification, and the large files
// are checked with a hash rather than public-key arithmetic.
package imagesource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/runnerrelease"
)

// SchemaVersion is the manifest layout this build writes and can read.
//
// Bumped when a field changes meaning. A reader refuses a schema it does not
// know rather than interpreting unfamiliar fields, because the failure mode of
// guessing is booting the wrong bytes.
const SchemaVersion = 1

// MaxManifestBytes bounds what a reader will parse.
//
// A manifest is a few hundred bytes. The bound exists because the document is
// fetched over the network before anything about the far end has been proven,
// and an unbounded read of an untrusted stream is how a fetch becomes a memory
// exhaustion. 64 KiB is roughly two hundred times the real size.
const MaxManifestBytes = 64 << 10

// Manifest describes one published guest image: the root filesystem, the kernel
// built to run it, and the facts a node needs in order to refuse it.
type Manifest struct {
	// Schema is the layout version of this document.
	Schema int `json:"schema"`

	// GuestContract is the protocol the baked agent speaks to the host.
	//
	// A STRING, NOT AN INTEGER, because it is compared for equality against the
	// provider's own constant and never ordered. Making it a number invites a
	// reader to accept "greater than or equal", which is exactly wrong: a newer
	// contract is not backward compatible by default, and assuming it is turns a
	// clean refusal into a guest that boots and never reports.
	GuestContract string `json:"guest_contract"`

	// Arch is the machine the image was built for, as `uname -m` spells it.
	Arch string `json:"arch"`

	// RunnerVersion is the Actions runner baked into the image.
	//
	// THE FIELD THAT MAKES THE THIRTY-DAY RULE VISIBLE. GitHub stops handing jobs
	// to a runner more than thirty days behind a release, so a node can read this
	// from the manifest and decline to import something already too old to work —
	// before downloading it, and without booting it.
	RunnerVersion string `json:"runner_version"`

	// BuiltAt is when the image was produced, in UTC.
	BuiltAt time.Time `json:"built_at"`

	// Rootfs is the guest's root filesystem.
	Rootfs Asset `json:"rootfs"`

	// Kernel is the kernel built to boot it.
	//
	// SHIPPED AS A PAIR AND VERSIONED TOGETHER. Whether a kernel can boot a
	// given root filesystem is a property of the two together — the guest's
	// init, its cgroup layout and the options Docker needs are all decided at
	// kernel configuration time. A mismatch is not a degradation, it is a VM
	// that does not come up, so the two travel in one manifest and are never
	// resolved independently.
	Kernel Asset `json:"kernel"`
}

// Asset is one downloadable file and the digest that proves it arrived intact.
type Asset struct {
	// Name is the file's name within the release.
	//
	// A BARE FILENAME, VALIDATED AS ONE. It is joined to a base URL to fetch and
	// to a staging directory to write, so a name carrying a separator or a
	// parent reference would write outside the directory the caller chose. The
	// check lives in Validate rather than at each use, so no future call site
	// can forget it.
	Name string `json:"name"`

	// SHA256 is the digest of the bytes as published, lowercase hex.
	//
	// OF THE PUBLISHED BYTES, NOT THE DECOMPRESSED ONES. It is checked against
	// exactly what came off the network, so verification does not depend on the
	// decompressor behaving, and a corrupt download is caught before anything
	// tries to interpret it.
	SHA256 string `json:"sha256"`

	// Size is the published length in bytes.
	//
	// Carried so a reader can bound the download rather than discovering the
	// length by exhausting a disk. A digest alone cannot do that: it is only
	// checkable after the last byte.
	Size int64 `json:"size"`

	// Compression names how the bytes are packed: "" for none, or "zstd".
	Compression string `json:"compression,omitempty"`

	// Version is what the artifact calls itself, where that means something —
	// the kernel's release for a kernel. Empty elsewhere.
	Version string `json:"version,omitempty"`
}

// MaxAssetBytes bounds a single published asset at 8 GiB.
//
// The root filesystem is four gigabytes raw and under half a gigabyte packed,
// so this is generous by an order of magnitude while still refusing a manifest
// whose size field would have a node write until the disk filled. GitHub caps a
// release asset at 2 GiB, so anything approaching this bound is a manifest that
// could not have been published by the pipeline that claims to have made it.
const MaxAssetBytes int64 = 8 << 30

var (
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	archPattern   = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

	// The guest contract is a small integer today. Bounded rather than free text
	// because it reaches operator-facing errors.
	contractPattern = regexp.MustCompile(`^\d{1,8}$`)

	// A runner version as github publishes them: 2.336.0, occasionally with a
	// suffix. Bounded, and parseable enough to compare.
	versionPattern = regexp.MustCompile(`^\d{1,6}\.\d{1,6}\.\d{1,6}([-+][0-9A-Za-z.-]{1,32})?$`)
)

// knownCompression is the set a reader will act on.
//
// An unknown value is refused rather than treated as "no compression", because
// the two differ by whether the bytes are run through a decompressor, and
// guessing wrong produces a root filesystem made of compressed data.
var knownCompression = map[string]bool{
	"":     true,
	"zstd": true,
}

// Validate reports everything wrong with a manifest, or nil.
//
// CALLED BEFORE ANY FIELD IS USED, including before the digests are trusted
// enough to check bytes against. The document arrives over the network from a
// service nobody here controls, so every field is treated as an assertion by a
// stranger until it has been through this.
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("imagesource: no manifest")
	}

	if m.Schema != SchemaVersion {
		return fmt.Errorf("imagesource: this build reads manifest schema %d and the "+
			"published one is schema %d; upgrade billet rather than importing an image "+
			"it cannot describe", SchemaVersion, m.Schema)
	}

	// CONSTRAINED, NOT MERELY NON-EMPTY. This value is interpolated into operator-
	// facing errors, and an unbounded string from the network can carry newlines
	// and terminal control sequences -- which is how a refusal message becomes a
	// place to write whatever the publisher likes on somebody's console.
	if !contractPattern.MatchString(m.GuestContract) {
		return fmt.Errorf("imagesource: %q is not a guest contract version", m.GuestContract)
	}

	if !archPattern.MatchString(m.Arch) {
		return fmt.Errorf("imagesource: %q is not an architecture name", m.Arch)
	}

	// A REAL VERSION, because it is compared against github's published releases
	// to decide whether the baked runner is still inside the thirty days. "recent"
	// is not something that comparison can do anything with.
	if !versionPattern.MatchString(m.RunnerVersion) {
		return fmt.Errorf("imagesource: %q is not a runner version, so nothing can tell "+
			"whether the baked runner is still inside github's thirty days", m.RunnerVersion)
	}

	if m.BuiltAt.IsZero() {
		return fmt.Errorf("imagesource: the manifest carries no build time")
	}

	// UTC, AS THE FIELD SAYS. Ages and orderings are computed from this across
	// machines in different zones, and an offset that survives into those
	// comparisons is the kind of bug that appears once at a daylight-saving
	// boundary and never reproduces. The generation names in the cluster are UTC
	// by construction for the same reason.
	if _, offset := m.BuiltAt.Zone(); offset != 0 {
		return fmt.Errorf("imagesource: the manifest's build time carries a %+d-second offset "+
			"and must be UTC", offset)
	}

	if err := m.Rootfs.validate("rootfs"); err != nil {
		return err
	}

	if err := m.Kernel.validate("kernel"); err != nil {
		return err
	}

	// TWO ASSETS, TWO NAMES. They are staged side by side in one directory, so
	// equal names would have the second overwrite the first and the digest check
	// would then pass against whichever landed last.
	// EqualFold, NOT ==. The staging directory may be on a case-folding filesystem
	// -- APFS's default, and the machine a developer runs this on -- where
	// "ROOTFS" and "rootfs" are one file. A byte-wise comparison passes and the
	// second download silently replaces the first, after which each digest check
	// passes against whichever landed last.
	if strings.EqualFold(m.Rootfs.Name, m.Kernel.Name) {
		return fmt.Errorf("imagesource: the rootfs and the kernel are both published as %q, "+
			"and staging them together would leave one overwriting the other", m.Rootfs.Name)
	}

	return nil
}

func (a *Asset) validate(field string) error {
	if !namePattern.MatchString(a.Name) {
		return fmt.Errorf("imagesource: %s is published as %q, which is not a plain file name; "+
			"a name carrying a path would be fetched from and written to somewhere nobody chose",
			field, a.Name)
	}

	if !digestPattern.MatchString(a.SHA256) {
		return fmt.Errorf("imagesource: %s carries %q where a sha256 digest belongs, so nothing "+
			"downloaded could be proven to be what was published", field, a.SHA256)
	}

	if a.Size <= 0 {
		return fmt.Errorf("imagesource: %s is published with a size of %d bytes", field, a.Size)
	}

	if a.Size > MaxAssetBytes {
		return fmt.Errorf("imagesource: %s is published as %d bytes, past the %d-byte bound this "+
			"reads; refusing rather than downloading until something fills",
			field, a.Size, MaxAssetBytes)
	}

	if !knownCompression[a.Compression] {
		return fmt.Errorf("imagesource: %s is packed with %q, which this build cannot unpack; "+
			"refusing rather than treating it as uncompressed", field, a.Compression)
	}

	return nil
}

// ParseManifest decodes and validates a manifest document.
//
// REFUSES TRAILING CONTENT. A document with a second JSON value after the first
// is not a manifest, and accepting one lets a publisher — or anything that can
// rewrite the response — append a version that a different reader would honour.
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("imagesource: the manifest is empty")
	}

	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("imagesource: the manifest is %d bytes, past the %d-byte bound",
			len(data), MaxManifestBytes)
	}

	// KEYS ARE CHECKED BEFORE THE DOCUMENT IS DECODED, because encoding/json
	// cannot express what this needs. It matches field names CASE-INSENSITIVELY
	// and silently takes the LAST of a duplicated key, so `{"schema":1,
	// "Schema":99}` decodes to 99 with DisallowUnknownFields raising nothing.
	// For a document whose whole purpose is to be signed and agreed upon by
	// several readers, that is a parser differential: two implementations can
	// read one signed byte string as two different manifests.
	if err := checkStrictKeys(data); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))

	// UNKNOWN FIELDS ARE REFUSED. A manifest written by a newer publisher may
	// carry a field this build would need in order to import safely — a new
	// compression, a second kernel — and silently dropping it produces a
	// confident import of something misunderstood. The schema number is the
	// intended way to widen this.
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("imagesource: could not read the manifest: %w", err)
	}

	// A SECOND DECODE REQUIRING EOF, NOT Decoder.More().
	//
	// More() reports whether another element follows INSIDE an array or object
	// being parsed. At the top level it answers by peeking for `]` or `}` — so
	// `{...}}` and `{...}]` both make it return false, and this accepted them.
	// Measured, not reasoned about: only a second complete value made More() true.
	//
	// Decoding again and demanding io.EOF is the check that was meant: anything
	// at all after the manifest, well formed or not, is refused.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("imagesource: the manifest is followed by more content, so it is " +
			"not a single document and this refuses to guess which part is authoritative")
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}

	return &m, nil
}

// knownKeys is every field name a manifest may carry, spelled exactly.
//
// FLAT RATHER THAN PER-LEVEL because the names happen to be distinct across
// levels, and a flat set is one thing to keep right instead of three. If a
// future field collides with one at another level, split this.
var knownKeys = map[string]bool{
	"schema": true, "guest_contract": true, "arch": true,
	"runner_version": true, "built_at": true, "rootfs": true, "kernel": true,
	"name": true, "sha256": true, "size": true,
	"compression": true, "version": true,
}

// checkStrictKeys refuses duplicate or misspelled object keys anywhere in the
// document.
//
// A RECURSIVE DESCENT RATHER THAN A FLAT TOKEN SCAN, because the flat version
// cannot tell a key from a string value -- and the case it must catch,
// `{"Schema": 99}`, is precisely a key. Walking the structure means every string
// this inspects is known to be a name.
//
// WALKS TOKENS RATHER THAN UNMARSHALLING, because unmarshalling into a map is
// what loses the information: a map cannot hold a duplicate, so by the time
// there is a map the ambiguity has already been resolved in silence.
func checkStrictKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))

	tok, err := dec.Token()
	if err != nil {
		// Malformed input is reported by the real decode, whose message is far
		// better than anything a token walk can produce.
		return nil //nolint:nilerr // the decode that follows reports the parse error
	}

	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		// Not an object at the top level. The decode refuses it on its own terms.
		return nil
	}

	return walkObject(dec)
}

// walkObject consumes an object whose opening brace has already been read.
func walkObject(dec *json.Decoder) error {
	seen := map[string]bool{}

	for {
		tok, err := dec.Token()
		if err != nil {
			return nil //nolint:nilerr // reported by the real decode
		}

		if delim, ok := tok.(json.Delim); ok && delim == '}' {
			return nil
		}

		// Positioned on a name: inside an object, anything that is not the closing
		// brace is a key, and json guarantees it is a string.
		key, ok := tok.(string)
		if !ok {
			return nil
		}

		if seen[key] {
			return fmt.Errorf("imagesource: the manifest carries the key %q more than once; "+
				"json decoders differ on which one wins, so a single signed document would "+
				"read as two different manifests", key)
		}

		seen[key] = true

		// EXACT SPELLING. encoding/json matches field names case-insensitively, so
		// `{"Schema": 99}` decodes into Schema and DisallowUnknownFields raises
		// nothing. Two readers of one signed document would then disagree about
		// what it says.
		if !knownKeys[key] {
			return fmt.Errorf("imagesource: the manifest carries the key %q, which is not a "+
				"field of a manifest; json would match it case-insensitively onto one, and a "+
				"document that reads differently to different readers cannot be signed for", key)
		}

		if err := walkValue(dec); err != nil {
			return err
		}
	}
}

// walkValue consumes exactly one value.
func walkValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return nil //nolint:nilerr // reported by the real decode
	}

	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		return walkObject(dec)
	case '[':
		for dec.More() {
			if err := walkValue(dec); err != nil {
				return err
			}
		}

		// Consume the closing bracket. Inside an array More() is exactly what it
		// was designed for, so it is correct here in a way it is not at the top
		// level. A failure here is reported by the real decode, whose message is
		// better than anything this walk could produce.
		if _, err := dec.Token(); err != nil {
			return nil //nolint:nilerr // the decode that follows reports the parse error
		}

		return nil
	}

	return nil
}

// Usable reports whether this build can import the image the manifest names.
//
// SEPARATE FROM Validate, because the two answer different questions and only
// one of them means the publisher did something wrong. Validate asks whether the
// document is well formed; this asks whether THIS billet, on THIS machine, can
// use what it describes. A well-formed manifest for another architecture is not
// a defect to report to anyone — it is simply not for this host.
func (m *Manifest) Usable(contract, arch string) error {
	// REVALIDATED HERE. Manifest is an exported struct with exported fields, so a
	// caller can build one as a literal or mutate the one ParseManifest returned.
	// This method is a gate, and a gate that assumes somebody else checked is not
	// one.
	if err := m.Validate(); err != nil {
		return err
	}

	if m.GuestContract != contract {
		return fmt.Errorf("imagesource: this image's agent speaks guest contract %q and this "+
			"billet speaks %q; importing it would produce microVMs that boot and never report",
			m.GuestContract, contract)
	}

	if m.Arch != arch {
		return fmt.Errorf("imagesource: this image was built for %q and this host is %q",
			m.Arch, arch)
	}

	return nil
}

// Age is how long ago the image was built, as of now.
func (m *Manifest) Age(now time.Time) time.Duration { return now.Sub(m.BuiltAt) }

// Stale reports that the baked runner is certainly past GitHub's grace period.
//
// A ONE-DIRECTIONAL CHECK, AND THAT IS THE POINT. GitHub stops handing jobs to a
// runner more than thirty days past its release, and this cannot prove a runner
// is CURRENT — the manifest names a version, not the date that version shipped,
// and settling that needs a network call this package deliberately does not
// make. What it CAN prove, from the build time alone, is the opposite: a runner
// baked into an image N days ago is at least N days old, because it was already
// published when it was baked. So an image older than the grace period contains
// a runner past it, with no lookup and no ambiguity.
//
// That asymmetry is worth keeping rather than papering over. A caller that can
// reach GitHub should ALSO compare the manifest's runner version against the
// published release, which is the check that catches a fresh image built around
// an old runner. This one is the floor: it is always available, it never gives a
// false positive, and it is what stops a node importing something already dead
// on arrival when nothing else can be consulted.
func (m *Manifest) Stale(now time.Time) error {
	age := m.Age(now)

	if age < runnerrelease.Grace {
		return nil
	}

	return fmt.Errorf("imagesource: this image was built %d days ago, so the runner baked "+
		"into it is at least that old and github stops handing jobs to a runner more than "+
		"%d days past its release; importing it would produce microVMs that register and "+
		"are never given work",
		int(age.Hours()/24), int(runnerrelease.Grace.Hours()/24))
}

// Aging reports that the image is old enough to be worth rebuilding, without
// being past the point of uselessness.
//
// SEPARATE FROM Stale BECAUSE THE ACTIONS DIFFER: this is a thing to say, and
// Stale is a thing to refuse. Conflating them either blocks on an image that
// still works or stays silent until the fleet has already stopped.
func (m *Manifest) Aging(now time.Time) bool {
	age := m.Age(now)

	return age >= runnerrelease.Warn && age < runnerrelease.Grace
}
