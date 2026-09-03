// Command mkreleasemanifest writes the signed-release manifest for one tag.
//
// A GO PROGRAM RATHER THAN A SHELL SCRIPT, and not for taste. The manifest's
// schema is a contract between a publisher and every deployment that will ever
// read it, and a document assembled by jq is a second implementation of that
// schema — one that agrees today and drifts on the first field anybody adds. Here
// the writer imports the reader's own type and puts its output through the
// reader's own validation before anything is published, so a release cannot ship
// a manifest its own fleet would refuse.
//
// IT IS NOT PART OF THE BILLET BINARY. It runs once, in the release workflow,
// against the directory GoReleaser just wrote.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/state"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mkreleasemanifest: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dist := flag.String("dist", "dist", "the directory GoReleaser wrote its artifacts into")
	version := flag.String("version", "", "the release tag, e.g. v0.4.0")
	commit := flag.String("commit", "", "the commit the release was built from")
	rollbackTo := flag.String("rollback-to", "",
		"the release a failed update of this one restores; empty for the first release")
	out := flag.String("out", "", "where to write the manifest")

	flag.Parse()

	if *version == "" || *commit == "" || *out == "" {
		return fmt.Errorf("--version, --commit and --out are required")
	}

	// EVERY COMPATIBILITY FACT IS READ FROM THE BUILD ITSELF, never passed in.
	//
	// This is the whole reason the generator is a Go program. A workflow that
	// typed the wire range or the schema number into a flag would be asserting
	// something about the binary from outside it, and the first release where
	// somebody bumped one and forgot the other would publish a manifest that
	// describes a build nobody made — which then fails closed on every deployment
	// in the field, for a reason no diagnostic could explain.
	m, err := releasesource.Build(releasesource.BuildRequest{
		Dist:          *dist,
		Version:       *version,
		Commit:        *commit,
		BuiltAt:       time.Now().UTC(),
		Wire:          releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
		LedgerSchema:  state.LatestSchemaVersion(),
		GuestContract: firecracker.GuestContract,
		RollbackTo:    *rollbackTo,
	})
	if err != nil {
		return err
	}

	// THE PACKAGE'S OWN RENDERER, not a second serialisation here. The bytes
	// that get signed have to be the bytes the reader was written against.
	body, err := m.Marshal()
	if err != nil {
		return err
	}

	if err := os.WriteFile(*out, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}

	fmt.Printf("wrote %s for %s: %d artifact(s), wire %d-%d, ledger schema %d, guest %s\n",
		*out, m.Version, len(m.Artifacts), m.Wire.Min, m.Wire.Max, m.LedgerSchema,
		m.GuestContract)

	return nil
}
