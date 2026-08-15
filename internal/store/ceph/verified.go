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

		if !found || gen.Built.After(newest.Built) {
			newest, found = gen, true
		}
	}

	return newest, found, nil
}
