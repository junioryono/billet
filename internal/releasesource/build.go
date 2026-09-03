package releasesource

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BuildRequest is everything a publisher knows that the files themselves do not.
type BuildRequest struct {
	// Dist is the directory GoReleaser wrote its artifacts into.
	Dist string

	Version string
	Commit  string
	BuiltAt time.Time

	Wire          Range
	LedgerSchema  int
	GuestContract string

	// RollbackTo is the release a failed update of this one restores. Empty for
	// the first release billet ever publishes.
	RollbackTo string
}

// Build assembles a manifest by hashing what was actually produced.
//
// IN GO RATHER THAN IN THE WORKFLOW, and that is the point. A manifest assembled
// by a shell heredoc is a second implementation of this schema, and the two drift
// on the first field anybody adds — the two-pins problem the vendored toolset
// declaration exists to prevent one directory over. Here the writer and the
// reader are the same type, and the result is put through the reader's own
// validation before it is published.
//
// THE DIGESTS COME FROM THE FILES, never from checksums.txt. Reading a digest out
// of a file GoReleaser wrote would make the manifest a restatement of another
// document's opinion; hashing the bytes that are about to be uploaded is the only
// thing that makes the signature over this manifest mean what it claims.
func Build(req BuildRequest) (*Manifest, error) {
	entries, err := os.ReadDir(req.Dist)
	if err != nil {
		return nil, fmt.Errorf("releasesource: read the dist directory: %w", err)
	}

	m := &Manifest{
		Schema:        SchemaVersion,
		Version:       strings.TrimSpace(req.Version),
		Commit:        strings.TrimSpace(req.Commit),
		BuiltAt:       req.BuiltAt.UTC(),
		Wire:          req.Wire,
		LedgerSchema:  req.LedgerSchema,
		GuestContract: strings.TrimSpace(req.GuestContract),
		// A RELEASE'S BUNDLED ACTIONS ARE ITS OWN TAG, because cut-release.yml
		// rewrites every sibling reference to the tag being cut before it tags.
		// Carried explicitly so a reader checks it rather than assuming it.
		Actions:    strings.TrimSpace(req.Version),
		RollbackTo: strings.TrimSpace(req.RollbackTo),
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		a, ok := describe(entry.Name())
		if !ok {
			continue
		}

		path := filepath.Join(req.Dist, entry.Name())

		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("releasesource: stat %s: %w", entry.Name(), err)
		}

		sum, err := fileDigest(path)
		if err != nil {
			return nil, err
		}

		a.Size = info.Size()
		a.SHA256 = sum
		m.Artifacts = append(m.Artifacts, a)
	}

	// SORTED, so two builds of the same tree produce byte-identical manifests.
	// Directory order is not stable, and a manifest whose bytes move for no
	// reason cannot be compared against a previous one by digest.
	sort.Slice(m.Artifacts, func(i, j int) bool {
		return m.Artifacts[i].Name < m.Artifacts[j].Name
	})

	// THROUGH THE READER'S OWN RULES BEFORE IT IS PUBLISHED, so a release cannot
	// ship a manifest its own fleet refuses. The alternative is discovering it
	// when every deployment stops being able to update.
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("releasesource: the manifest this build produced is one billet "+
			"would refuse: %w", err)
	}

	return m, nil
}

// describe reads a platform and a kind out of a GoReleaser artifact name.
//
// PINNED TO THE NAME TEMPLATE THE CONFIG SETS, which is already a contract rather
// than a preference: the install script derives the same name from uname, so
// changing it breaks every existing installer. An unrecognised file is SKIPPED
// rather than guessed at — checksums.txt and the metadata files live in the same
// directory, and a manifest that named them would have an updater try to install
// a list of hashes.
func describe(name string) (Artifact, bool) {
	var (
		kind string
		stem string
	)

	switch {
	case strings.HasSuffix(name, ".tar.gz"):
		kind, stem = KindArchive, strings.TrimSuffix(name, ".tar.gz")
	case strings.HasSuffix(name, ".zip"):
		kind, stem = KindArchive, strings.TrimSuffix(name, ".zip")
	case strings.HasSuffix(name, ".deb"):
		kind, stem = KindDeb, strings.TrimSuffix(name, ".deb")
	case strings.HasSuffix(name, ".rpm"):
		kind, stem = KindRPM, strings.TrimSuffix(name, ".rpm")
	default:
		return Artifact{}, false
	}

	// billet_0.4.0_linux_amd64 — project, version, os, arch.
	parts := strings.Split(stem, "_")
	if len(parts) < 4 {
		return Artifact{}, false
	}

	return Artifact{
		Name: name,
		OS:   parts[len(parts)-2],
		Arch: parts[len(parts)-1],
		Kind: kind,
	}, true
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("releasesource: open %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", fmt.Errorf("releasesource: hash %s: %w", path, err)
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// Marshal renders a manifest the way the publisher writes it.
//
// ONE RENDERER, for the reason ChannelStatement.Marshal is one: the bytes that
// get signed have to be the bytes the reader was written against. A publisher
// that serialised the document its own way would be signing something this
// package has never seen, and the difference would surface as an unverifiable
// signature on a release nobody had tampered with.
func (m *Manifest) Marshal() ([]byte, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("releasesource: render the manifest for %s: %w", m.Version, err)
	}

	return append(body, '\n'), nil
}
