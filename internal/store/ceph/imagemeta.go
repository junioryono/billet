package ceph

import (
	"context"
	"fmt"
	"strings"
)

// RunnerVersionKey is where a published image records the actions/runner it carries.
//
// ON THE IMAGE BECAUSE THE IMAGE IS THE ONLY THING THAT KNOWS. billet compiles a
// pinned runner version into its own binary, and that says what a build WOULD install
// rather than what the running fleet HAS. The two part company the moment a scheduled
// rebuild takes up a newer release — and the question anyone actually asks is about
// the fleet: GitHub stops sending jobs to a runner more than thirty days behind a
// release, and it is the guests that are behind or not, never the binary.
const RunnerVersionKey = "billet.runner_version"

// RunnerVersion reports which actions/runner the published image carries.
//
// A MISSING KEY IS NOT AN ERROR AND NOT A VERSION. An image published before billet
// recorded this, or by hand, has nothing to say — and answering "" lets the caller
// tell that apart from "the image says 2.336.0", which is the distinction between
// "cannot tell" and "here is the answer". Reporting a guess here would put a number
// on a deadline that nobody checked.
func (c *Client) RunnerVersion(ctx context.Context, image string) (string, bool, error) {
	if strings.TrimSpace(image) == "" {
		return "", false, fmt.Errorf("ceph: no image to read %s from", RunnerVersionKey)
	}

	// THE HEAD, NOT A SNAPSHOT. Metadata belongs to the image rather than to any
	// snapshot of it, so the value describes the most recent publish — which is what
	// a new job will boot once a tier is pointed at it.
	name, _, _ := strings.Cut(image, "@")

	// NOT --format json, MEASURED. `rbd image-meta get` rejects it outright —
	// `unrecognised option '--format'`, exit 1 — unlike almost every other rbd
	// subcommand this package calls. Asking for it made every lookup fail, and the
	// caller's "cannot tell, fall back to the compiled-in pin" path swallowed that
	// silently, so the wrong answer looked exactly like an image with no metadata.
	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "get", name,
		RunnerVersionKey)
	if err != nil {
		// `rbd` exits 2 for a key that is not there, which is the ordinary answer for
		// an image published before this was recorded rather than a failure to read.
		if isNoSuchFile(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("ceph: read %s from %s/%s: %w",
			RunnerVersionKey, c.cfg.ImagePool, name, err)
	}

	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", false, nil
	}

	return version, true, nil
}
