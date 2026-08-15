package ceph

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// VerifiedKey records that a generation was proved to work. The full key is this
// plus "." plus the generation.
//
// IN THE CLUSTER, BECAUSE PROMOTION IS A FLEET-WIDE FACT. The alternative is a
// config file naming a generation, edited on every node and restarted — which is why
// nothing was ever promoted in practice: the schedule published a verified image
// every week and the fleet went on booting whatever somebody last typed.
const VerifiedKey = "billet.verified"

// Verified is the generation reference a tier can name to mean "the newest one that
// was proved to work".
//
// A WORD RATHER THAN AN IMPLICIT DEFAULT. A bare image name stays refused: choosing
// a generation for somebody who did not choose one is how a job silently boots
// something nobody decided on. Naming `@verified` IS the decision — it says "whatever
// passed, most recently" — and a tier that wants one exact generation still pins it
// and gets it forever.
const Verified = "verified"

// MarkVerified records that a generation booted, registered and ran a container.
//
// THE VALUE IS WHEN, so an operator reading the metadata can tell a generation
// verified an hour ago from one verified in March.
func (c *Client) MarkVerified(ctx context.Context, image string, at time.Time) error {
	name, generation, found := strings.Cut(strings.TrimSpace(image), "@")
	if !found || strings.TrimSpace(generation) == "" {
		return fmt.Errorf("ceph: %q names no generation to mark verified", image)
	}

	if _, ok := ParseGeneration(generation); !ok {
		return fmt.Errorf("ceph: %q is not a generation billet published, so marking it "+
			"verified would put a claim on something nothing can resolve", generation)
	}

	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "set", name,
		VerifiedKey+"."+generation, at.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("ceph: record %s as verified: %w", image, err)
	}

	return nil
}

// UnmarkVerified withdraws that claim, which is how a fleet is rolled back.
//
// IDEMPOTENT, because the thing an operator does in a hurry should not fail for
// having already worked.
func (c *Client) UnmarkVerified(ctx context.Context, image string) error {
	name, generation, found := strings.Cut(strings.TrimSpace(image), "@")
	if !found || strings.TrimSpace(generation) == "" {
		return fmt.Errorf("ceph: %q names no generation to unmark", image)
	}

	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "remove", name,
		VerifiedKey+"."+generation); err != nil {
		if isNoSuchFile(err) {
			return nil
		}

		return fmt.Errorf("ceph: withdraw the verification of %s: %w", image, err)
	}

	return nil
}

// NewestVerified resolves an image to the most recent generation that passed.
//
// BY BUILD TIME, NOT BY WHEN IT WAS VERIFIED. Re-verifying an older generation — the
// obvious thing to do while investigating a bad one — would otherwise promote it over
// a newer one that has been serving jobs for a week.
//
// NOT FOUND IS NOT AN ERROR here, and the caller must not treat it as one: a fleet
// whose newest generations have all failed verification has nothing to resolve to,
// and the honest answer is to say so rather than to fall back to something unproven.
func (c *Client) NewestVerified(ctx context.Context, image string) (Generation, bool, error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return Generation{}, false, fmt.Errorf("ceph: no image to resolve")
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "list", name)
	if err != nil {
		if isNoSuchFile(err) {
			return Generation{}, false, nil
		}

		return Generation{}, false, fmt.Errorf("ceph: read the verifications of %s/%s: %w",
			c.cfg.ImagePool, name, err)
	}

	// INTERSECTED WITH THE SNAPSHOTS THAT ACTUALLY EXIST.
	//
	// A verification key is not proof the generation is still there. A reap that
	// removed a snapshot and failed to remove its key, or one that raced a verify,
	// leaves a key describing nothing -- and resolving `@verified` from metadata
	// alone then points every launch at a generation that is gone. The clone fails,
	// and the message is about a missing snapshot rather than about the alias that
	// chose it.
	existing, err := c.Generations(ctx, name)
	if err != nil {
		return Generation{}, false, err
	}

	live := make(map[string]bool, len(existing))
	for _, gen := range existing {
		live[gen.Name] = true
	}

	var (
		newest Generation
		found  bool
	)

	// THE TABLE IS PARSED RATHER THAN ASKED PER KEY, because the generations are not
	// known in advance. `image-meta list` prints a heading and then `key<spaces>value`
	// rows; only the key is needed, so the value's own spacing cannot mislead this.
	for _, line := range strings.Split(string(out), "\n") {
		key, _, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}

		generation, isVerification := strings.CutPrefix(key, VerifiedKey+".")
		if !isVerification {
			continue
		}

		gen, isGeneration := ParseGeneration(generation)
		if !isGeneration {
			continue
		}

		if !live[gen.Name] {
			continue
		}

		if !found || gen.Built.After(newest.Built) {
			newest, found = gen, true
		}
	}

	return newest, found, nil
}

// RecordVerification records a generation's kernel pairing and its verification
// together, under the publish lock, having proved the generation still exists.
//
// THREE THINGS THAT WERE SEPARATE AND HAD TO STOP BEING.
//
// The two writes were unordered against reaping. A reap landing between them
// leaves a generation VERIFIED BUT UNPAIRED -- every node takes it up through
// `@verified` and each boots it against its own kernel -- which is the exact state
// the write order was chosen to prevent. Holding the publish lock is what makes
// the pair indivisible with respect to the only thing that removes generations.
//
// And neither write proved the generation was still there. Both validate the NAME
// and nothing else, so a reap completing first left both keys recreated for a
// snapshot that no longer exists. The existence check happens under the lock, so
// it cannot go stale between the check and the writes.
//
// An empty kernel means "record no pairing" -- the generation already had one, or
// this node's kernel is outside the managed directory -- and is not an error.
func (c *Client) RecordVerification(
	ctx context.Context,
	image, generation, kernel string,
	at time.Time,
) error {
	if err := checkCloneName(image); err != nil {
		return err
	}

	if _, ok := ParseGeneration(generation); !ok {
		return fmt.Errorf("ceph: %q is not a generation billet published", generation)
	}

	lock, err := c.TakePublishLock(ctx, at)
	if err != nil {
		return fmt.Errorf("ceph: could not record the verification of %s@%s because the "+
			"publish lock is held; a reap or a publish is in progress and recording now "+
			"could describe a generation it is removing: %w", image, generation, err)
	}

	defer func() {
		// ON A CONTEXT STRIPPED OF CANCELLATION, because a leaked publish lock blocks
		// every publisher on every node for hours.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		if releaseErr := lock.Release(releaseCtx); releaseErr != nil {
			// BEST EFFORT AND NOT SUBSTITUTED FOR THE REAL RESULT. If the recording
			// succeeded, a failed release is a leaked lock that StaleLockAfter bounds;
			// if it failed, that failure is the one worth reporting.
			_ = releaseErr
		}
	}()

	// PROVED UNDER THE LOCK. Checked before taking it, this would be a fact about a
	// moment that has passed.
	generations, err := c.Generations(ctx, image)
	if err != nil {
		return err
	}

	present := false

	for _, gen := range generations {
		if gen.Name == generation {
			present = true

			break
		}
	}

	if !present {
		return fmt.Errorf("ceph: %s@%s no longer exists, so there is nothing to record a "+
			"verification against. It was removed while it was being verified -- the probe "+
			"still booted because a clone outlives the snapshot it came from", image, generation)
	}

	// THE PAIRING FIRST, so a failure between the two leaves the generation
	// unverified rather than verified-and-unpaired. Unverified is a state nothing
	// acts on; verified-and-unpaired is one every node acts on wrongly.
	if kernel != "" {
		if err := c.SetKernel(ctx, image, generation, kernel); err != nil {
			return err
		}
	}

	return c.MarkVerified(ctx, image+"@"+generation, at)
}
