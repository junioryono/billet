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
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
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

	if strings.TrimSpace(m.GuestContract) == "" {
		return fmt.Errorf("imagesource: the manifest names no guest contract, so nothing " +
			"can tell whether this image's agent speaks to this billet")
	}

	if !archPattern.MatchString(m.Arch) {
		return fmt.Errorf("imagesource: %q is not an architecture name", m.Arch)
	}

	if strings.TrimSpace(m.RunnerVersion) == "" {
		return fmt.Errorf("imagesource: the manifest names no runner version, so nothing " +
			"can tell whether the baked runner is still inside github's thirty days")
	}

	if m.BuiltAt.IsZero() {
		return fmt.Errorf("imagesource: the manifest carries no build time")
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
	if m.Rootfs.Name == m.Kernel.Name {
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

	dec := json.NewDecoder(strings.NewReader(string(data)))

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

	if dec.More() {
		return nil, fmt.Errorf("imagesource: the manifest is followed by more content, so it is " +
			"not a single document and this refuses to guess which half is authoritative")
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}

	return &m, nil
}

// Usable reports whether this build can import the image the manifest names.
//
// SEPARATE FROM Validate, because the two answer different questions and only
// one of them means the publisher did something wrong. Validate asks whether the
// document is well formed; this asks whether THIS billet, on THIS machine, can
// use what it describes. A well-formed manifest for another architecture is not
// a defect to report to anyone — it is simply not for this host.
func (m *Manifest) Usable(contract, arch string) error {
	if m.GuestContract != contract {
		return fmt.Errorf("imagesource: this image's agent speaks guest contract %s and this "+
			"billet speaks %s; importing it would produce microVMs that boot and never report",
			m.GuestContract, contract)
	}

	if m.Arch != arch {
		return fmt.Errorf("imagesource: this image was built for %s and this host is %s",
			m.Arch, arch)
	}

	return nil
}
