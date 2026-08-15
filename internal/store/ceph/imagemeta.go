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

// KernelVersionKey records which kernel a generation is paired with.
//
// PER GENERATION, FOR THE SAME REASON THE RUNNER VERSION IS. The kernel and the
// root filesystem are a matched pair -- a guest booted with a different kernel
// fails in the middle of somebody's job -- and two generations of one image can
// want different kernels. An operator pointing one config at both has no other way
// to know which.
const KernelVersionKey = "billet.kernel_version"

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

// SetRunnerVersion records which actions/runner a generation carries.
//
// THE COUNTERPART TO RunnerVersion, KEYED THE SAME WAY. A single head-level key
// would describe the last thing published rather than what any job runs, and an
// alarm reading it reports the newest image as though it were the fleet — staying
// green right through the expiry it exists to catch.
//
// WRITTEN AFTER THE SNAPSHOT EXISTS, by every caller. The value describes a
// generation, so recording it against one that was never published leaves a key
// nothing will ever read and nothing will ever clean up.
func (c *Client) SetRunnerVersion(ctx context.Context, image, generation, version string) error {
	// THE VALIDATION LIVES IN setGenerationMeta, shared with every other
	// generation-keyed value, so it cannot drift between them: a key written against
	// a malformed generation is one nothing reads and nothing reaps, and that has to
	// be true of all of them or none.
	return c.setGenerationMeta(ctx, RunnerVersionKey, image, generation, version)
}

// setGenerationMeta writes one generation-keyed metadata value.
//
// SHARED BY EVERY CALLER, so the validation cannot drift between them: a key
// written against a malformed generation is one nothing reads and nothing reaps,
// and that has to be true of all of them or none.
func (c *Client) setGenerationMeta(
	ctx context.Context,
	key, image, generation, value string,
) error {
	if err := checkCloneName(image); err != nil {
		return err
	}

	if _, ok := ParseGeneration(generation); !ok {
		return fmt.Errorf("ceph: %q is not a generation billet published, so recording %s "+
			"against it would write a key nothing reads and nothing reaps", generation, key)
	}

	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("ceph: refusing to record an empty %s for %s@%s, which reads back "+
			"as though nothing was recorded", key, image, generation)
	}

	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "set", image,
		key+"."+generation, value); err != nil {
		return fmt.Errorf("ceph: could not record %s for %s@%s: %w", key, image, generation, err)
	}

	return nil
}

// SetKernelVersion records which kernel a generation is paired with.
func (c *Client) SetKernelVersion(ctx context.Context, image, generation, version string) error {
	return c.setGenerationMeta(ctx, KernelVersionKey, image, generation, version)
}

// KernelVersion reports which kernel a generation is paired with.
func (c *Client) KernelVersion(ctx context.Context, image, generation string) (string, bool, error) {
	if err := checkCloneName(image); err != nil {
		return "", false, err
	}

	if _, ok := ParseGeneration(generation); !ok {
		return "", false, fmt.Errorf("ceph: %q is not a generation billet published", generation)
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "get", image,
		KernelVersionKey+"."+generation)
	if err != nil {
		if isNoSuchFile(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("ceph: could not read %s for %s@%s: %w",
			KernelVersionKey, image, generation, err)
	}

	return strings.TrimSpace(string(out)), true, nil
}
