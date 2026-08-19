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
// was proved to work with this node's guest contract".
//
// A WORD RATHER THAN AN IMPLICIT DEFAULT. A bare image name stays refused: choosing
// a generation for somebody who did not choose one is how a job silently boots
// something nobody decided on. Naming `@verified` IS the decision — it says "whatever
// passed, most recently" — and a tier that wants one exact generation still pins it
// and gets it forever.
const Verified = "verified"

// MarkVerified records that a generation booted, registered and ran a container.
// It excludes reaping and proves the generation still exists with the expected
// guest contract before publishing it.
func (c *Client) MarkVerified(
	ctx context.Context,
	image, expectedContract string,
	at time.Time,
) (err error) {
	name, generation, found := strings.Cut(strings.TrimSpace(image), "@")
	if !found || strings.TrimSpace(generation) == "" {
		return fmt.Errorf("ceph: %q names no generation to mark verified", image)
	}

	if _, ok := ParseGeneration(generation); !ok {
		return fmt.Errorf("ceph: %q is not a generation billet published, so marking it "+
			"verified would put a claim on something nothing can resolve", generation)
	}

	lock, lockErr := c.TakePublishLock(ctx, at)
	if lockErr != nil {
		return fmt.Errorf("ceph: could not mark %s verified because the publish lock is held; "+
			"a reap, verification, or publish is in progress: %w", image, lockErr)
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		if releaseErr := lock.Release(releaseCtx); releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()

	generations, err := c.Generations(ctx, name)
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
		return fmt.Errorf("ceph: %s no longer exists, so it cannot be marked verified", image)
	}

	contract, recorded, err := c.GuestContract(ctx, name, generation)
	if err != nil {
		return err
	}
	if !recorded || contract != expectedContract {
		return fmt.Errorf("ceph: %s cannot be promoted for this binary: it records guest contract %q, but this binary requires %q; boot-verify it with this binary instead",
			image, contract, expectedContract)
	}

	return c.markVerified(ctx, name, generation, at)
}

// markVerified writes the verification while its caller holds the publish lock.
func (c *Client) markVerified(
	ctx context.Context,
	name, generation string,
	at time.Time,
) error {
	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "set", name,
		VerifiedKey+"."+generation, at.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("ceph: record %s@%s as verified: %w", name, generation, err)
	}

	return nil
}

// UnmarkVerified withdraws that claim, which is how a fleet is rolled back.
//
// IDEMPOTENT, because the thing an operator does in a hurry should not fail for
// having already worked.
func (c *Client) UnmarkVerified(ctx context.Context, image string) (err error) {
	name, generation, found := strings.Cut(strings.TrimSpace(image), "@")
	if !found || strings.TrimSpace(generation) == "" {
		return fmt.Errorf("ceph: %q names no generation to unmark", image)
	}

	lock, lockErr := c.TakePublishLock(ctx, time.Now())
	if lockErr != nil {
		return fmt.Errorf("ceph: could not unmark %s because the publish lock is held; a "+
			"reap, verification, or publish is in progress: %w", image, lockErr)
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		if releaseErr := lock.Release(releaseCtx); releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()

	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "remove", name,
		VerifiedKey+"."+generation); err != nil {
		if isNoSuchFile(err) {
			return nil
		}

		return fmt.Errorf("ceph: withdraw the verification of %s: %w", image, err)
	}

	return nil
}

// NewestVerified resolves an image to the most recent generation that passed,
// without filtering by guest contract. Operator commands use this to describe the
// publication history; a node launch uses NewestVerifiedForContract instead.
//
// BY BUILD TIME, NOT BY WHEN IT WAS VERIFIED. Re-verifying an older generation — the
// obvious thing to do while investigating a bad one — would otherwise promote it over
// a newer one that has been serving jobs for a week.
//
// NOT FOUND IS NOT AN ERROR here, and the caller must not treat it as one: a fleet
// whose newest generations have all failed verification has nothing to resolve to,
// and the honest answer is to say so rather than to fall back to something unproven.
func (c *Client) NewestVerified(ctx context.Context, image string) (Generation, bool, error) {
	return c.newestGeneration(ctx, image, "", true)
}

// NewestVerifiedForContract resolves an image to the newest verified generation
// that speaks one exact host/guest protocol.
//
// CONTRACT-RELATIVE SO A ROLLING UPGRADE DOES NOT ADVANCE OLD NODES. A candidate
// binary may publish a newer verified generation while nodes on the prior binary
// are still serving jobs. Those nodes must keep resolving @verified to their newest
// compatible generation rather than booting a guest that will reject their metadata.
func (c *Client) NewestVerifiedForContract(
	ctx context.Context,
	image, contract string,
) (Generation, bool, error) {
	if strings.TrimSpace(contract) == "" {
		return Generation{}, false, fmt.Errorf("ceph: no guest contract to resolve %s against", image)
	}

	return c.newestGeneration(ctx, image, contract, true)
}

// NewestForContract resolves an image to the newest live generation that records
// one exact host/guest protocol, whether or not that generation is verified yet.
// Upgrade compatibility checks use it to boot-verify an already imported image
// instead of requiring a redundant download.
func (c *Client) NewestForContract(
	ctx context.Context,
	image, contract string,
) (Generation, bool, error) {
	if strings.TrimSpace(contract) == "" {
		return Generation{}, false, fmt.Errorf("ceph: no guest contract to resolve %s against", image)
	}

	return c.newestGeneration(ctx, image, contract, false)
}

func (c *Client) newestGeneration(
	ctx context.Context,
	image, contract string,
	verifiedOnly bool,
) (Generation, bool, error) {
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

	verified := map[string]bool{}
	contracts := map[string]string{}

	// THE TABLE IS PARSED RATHER THAN ASKED PER KEY, because the generations are not
	// known in advance. `image-meta list` prints a heading and then `key<spaces>value`
	// rows. Contract values carry no whitespace, but trimming the remainder also
	// tolerates the table alignment RBD adds.
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}

		if generation, isVerification := strings.CutPrefix(key, VerifiedKey+"."); isVerification {
			if _, isGeneration := ParseGeneration(generation); isGeneration {
				verified[generation] = true
			}

			continue
		}

		if generation, isContract := strings.CutPrefix(key, GuestContractKey+"."); isContract {
			if _, isGeneration := ParseGeneration(generation); isGeneration {
				contracts[generation] = strings.TrimSpace(value)
			}
		}
	}

	var (
		newest Generation
		found  bool
	)

	for generation := range live {
		gen, _ := ParseGeneration(generation)

		if verifiedOnly && !verified[gen.Name] {
			continue
		}
		if contract != "" && contracts[gen.Name] != contract {
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
// AN EMPTY KERNEL IS NOT ALWAYS BENIGN, which is why the caller says which case it
// is rather than this inferring it. "Already paired" needs no write; "this node's
// kernel is not one billet manages" means the generation would become @verified
// with nothing recorded -- and every node resolving that alias then boots it
// against whatever it happens to be configured with, which is the state this
// function exists to prevent. The second case is refused unless the caller has
// been told to allow it.
func (c *Client) RecordVerification(
	ctx context.Context,
	image, generation, kernel, guestContract string,
	paired, allowUnpaired bool,
	at time.Time,
) (err error) {
	if kernel == "" && !paired && !allowUnpaired {
		return fmt.Errorf("ceph: %s@%s booted, but the kernel that proved it is not one "+
			"billet manages, so nothing can record which kernel this generation needs. "+
			"Marking it verified would publish it to every node through @verified, and each "+
			"would boot it against whatever it is configured with -- which is the mismatch "+
			"that fails inside a job rather than at launch. Pull the image so its kernel is "+
			"installed and paired, or pass --allow-unpaired if every node in this deployment "+
			"is configured with the same kernel", image, generation)
	}

	if err := checkCloneName(image); err != nil {
		return err
	}

	if _, ok := ParseGeneration(generation); !ok {
		return fmt.Errorf("ceph: %q is not a generation billet published", generation)
	}

	lock, lockErr := c.TakePublishLock(ctx, at)
	if lockErr != nil {
		return fmt.Errorf("ceph: could not record the verification of %s@%s because the "+
			"publish lock is held; a reap or a publish is in progress and recording now "+
			"could describe a generation it is removing: %w", image, generation, lockErr)
	}

	defer func() {
		// ON A CONTEXT STRIPPED OF CANCELLATION, because a leaked publish lock blocks
		// every publisher on every node for hours.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		// REPORTED WHEN NOTHING ELSE FAILED, which ImportGeneration already does and
		// these did not. Release stops the heartbeat before removing the lock, so a
		// transient failure leaves every publish, verification and reap on every node
		// blocked until StaleLockAfter -- six hours -- while the command that caused
		// it printed success.
		if releaseErr := lock.Release(releaseCtx); releaseErr != nil && err == nil {
			err = releaseErr
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

	// THE CONTRACT BEFORE THE VERIFICATION, for the same reason as the kernel.
	// A verified generation without it cannot be judged during an upgrade; writing
	// the verification first would let the fleet take up a generation while the
	// compatibility fact its next binary depends on is still absent.
	if err := c.SetGuestContract(ctx, image, generation, guestContract); err != nil {
		return err
	}

	return c.markVerified(ctx, image, generation, at)
}
