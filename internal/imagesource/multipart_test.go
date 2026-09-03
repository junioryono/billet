package imagesource

import (
	"reflect"
	"strings"
	"testing"
)

// TestKnownKeysCoversEveryStructTag is the guard the rootfs_multipart bug needed.
//
// knownKeys is hand-maintained and the strict reader refuses anything absent from
// it, so a field added to Manifest without a matching line makes the reader reject
// its own output — with a message about a misspelled key, which sends the reader
// looking at the publisher rather than at this map. Reflection over the tags turns
// that into a failing test naming the missing key.
func TestKnownKeysCoversEveryStructTag(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[Manifest](),
		reflect.TypeFor[Asset](),
		reflect.TypeFor[Multipart](),
	} {
		for field := range typ.Fields() {
			tag := field.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}

			key, _, _ := strings.Cut(tag, ",")
			if key == "" {
				continue
			}

			if !knownKeys[key] {
				t.Errorf("%s.%s is published as %q and knownKeys does not list it, so the "+
					"strict reader refuses a manifest carrying billet's own field",
					typ.Name(), field.Name, key)
			}
		}
	}
}

// validMultipartManifest is a schema 2 document: the root filesystem published as
// three parts that concatenate into one 3000-byte file.
func validMultipartManifest() Manifest {
	m := validManifest()
	m.Schema = SchemaV2
	m.Rootfs = Asset{}
	m.RootfsMultipart = &Multipart{
		Name:        "rootfs.img.zst",
		SHA256:      strings.Repeat("c", 64),
		Size:        3000,
		Compression: "zstd",
		Parts: []Asset{
			{Name: "rootfs.img.zst.part00", SHA256: strings.Repeat("1", 64), Size: 1000},
			{Name: "rootfs.img.zst.part01", SHA256: strings.Repeat("2", 64), Size: 1000},
			{Name: "rootfs.img.zst.part02", SHA256: strings.Repeat("3", 64), Size: 1000},
		},
	}

	return m
}

func TestAMultipartManifestParses(t *testing.T) {
	got, err := ParseManifest(mustJSON(t, validMultipartManifest()))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	if got.RootfsMultipart == nil {
		t.Fatal("the parsed manifest carries no multipart root filesystem")
	}

	if n := len(got.RootfsMultipart.Parts); n != 3 {
		t.Errorf("parsed %d parts, want 3", n)
	}
}

// TestTheRootFilesystemIsDescribedExactlyOnce is the property that keeps "which
// bytes are the image" from being a guess.
func TestTheRootFilesystemIsDescribedExactlyOnce(t *testing.T) {
	t.Run("both", func(t *testing.T) {
		m := validMultipartManifest()
		m.Rootfs = validManifest().Rootfs

		requireRefused(t, m, "twice")
	})

	t.Run("neither", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart = nil

		requireRefused(t, m, "no root filesystem")
	})

	t.Run("parts under schema 1", func(t *testing.T) {
		m := validMultipartManifest()
		m.Schema = SchemaV1

		requireRefused(t, m, "only schema 2 describes")
	})
}

// TestThePublishedSizesMustAddUp catches a dropped, repeated or substituted part
// before a single byte is downloaded.
func TestThePublishedSizesMustAddUp(t *testing.T) {
	t.Run("a part is missing", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Parts = m.RootfsMultipart.Parts[:2]

		requireRefused(t, m, "add up to 2000")
	})

	t.Run("a part is too large", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Parts[0].Size = 2000

		requireRefused(t, m, "more than the 3000 bytes")
	})

	// A part whose own size exceeds the published whole is refused, and this is
	// the reachable form of the running-total check.
	t.Run("a part is larger than the whole", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Parts[0].Size = 4000

		requireRefused(t, m, "more than the 3000 bytes")
	})
}

// TestTheSizeBoundsCannotOverflowAnInt64 is why the running total in validate can
// be trusted, and it is an assertion about the CONSTANTS rather than about a
// document.
//
// The running total is written to avoid wrapping, but the reason it never has to
// is that the per-part and per-count bounds multiply to something an int64 holds
// comfortably. That is a relationship between three constants, and nothing stops
// a later change from raising one of them past it -- at which point the overflow
// branch stops being unreachable defence and becomes live code that no document
// in the test suite can reach. Assert the relationship instead of writing a test
// for a state the other bounds make impossible.
func TestTheSizeBoundsCannotOverflowAnInt64(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)

	if MaxPartBytes > maxInt64/int64(MaxParts) {
		t.Fatalf("%d parts of %d bytes can overflow an int64; the running total in "+
			"Multipart.validate is no longer sufficient", MaxParts, MaxPartBytes)
	}

	if got := MaxPartBytes * int64(MaxParts); got < MaxRootfsBytes {
		t.Errorf("the parts can hold at most %d bytes and a root filesystem may be published "+
			"as %d; a legitimate image could not be split into %d parts",
			got, MaxRootfsBytes, MaxParts)
	}
}

func TestPartBoundsAreGitHubsOwnLimits(t *testing.T) {
	t.Run("a part past 2 GiB could not have been uploaded", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Size = MaxPartBytes + 1
		m.RootfsMultipart.Parts = []Asset{
			{Name: "only.part", SHA256: strings.Repeat("1", 64), Size: MaxPartBytes + 1},
		}

		requireRefused(t, m, "must be under")
	})

	t.Run("no parts", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Parts = nil

		requireRefused(t, m, "none are listed")
	})

	t.Run("too many parts", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Parts = make([]Asset, MaxParts+1)

		for i := range m.RootfsMultipart.Parts {
			m.RootfsMultipart.Parts[i] = Asset{
				Name:   "part-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
				SHA256: strings.Repeat("1", 64),
				Size:   1,
			}
		}

		m.RootfsMultipart.Size = int64(MaxParts + 1)

		requireRefused(t, m, "past the 64 this reads")
	})

	// A PART IS A BYTE RANGE OF ONE STREAM, so a part that claims its own
	// compression describes something that does not exist, and a reader acting on
	// it would decompress a fragment.
	t.Run("a part claims compression", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Parts[1].Compression = "zstd"

		requireRefused(t, m, "byte ranges of one stream")
	})
}

// TestEveryStagedNameIsDistinct covers the collision the single-asset check
// already covered, extended to the names multipart adds -- including the
// assembled file, which is written into the same directory as its own parts.
func TestEveryStagedNameIsDistinct(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(m *Manifest)
	}{
		{"a part is named after the kernel", func(m *Manifest) {
			m.RootfsMultipart.Parts[0].Name = m.Kernel.Name
		}},
		{"a part is named after the assembled file", func(m *Manifest) {
			m.RootfsMultipart.Parts[2].Name = m.RootfsMultipart.Name
		}},
		{"two parts share a name", func(m *Manifest) {
			m.RootfsMultipart.Parts[1].Name = m.RootfsMultipart.Parts[0].Name
		}},
		// APFS IS CASE-FOLDING BY DEFAULT, so a byte-wise comparison passes here
		// and the second download silently replaces the first.
		{"two parts differ only in case", func(m *Manifest) {
			m.RootfsMultipart.Parts[1].Name = strings.ToUpper(m.RootfsMultipart.Parts[0].Name)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validMultipartManifest()
			tc.mutate(&m)

			requireRefused(t, m, "overwrite")
		})
	}
}

// TestRootfsImageNormalizesBothSchemas is what makes the whole-file check
// impossible to skip: there is one shape and therefore one code path.
func TestRootfsImageNormalizesBothSchemas(t *testing.T) {
	t.Run("schema 1 becomes a single part", func(t *testing.T) {
		m := validManifest()
		img := m.RootfsImage()

		switch {
		case len(img.Parts) != 1:
			t.Fatalf("a single-asset image normalized to %d parts", len(img.Parts))
		case img.SHA256 != m.Rootfs.SHA256:
			t.Errorf("the whole-file digest is %q and the asset's is %q; they must be the "+
				"same fact for the aggregate check to mean anything on schema 1",
				img.SHA256, m.Rootfs.SHA256)
		case img.Name != m.Rootfs.Name:
			t.Errorf("normalized name %q, want %q", img.Name, m.Rootfs.Name)
		case img.Size != m.Rootfs.Size:
			t.Errorf("normalized size %d, want %d", img.Size, m.Rootfs.Size)
		case img.Compression != m.Rootfs.Compression:
			t.Errorf("normalized compression %q, want %q", img.Compression, m.Rootfs.Compression)
		case img.Assembled():
			t.Error("a single-asset image reports itself as assembled from parts")
		}
	})

	t.Run("schema 2 keeps its parts", func(t *testing.T) {
		m := validMultipartManifest()
		img := m.RootfsImage()

		if len(img.Parts) != 3 || !img.Assembled() {
			t.Fatalf("a three-part image normalized to %d parts, assembled=%v",
				len(img.Parts), img.Assembled())
		}

		if img.SHA256 != m.RootfsMultipart.SHA256 {
			t.Error("the whole-file digest was not carried through normalization")
		}
	})

	// THE RETURNED PARTS MUST NOT ALIAS THE MANIFEST. A caller that sliced into
	// them could change what a later verification hashes.
	t.Run("the parts are a copy", func(t *testing.T) {
		m := validMultipartManifest()

		img := m.RootfsImage()
		img.Parts[0].SHA256 = strings.Repeat("f", 64)

		if m.RootfsMultipart.Parts[0].SHA256 == strings.Repeat("f", 64) {
			t.Error("RootfsImage handed out the manifest's own parts, so a caller can change " +
				"the digest a later verification checks against")
		}
	})
}

func TestDownloadsCoversEveryPartAndTheKernel(t *testing.T) {
	m := validMultipartManifest()

	got := m.Downloads()
	if len(got) != 4 {
		t.Fatalf("Downloads returned %d assets, want 3 parts and the kernel", len(got))
	}

	if got[len(got)-1].Name != m.Kernel.Name {
		t.Error("the kernel is not last; it is the small one and the root filesystem is " +
			"what decides whether the import is worth continuing")
	}

	for i, part := range m.RootfsMultipart.Parts {
		if got[i].Name != part.Name {
			t.Errorf("download %d is %q, want %q; the order parts concatenate in is the "+
				"order they must be fetched and joined", i, got[i].Name, part.Name)
		}
	}
}

// TestTheDecompressedNameCannotLandOnAnotherAsset is the hole the first version
// of the staged-name check left open.
//
// THE UNPACKER DROPS A .zst SUFFIX AND RUNS `zstd -f`, which overwrites without
// asking. A manifest naming the root filesystem "vmlinux-billet.zst" and the
// kernel "vmlinux-billet" passes a check over the PUBLISHED names -- they are
// different strings -- and then the decompression writes the root filesystem over
// the already-verified kernel, which nothing re-hashes before installing. The
// guest then boots a "kernel" that is a compressed filesystem.
//
// This is reachable by an ordinary naming choice, not only a hostile one.
func TestTheDecompressedNameCannotLandOnAnotherAsset(t *testing.T) {
	t.Run("over the kernel", func(t *testing.T) {
		m := validManifest()
		m.Rootfs.Name = "vmlinux-billet.zst"
		m.Rootfs.Compression = "zstd"
		m.Kernel.Name = "vmlinux-billet"

		requireRefused(t, m, "overwrite")
	})

	t.Run("over a part", func(t *testing.T) {
		m := validMultipartManifest()
		m.RootfsMultipart.Name = "image.zst"
		m.RootfsMultipart.Parts[0].Name = "image"

		requireRefused(t, m, "overwrite")
	})

	// AND THE ORDINARY CASE STILL PASSES. A check that refused every manifest
	// would satisfy the tests above and break every release.
	t.Run("the ordinary names are fine", func(t *testing.T) {
		if _, err := ParseManifest(mustJSON(t, validManifest())); err != nil {
			t.Fatalf("the ordinary schema 1 manifest was refused: %v", err)
		}

		if _, err := ParseManifest(mustJSON(t, validMultipartManifest())); err != nil {
			t.Fatalf("the ordinary schema 2 manifest was refused: %v", err)
		}
	})
}

// TestASchemaMustMatchItsLayoutInBothDirections. Refusing parts under schema 1
// while accepting a single asset under schema 2 makes the version number
// decorative: a schema 1 document with its number changed to 2 is accepted, and
// "which layout is this" stops being answerable from the field that says so.
func TestASchemaMustMatchItsLayoutInBothDirections(t *testing.T) {
	m := validManifest()
	m.Schema = SchemaV2

	requireRefused(t, m, "must match the schema it declares")
}

func TestUnpackedNameFollowsTheCompression(t *testing.T) {
	for _, tc := range []struct {
		name, compression, want string
	}{
		{"rootfs.img.zst", "zstd", "rootfs.img"},
		{"rootfs.img", "", "rootfs.img"},
		// PACKED BUT NOT SUFFIXED: the unpacker cannot drop anything, so it must
		// choose a name that is still distinct from the packed one.
		{"rootfs.img", "zstd", "rootfs.img.raw"},
	} {
		t.Run(tc.name+"/"+tc.compression, func(t *testing.T) {
			mp := Multipart{Name: tc.name, Compression: tc.compression}

			if got := mp.UnpackedName(); got != tc.want {
				t.Errorf("UnpackedName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func requireRefused(t *testing.T, m Manifest, want string) {
	t.Helper()

	_, err := ParseManifest(mustJSON(t, m))
	if err == nil {
		t.Fatal("the manifest was accepted")
	}

	if !strings.Contains(err.Error(), want) {
		t.Errorf("refused with %q, which does not mention %q; the message is what an "+
			"operator acts on", err, want)
	}
}
