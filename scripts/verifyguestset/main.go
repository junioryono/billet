// Command verifyguestset proves a guest manifest carried from an earlier release before
// that manifest is published again: it holds the manifest to its bundle under billet's
// own signing policy, the one every node verifies with, and to the machine it is carried
// for (ADR-022).
//
// IMAGESOURCE'S POLICY RATHER THAN COSIGN'S FLAGS, because a second statement of who may
// sign an image is a second thing to keep equal to the first, and a carried set that
// passes it and fails the node's would be published and pulled by nobody. The digests of
// what the manifest names are the publisher's to check, as they are for a set built in
// the same run.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/junioryono/billet/internal/imagesource"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "verifyguestset:", err)
		os.Exit(1)
	}
}

// run takes the machine the set is carried for, the manifest and its bundle.
func run(args []string, stdout io.Writer) error {
	if len(args) != 3 {
		return errors.New("usage: verifyguestset MACHINE MANIFEST BUNDLE")
	}

	machine, manifestPath, bundlePath := args[0], args[1], args[2]

	raw, err := os.ReadFile(manifestPath) //nolint:gosec // G304: the workflow names the file it just downloaded.
	if err != nil {
		return fmt.Errorf("could not read the manifest: %w", err)
	}

	bundle, err := os.ReadFile(bundlePath) //nolint:gosec // G304: the workflow names the file it just downloaded.
	if err != nil {
		return fmt.Errorf("could not read the bundle: %w", err)
	}

	policy, err := imagesource.PolicyFor(imagesource.DefaultSource(), "", "", false)
	if err != nil {
		return fmt.Errorf("could not state billet's signing policy: %w", err)
	}

	// VERIFIED BEFORE IT IS PARSED, as a node does: nothing reads a manifest nothing has
	// vouched for.
	if err := imagesource.VerifySignature(raw, bundle, policy); err != nil {
		return err
	}

	manifest, err := imagesource.ParseManifest(raw)
	if err != nil {
		return err
	}

	if manifest.Arch != machine {
		return fmt.Errorf("the manifest describes a %s image, and it is carried for %s", manifest.Arch, machine)
	}

	if _, err := fmt.Fprintf(stdout, "verified: a %s image signed by %s\n", manifest.Arch, policy.Identity); err != nil {
		return fmt.Errorf("could not report the verdict: %w", err)
	}

	return nil
}
