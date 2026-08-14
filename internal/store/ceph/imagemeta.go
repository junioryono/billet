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
// The full key is this plus "." plus the generation.
const RunnerVersionKey = "billet.runner_version"

// RunnerVersion reports which actions/runner a specific generation carries.
//
// THE GENERATION, NOT THE HEAD, AND THAT DISTINCTION IS THE WHOLE POINT. Generations
// are immutable and promotion is deliberate, so a fleet can sit on last month's
// generation while the head advances every week. Reading the head would report the
// newest BUILD as though it were the fleet — saying everything is current while the
// guests that jobs actually boot age past the deadline, which is the outage this is
// meant to catch, hidden by the thing meant to catch it.
//
// A MISSING KEY IS NOT AN ERROR AND NOT A VERSION. A generation published before
// billet recorded this, or by hand, has nothing to say — and answering "" lets the
// caller tell that apart from an answer, which is the difference between "cannot
// tell" and "here is the number".
func (c *Client) RunnerVersion(ctx context.Context, image string) (string, bool, error) {
	name, generation, found := strings.Cut(strings.TrimSpace(image), "@")
	if strings.TrimSpace(name) == "" {
		return "", false, fmt.Errorf("ceph: no image to read %s from", RunnerVersionKey)
	}

	if !found || strings.TrimSpace(generation) == "" {
		// A bare image name is not something a tier can boot — the provider refuses
		// one — so there is no generation to answer about.
		return "", false, nil
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "get", name,
		RunnerVersionKey+"."+generation)
	if err != nil {
		// `rbd` exits 2 for a key that is not there, which is the ordinary answer for
		// a generation published before this was recorded rather than a failure to
		// read.
		//
		// NOT --format json, MEASURED. `rbd image-meta get` rejects it outright —
		// `unrecognised option '--format'`, exit 1 — unlike almost every other rbd
		// subcommand this package calls. Asking for it made every lookup fail, and
		// the caller's fallback swallowed that silently, so the wrong answer looked
		// exactly like a generation with no metadata.
		if isNoSuchFile(err) {
			// ENOENT MEANS TWO THINGS HERE AND THEY ARE NOT THE SAME. `rbd` exits 2
			// both for "that key is not set" — ordinary, for a generation published
			// before billet recorded this — and for "no such image or pool", which
			// is a deployment pointing at storage that is not there.
			//
			// Reporting the second as the first sends the caller down its "cannot
			// tell, use the compiled-in pin" path, so a mistyped pool reads as an
			// image with no metadata and the operator is told a version that
			// describes nothing. Asking whether the image exists separates them.
			if _, existsErr := c.rbdCmd(ctx, true, "-p", c.cfg.ImagePool, "info", name); existsErr != nil {
				return "", false, fmt.Errorf("ceph: %s/%s is not there to read a runner version "+
					"from: %w", c.cfg.ImagePool, name, existsErr)
			}

			return "", false, nil
		}

		return "", false, fmt.Errorf("ceph: read the runner version of %s/%s: %w",
			c.cfg.ImagePool, image, err)
	}

	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", false, nil
	}

	return version, true, nil
}
