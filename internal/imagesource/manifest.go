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
)

// The manifest layouts this build knows about.
//
// Bumped when a field changes meaning. A reader refuses a schema it does not
// know rather than interpreting unfamiliar fields, because the failure mode of
// guessing is booting the wrong bytes.
const (
	// SchemaV1 publishes the root filesystem as one release asset.
	SchemaV1 = 1

	// SchemaV2 publishes it as ordered parts, because GitHub caps a single
	// release asset at 2 GiB and a parity-sized image packs to well past that.
	SchemaV2 = 2
)

// SchemaVersion is the layout this build WRITES.
//
// DELIBERATELY BEHIND WHAT IT READS, and that gap is the whole migration. A
// reader accepts exactly the schemas it understands and refuses everything else,
// so publishing a new layout in the same change that teaches the reader about it
// is a FLAG DAY: the next release becomes unreadable to every already-deployed
// binary, and the thing that would fix them is the image they can no longer
// pull. So readers learn v2 and ship; only once the fleet carries them does the
// writer move. Anything that changes this constant must ask whether every
// deployment in the wild can already read what it is about to publish.
const SchemaVersion = SchemaV1

// readableSchemas is what this build will parse.
var readableSchemas = map[int]bool{
	SchemaV1: true,
	SchemaV2: true,
}

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
	// THE FIELD THAT MAKES THE THIRTY-DAY RULE ANSWERABLE. GitHub stops handing
	// jobs to a runner more than thirty days past the first release newer than it,
	// and this is the one fact about an image that lets anything ask: the importer
	// resolves it against the release history before downloading, and declines an
	// image whose runner is already refused. The build DATE cannot answer that
	// question in either direction, which is why nothing here computes from it.
	RunnerVersion string `json:"runner_version"`

	// BuiltAt is when the image was produced, in UTC.
	BuiltAt time.Time `json:"built_at"`

	// Rootfs is the guest's root filesystem, published as one asset.
	//
	// SCHEMA 1 ONLY. A schema 2 manifest carries RootfsMultipart instead and
	// leaves this zero; Validate refuses a document that sets both or neither,
	// because "which one describes the image" must never be a guess.
	// omitzero, NOT omitempty: omitempty has no effect on a struct, so a schema 2
	// manifest would publish an empty "rootfs" object beside its parts and invite
	// a reader to treat the two as both present.
	Rootfs Asset `json:"rootfs,omitzero"`

	// RootfsMultipart is the root filesystem published as ordered parts.
	//
	// SCHEMA 2 ONLY, and it exists because of a hard limit rather than a
	// preference: GitHub caps a single release asset at 2 GiB, and an image
	// carrying what a github-hosted runner carries packs to well past that. A
	// release may hold up to a thousand assets, so the file is split and put
	// back together on the way in.
	RootfsMultipart *Multipart `json:"rootfs_multipart,omitempty"`

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

// Multipart is one logical file published as several release assets.
//
// THE WHOLE-FILE DIGEST IS THE LOAD-BEARING CHECK, and the per-part digests do
// not replace it. Each part's digest proves that piece arrived as published; only
// the digest of the reassembled file proves the pieces were put back together in
// the right order and none was dropped or repeated. A reader that verified parts
// and skipped the whole would accept a correctly-signed manifest whose parts had
// been reordered into a different filesystem.
type Multipart struct {
	// Name is what the reassembled file is called once the parts are joined.
	//
	// A BARE FILENAME, validated as one, for the reason Asset.Name is: it is
	// joined to a staging directory to write.
	Name string `json:"name"`

	// SHA256 is the digest of the REASSEMBLED bytes, lowercase hex.
	SHA256 string `json:"sha256"`

	// Size is the reassembled length in bytes.
	Size int64 `json:"size"`

	// Compression names how the reassembled file is packed: "" or "zstd".
	//
	// ON THE WHOLE, NEVER ON A PART. The parts are byte ranges of one
	// compressed stream, not independently compressed files, so decompressing a
	// part on its own is meaningless and a part that claimed a compression would
	// be describing something that does not exist.
	Compression string `json:"compression,omitempty"`

	// Parts are the published pieces, in the order they concatenate.
	Parts []Asset `json:"parts"`
}

// MaxAssetBytes bounds a single published asset at 8 GiB.
//
// The root filesystem is four gigabytes raw and under half a gigabyte packed,
// so this is generous by an order of magnitude while still refusing a manifest
// whose size field would have a node write until the disk filled. GitHub caps a
// release asset at 2 GiB, so anything approaching this bound is a manifest that
// could not have been published by the pipeline that claims to have made it.
const MaxAssetBytes int64 = 8 << 30

// MaxPartBytes bounds one part of a multipart asset at GitHub's own limit.
//
// 2 GiB IS NOT A CHOICE HERE, it is what a release accepts: "Each file included
// in a release must be under 2 GiB." A manifest naming a larger part describes
// something the pipeline it claims to come from could not have uploaded, so it is
// refused before anything is downloaded rather than after the upload that would
// have failed.
const MaxPartBytes int64 = 2 << 30

// MaxRootfsBytes bounds the reassembled root filesystem at 128 GiB.
//
// The bound exists so a manifest cannot ask a node to write until its disk fills.
// It is well above a parity-sized image — tens of gigabytes packed — and well
// below anything a node could be expected to stage.
const MaxRootfsBytes int64 = 128 << 30

// MaxParts bounds how many pieces one file may be published in.
//
// A release holds at most a thousand assets, and a parity-sized image needs on
// the order of ten parts. The bound refuses a manifest that would turn one import
// into thousands of requests, which is a denial of service against the node
// rather than a plausible publication.
const MaxParts = 64

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

	if !readableSchemas[m.Schema] {
		return fmt.Errorf("imagesource: this build reads manifest schemas %d and %d, and the "+
			"published one is schema %d; upgrade billet rather than importing an image "+
			"it cannot describe", SchemaV1, SchemaV2, m.Schema)
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

	// EXACTLY ONE DESCRIPTION OF THE ROOT FILESYSTEM. Both set, or neither, would
	// leave "which one is the image" to whichever accessor a caller happened to
	// reach for -- and the two could describe different filesystems.
	multipart := m.RootfsMultipart != nil
	single := m.Rootfs != Asset{}

	// DISPATCHED BY SCHEMA IN BOTH DIRECTIONS. Refusing parts under schema 1 while
	// accepting a single asset under schema 2 makes the version number decorative:
	// a schema 1 document with its number changed to 2 would be accepted, and
	// "which layout is this" stops being answerable from the field that exists to
	// answer it.
	switch {
	case multipart && single:
		return fmt.Errorf("imagesource: the manifest describes the root filesystem twice, as a " +
			"single asset and as parts; nothing can tell which one is the image")
	case !multipart && !single:
		return fmt.Errorf("imagesource: the manifest describes no root filesystem")
	case multipart && m.Schema != SchemaV2:
		return fmt.Errorf("imagesource: a schema %d manifest published the root filesystem as "+
			"parts, which only schema %d describes", m.Schema, SchemaV2)
	case single && m.Schema != SchemaV1:
		return fmt.Errorf("imagesource: a schema %d manifest published the root filesystem as "+
			"one asset, which is the schema %d layout; a document must match the schema it "+
			"declares", m.Schema, SchemaV1)
	}

	if err := m.Kernel.validate("kernel"); err != nil {
		return err
	}

	if single {
		if err := m.Rootfs.validate("rootfs"); err != nil {
			return err
		}
	} else if err := m.RootfsMultipart.validate(); err != nil {
		return err
	}

	// EVERY STAGED NAME IS DISTINCT. They are written side by side in one
	// directory, so equal names would have the second overwrite the first and each
	// digest check would then pass against whichever landed last.
	//
	// EqualFold, NOT ==. The staging directory may be on a case-folding filesystem
	// -- APFS's default, and the machine a developer runs this on -- where
	// "ROOTFS" and "rootfs" are one file. A byte-wise comparison passes and the
	// second download silently replaces the first.
	//
	// THE ASSEMBLED NAME IS IN THIS SET TOO, not just the downloaded parts: it is
	// written into the same directory, so a part named after the file it
	// concatenates into would be overwritten mid-assembly.
	staged := make(map[string]string, len(m.stagedNames()))

	for _, name := range m.stagedNames() {
		key := strings.ToLower(name)
		if prior, dup := staged[key]; dup {
			return fmt.Errorf("imagesource: %q and %q are staged into one directory under names "+
				"that collide, and one would overwrite the other", prior, name)
		}

		staged[key] = name
	}

	return nil
}

// stagedNames is every file name this manifest causes to be written into the
// staging directory: the downloaded assets, the name parts are assembled into,
// and the name the assembled file DECOMPRESSES to.
//
// THE DECOMPRESSED NAME IS IN THIS SET BECAUSE IT IS A REAL WRITE, and leaving it
// out was a hole rather than an omission. The unpacker derives it by dropping a
// .zst suffix and runs `zstd -f`, which overwrites without asking — so a manifest
// naming the root filesystem "vmlinux-billet.zst" and the kernel "vmlinux-billet"
// passes a check over the published names, and then the decompression writes the
// root filesystem over the already-verified kernel. Nothing downstream re-hashes
// the kernel before installing it, so the guest would boot a kernel that is
// actually a compressed filesystem.
//
// That is reachable by an ordinary naming choice, not only by a hostile one.
func (m *Manifest) stagedNames() []string {
	img := m.RootfsImage()

	names := []string{m.Kernel.Name, img.Name}

	if unpacked := img.UnpackedName(); unpacked != img.Name {
		names = append(names, unpacked)
	}

	// A SINGLE-ASSET IMAGE IS ITS OWN ONLY PART, legitimately: the downloaded file
	// IS the assembled file, so listing that name twice would refuse every valid
	// schema 1 manifest. That exemption is exactly this shape and nothing wider --
	// a multipart image with a part named after the file its parts join into is a
	// real collision, and the part would be overwritten mid-assembly.
	if len(img.Parts) == 1 && img.Parts[0].Name == img.Name {
		return names
	}

	for _, p := range img.Parts {
		names = append(names, p.Name)
	}

	return names
}

// UnpackedName is what the assembled file is called once it is decompressed.
//
// DEFINED HERE RATHER THAN AT THE CALL SITE so validation and the unpacker cannot
// disagree about it. They did: the name was derived in cmd/billet and checked
// nowhere, which is what let a decompression land on another asset.
func (mp *Multipart) UnpackedName() string {
	if mp.Compression == "" {
		return mp.Name
	}

	if trimmed, ok := strings.CutSuffix(mp.Name, ".zst"); ok {
		return trimmed
	}

	return mp.Name + ".raw"
}

func (mp *Multipart) validate() error {
	if !namePattern.MatchString(mp.Name) {
		return fmt.Errorf("imagesource: the root filesystem assembles into %q, which is not a "+
			"plain file name; it would be written somewhere nobody chose", mp.Name)
	}

	if !digestPattern.MatchString(mp.SHA256) {
		return fmt.Errorf("imagesource: the root filesystem carries %q where the digest of the "+
			"reassembled file belongs, and that digest is the only thing that proves the parts "+
			"were joined in the published order", mp.SHA256)
	}

	if !knownCompression[mp.Compression] {
		return fmt.Errorf("imagesource: the root filesystem is packed with %q, which this build "+
			"cannot unpack; refusing rather than treating it as uncompressed", mp.Compression)
	}

	switch {
	case len(mp.Parts) == 0:
		return fmt.Errorf("imagesource: the root filesystem is published as parts and none " +
			"are listed")
	case len(mp.Parts) > MaxParts:
		return fmt.Errorf("imagesource: the root filesystem is published in %d parts, past the "+
			"%d this reads; refusing rather than turning one import into that many requests",
			len(mp.Parts), MaxParts)
	}

	if mp.Size <= 0 {
		return fmt.Errorf("imagesource: the reassembled root filesystem is published with a "+
			"size of %d bytes", mp.Size)
	}

	if mp.Size > MaxRootfsBytes {
		return fmt.Errorf("imagesource: the reassembled root filesystem is published as %d "+
			"bytes, past the %d-byte bound this reads; refusing rather than staging until "+
			"something fills", mp.Size, MaxRootfsBytes)
	}

	// THE SIZES MUST ADD UP, AND THIS IS CHECKED BEFORE ANYTHING IS DOWNLOADED.
	// The whole-file digest catches a reordered or truncated set too, but only
	// after every byte has been fetched and joined. Summing the published lengths
	// costs nothing and refuses that manifest up front.
	var total int64

	for i := range mp.Parts {
		part := &mp.Parts[i]

		if err := part.validatePart(i); err != nil {
			return err
		}

		// OVERFLOW IS CHECKED, not assumed away. Sizes come from the network, and
		// int64 addition wraps; a wrapped total that happened to equal Size would
		// pass this check on its way to a download nobody bounded.
		if total > mp.Size-part.Size {
			return fmt.Errorf("imagesource: the root filesystem's parts add up to more than the "+
				"%d bytes the reassembled file is published as", mp.Size)
		}

		total += part.Size
	}

	if total != mp.Size {
		return fmt.Errorf("imagesource: the root filesystem's %d parts add up to %d bytes and "+
			"the reassembled file is published as %d; a part is missing or the manifest "+
			"describes a different file", len(mp.Parts), total, mp.Size)
	}

	return nil
}

// validatePart is Asset.validate with the tighter bound a release actually
// enforces, and with compression refused rather than merely checked.
func (a *Asset) validatePart(i int) error {
	field := fmt.Sprintf("root filesystem part %d", i+1)

	if err := a.validate(field); err != nil {
		return err
	}

	if a.Size > MaxPartBytes {
		return fmt.Errorf("imagesource: %s is published as %d bytes and a release asset must be "+
			"under %d; no pipeline could have uploaded it", field, a.Size, MaxPartBytes)
	}

	// A PART IS A BYTE RANGE, NOT A FILE. Compression belongs to the reassembled
	// stream, so a part claiming its own would be describing something that does
	// not exist -- and a reader acting on it would decompress a fragment.
	if a.Compression != "" {
		return fmt.Errorf("imagesource: %s claims compression %q; parts are byte ranges of one "+
			"stream and only the reassembled file is packed", field, a.Compression)
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
// HAND-MAINTAINED, AND A STRUCT TAG ALONE DOES NOT ADD TO IT. A field added to
// Manifest without a line here is refused by its own reader, which is what
// happened when rootfs_multipart arrived; TestKnownKeysCoversEveryStructTag
// exists so the next one is caught by a test rather than by a puzzling refusal.
var knownKeys = map[string]bool{
	"schema": true, "guest_contract": true, "arch": true,
	"runner_version": true, "built_at": true, "rootfs": true, "kernel": true,
	"rootfs_multipart": true, "parts": true,
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

// AgeNotice is when an image is old enough to be worth mentioning.
//
// ABOUT BILLET'S BUILD CADENCE, NOT ABOUT GITHUB, and the distinction is the whole
// reason this constant exists rather than reusing runnerrelease's window. The guest
// image is rebuilt weekly, so an image a fortnight old means two builds did not
// happen or a node stopped refreshing — worth saying, and evidence of nothing else.
const AgeNotice = 14 * 24 * time.Hour

// Age is how long ago the image was built, as of now.
func (m *Manifest) Age(now time.Time) time.Duration { return now.Sub(m.BuiltAt) }

// Aging reports that the image is old enough to be worth mentioning.
//
// MAINTENANCE INFORMATION, AND NOTHING MORE. There used to be a Stale() beside this
// that REFUSED an import at built_at + 30 days, on the reasoning that a runner baked
// N days ago is at least N days old. The arithmetic is true and the conclusion does
// not follow: GitHub's window opens when the first release NEWER than the baked one
// appears, so an image built the day a release shipped is still current a year later
// if nothing else ships, and an image built yesterday around a runner three releases
// behind is already refused. It rejected images that worked and accepted images that
// could not, from a number that was never about GitHub at all.
//
// What settles acceptance is the release history, which needs a network call this
// package deliberately does not make — so the caller asks runnerrelease and is
// honest when it cannot reach it. Age stays as what it always was: a fact about the
// artifact.
func (m *Manifest) Aging(now time.Time) bool { return m.Age(now) >= AgeNotice }

// RootfsImage describes the root filesystem in ONE shape, whichever schema
// published it.
//
// THIS IS WHY THE AGGREGATE CANNOT BE SKIPPED. A schema 1 manifest is normalized
// into a single-part multipart whose whole-file digest is that one asset's
// digest, so every consumer runs the same download-then-assemble-then-verify path
// and there is no second, shorter route that stops after the parts. Adding a
// parts field without this left the old per-asset path in place beside the new
// one, and the old path is exactly the one that verifies pieces and never checks
// that they were joined correctly.
//
// The returned value is a copy: Parts aliases nothing the caller can use to
// change what a later verification hashes.
func (m *Manifest) RootfsImage() Multipart {
	if m.RootfsMultipart != nil {
		out := *m.RootfsMultipart
		out.Parts = make([]Asset, len(m.RootfsMultipart.Parts))
		copy(out.Parts, m.RootfsMultipart.Parts)

		return out
	}

	return Multipart{
		Name:        m.Rootfs.Name,
		SHA256:      m.Rootfs.SHA256,
		Size:        m.Rootfs.Size,
		Compression: m.Rootfs.Compression,
		Parts:       []Asset{m.Rootfs},
	}
}

// Downloads is every asset a node must fetch, in the order it should fetch them.
//
// THE KERNEL LAST, because it is small and the root filesystem is what decides
// whether the import is worth continuing at all.
func (m *Manifest) Downloads() []Asset {
	img := m.RootfsImage()

	return append(img.Parts, m.Kernel)
}

// Assembled reports whether this image arrives as more than one file.
//
// Used only to decide what to tell an operator; nothing branches on it, because
// both shapes take the same path.
func (mp *Multipart) Assembled() bool { return len(mp.Parts) > 1 }
