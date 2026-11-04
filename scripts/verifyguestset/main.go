// Command verifyguestset proves what a carried guest image comes from, before it is
// published again (ADR-022), under billet's own signing policy, the one every node verifies
// with: a manifest against its bundle and the machine it is carried for, or the channel
// pointer that chose its release.
//
// IMAGESOURCE'S POLICY RATHER THAN COSIGN'S FLAGS, because a second statement of who may
// sign an image is a second thing to keep equal to the first, and a carried set that
// passes it and fails the node's would be published and pulled by nobody. The digests of
// what the manifest names are the publisher's to check, as they are for a set built in
// the same run.
//
// A POINTER'S EXPIRY IS NOT HELD HERE. A run after weeks of failed builds still takes its
// last good image from the pointer that named it, and an expired pointer is no less what
// this workflow signed; replaying an older one needs a write to the channel's branch, and
// it can only name a release this workflow published.
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

// run takes `manifest MACHINE FILE BUNDLE` or `channel FILE BUNDLE`.
func run(args []string, stdout io.Writer) error {
	switch {
	case len(args) == 4 && args[0] == "manifest":
		return verifyManifest(args[1], args[2], args[3], stdout)
	case len(args) == 3 && args[0] == "channel":
		return verifyChannel(args[1], args[2], stdout)
	default:
		return errors.New("usage: verifyguestset manifest MACHINE MANIFEST BUNDLE, or channel POINTER BUNDLE")
	}
}

// verified reads a file and its bundle and holds them to billet's policy, as a node does:
// nothing reads a document nothing has vouched for.
func verified(path, bundlePath string) ([]byte, imagesource.Policy, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the workflow names the file it just fetched.
	if err != nil {
		return nil, imagesource.Policy{}, fmt.Errorf("could not read the document: %w", err)
	}

	bundle, err := os.ReadFile(bundlePath) //nolint:gosec // G304: the workflow names the file it just fetched.
	if err != nil {
		return nil, imagesource.Policy{}, fmt.Errorf("could not read the bundle: %w", err)
	}

	policy, err := imagesource.PolicyFor(imagesource.DefaultSource(), "", "", false)
	if err != nil {
		return nil, imagesource.Policy{}, fmt.Errorf("could not state billet's signing policy: %w", err)
	}

	if err := imagesource.VerifySignature(raw, bundle, policy); err != nil {
		return nil, imagesource.Policy{}, err
	}

	return raw, policy, nil
}

func verifyManifest(machine, path, bundlePath string, stdout io.Writer) error {
	raw, policy, err := verified(path, bundlePath)
	if err != nil {
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

func verifyChannel(path, bundlePath string, stdout io.Writer) error {
	_, policy, err := verified(path, bundlePath)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(stdout, "verified: a channel pointer signed by %s\n", policy.Identity); err != nil {
		return fmt.Errorf("could not report the verdict: %w", err)
	}

	return nil
}
