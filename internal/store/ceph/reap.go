package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Retention says which generations survive a reap.
type Retention struct {
	// Keep is how many VERIFIED generations per guest contract to leave behind,
	// newest first.
	//
	// A COUNT RATHER THAN AN AGE, because the reason to keep more than the current
	// one is rollback: the newest may turn out bad after it has been promoted, and
	// `billet images unpromote` needs somewhere to land. How many days ago that
	// candidate was built is not the question — how many candidates there are is.
	Keep int
	// Pinned are generations a tier names explicitly. A config may pin one and
	// expect it to be there forever, and removing it would strand that tier.
	Pinned []string
}

// Reapable is one generation and why it is being removed, or kept.
type Reapable struct {
	Generation Generation
	// Verified records the verification state the plan was built from. Reap uses
	// it to distinguish a deliberately expired verified generation from one that
	// became verified after the plan was made.
	Verified bool
	// Contract is the guest contract recorded when the plan was built. A
	// compatibility boot may backfill it before Reap takes the publish lock.
	Contract string
	// Reason is why this generation is being kept, empty when it is not.
	Reason string
}

// PlanReap decides which generations may be removed.
//
// A PLAN RATHER THAN AN ACTION, and the same function answers `--dry-run` and the
// real thing. A preview computed by different code than the operation is a preview
// that eventually stops describing it — which for an irreversible command against a
// cluster is the one property worth guaranteeing.
//
// WHAT IT DOES NOT CONSIDER: whether anything is currently booting a generation.
// Measured on the cluster — clone v2 removes a snapshot with a live child, returns
// 0, and the child stays usable — so a running job is never disturbed by this.
// Retention is about what might still be BOOTED, not about what is in use, which is
// why there is no liveness check here to get wrong.
func PlanReap(
	all []Generation,
	verified map[string]bool,
	contracts map[string]string,
	keep Retention,
) []Reapable {
	sorted := append([]Generation(nil), all...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Built.After(sorted[j].Built) })

	pinned := map[string]bool{}
	for _, name := range keep.Pinned {
		_, generation, found := strings.Cut(strings.TrimSpace(name), "@")
		if !found {
			generation = strings.TrimSpace(name)
		}

		if generation != "" && generation != Verified {
			pinned[generation] = true
		}
	}

	plan := make([]Reapable, 0, len(sorted))
	keptByContract := map[string]int{}

	for _, gen := range sorted {
		contract := contracts[gen.Name]
		item := Reapable{
			Generation: gen,
			Verified:   verified[gen.Name],
			Contract:   contract,
		}

		switch {
		case pinned[gen.Name]:
			// A TIER SAYS IT BOOTS THIS ONE. Whether it was ever verified is not the
			// question: somebody chose it, and the choice outranks this.
			item.Reason = "a tier pins it"

		case verified[gen.Name] && keptByContract[contract] < keep.Keep:
			keptByContract[contract]++

			reason := "verified, and it is what @" + Verified + " resolves to"
			if contract != "" {
				reason += " for guest contract " + contract
			}
			if keptByContract[contract] > 1 {
				reason = fmt.Sprintf("verified, and kept as rollback %d of %d for guest contract %s",
					keptByContract[contract]-1, keep.Keep-1, printableContract(contract))
			}

			item.Reason = reason
		}

		plan = append(plan, item)
	}

	return plan
}

func printableContract(contract string) string {
	if contract == "" {
		return "legacy/unrecorded"
	}

	return contract
}

// Reap removes the generations a plan does not keep, oldest first.
//
// OLDEST FIRST, so that an interrupted reap has removed the least useful things
// rather than a random half.
func (c *Client) Reap( //nolint:nonamedreturns // the deferred release reports through the return value; a local assigned inside a defer is read after the return has been computed
	ctx context.Context,
	image string,
	plan []Reapable,
	retention Retention,
) (removed []string, err error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return nil, fmt.Errorf("ceph: no image to reap generations of")
	}

	// THE SAME LOCK VERIFICATION TAKES, and taking it here is what makes that one
	// mean anything: exclusion has to be mutual. A verify that holds the lock while
	// it proves a generation exists and records the result is only safe if nothing
	// can remove that generation meanwhile -- and removing generations is precisely
	// what this does.
	//
	// It is also the lock a publish takes, so a reap cannot run while an image is
	// being written either.
	lock, err := c.TakePublishLock(ctx, time.Now())
	if err != nil {
		return nil, fmt.Errorf("ceph: could not reap %s because the publish lock is held; a "+
			"publish or a verification is in progress and removing generations now could "+
			"delete one it is recording: %w", name, err)
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		// REPORTED WHEN NOTHING ELSE FAILED. Release stops the heartbeat before
		// removing the lock, so a transient failure blocks every publish,
		// verification and reap on every node until StaleLockAfter -- six hours --
		// while the command that caused it printed success.
		if releaseErr := lock.Release(releaseCtx); releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()

	// REPLANNED UNDER THE LOCK, because the preview was made without it.
	//
	// cmdImagesReap reads the verification state, decides what to remove, and only
	// then calls this -- so a verification can acquire the lock, mark a previously
	// unverified generation, and release it in the window between. Acting on the
	// plan as given would then delete a generation that became @verified while this
	// was deciding, which is the newest thing every node is about to boot.
	//
	// Re-reading here is cheap against the cost of being wrong, and it is the only
	// place the answer cannot go stale before it is used.
	allNow, err := c.Generations(ctx, name)
	if err != nil {
		return nil, err
	}
	verifiedNow, err := c.VerifiedGenerations(ctx, name)
	if err != nil {
		return nil, err
	}
	contractsNow, err := c.GenerationGuestContracts(ctx, name)
	if err != nil {
		return nil, err
	}

	originallyDoomed := make(map[string]Reapable, len(plan))
	for _, item := range plan {
		if item.Reason == "" {
			originallyDoomed[item.Generation.Name] = item
		}
	}

	// Intersected with the preview: state changes may make something newly
	// removable, but an irreversible command never removes more than it showed the
	// operator. Replanning the WHOLE set matters because one generation changing
	// contract can make an unchanged older generation the last rollback for its
	// contract.
	fresh := PlanReap(allNow, verifiedNow, contractsNow, retention)
	doomed := make([]Generation, 0, len(fresh))
	for _, item := range fresh {
		preview, shown := originallyDoomed[item.Generation.Name]
		if item.Reason == "" && shown &&
			preview.Verified == item.Verified && preview.Contract == item.Contract {
			doomed = append(doomed, item.Generation)
		}
	}

	sort.Slice(doomed, func(i, j int) bool { return doomed[i].Built.Before(doomed[j].Built) })

	removed = make([]string, 0, len(doomed))

	for _, gen := range doomed {
		if _, err := c.rbdCmd(ctx, false, "snap", "rm",
			c.cfg.ImagePool+"/"+name+"@"+gen.Name); err != nil {
			if isNoSuchFile(err) {
				// Already gone, which is the ordinary answer when two nodes reap at
				// once and not a reason to stop.
				continue
			}

			return removed, fmt.Errorf("ceph: remove the generation %s@%s: %w", name, gen.Name, err)
		}

		// AND ALL OF ITS METADATA, or the keys outlive the thing they describe: a
		// verification claim for a generation nothing can boot would keep answering
		// `@verified` with a name that is gone.
		//
		// EVERY KEY, WHICH IS WHY THIS IS A LIST AND NOT TWO LINES. KernelKey was
		// added later and this loop was not, so a reaped generation left its kernel
		// pairing behind -- and the kernel reaper reads exactly those keys to decide
		// what is still needed. A dead generation's key would have kept its kernel
		// alive forever, which is the opposite of what reaping is for.
		c.removeGenerationMetadata(ctx, name, gen.Name,
			VerifiedKey, RunnerVersionKey, KernelKey, GuestContractKey)

		removed = append(removed, gen.Name)
	}

	return removed, nil
}

// VerifiedGenerations is the set of generations that passed verification.
func (c *Client) VerifiedGenerations(ctx context.Context, image string) (map[string]bool, error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return nil, fmt.Errorf("ceph: no image to read verifications of")
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "list", name)
	if err != nil {
		if isNoSuchFile(err) {
			return map[string]bool{}, nil
		}

		return nil, fmt.Errorf("ceph: read the verifications of %s/%s: %w", c.cfg.ImagePool, name, err)
	}

	verified := map[string]bool{}

	for _, line := range strings.Split(string(out), "\n") {
		key, _, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}

		generation, isVerification := strings.CutPrefix(key, VerifiedKey+".")
		if !isVerification {
			continue
		}

		if _, isGeneration := ParseGeneration(generation); isGeneration {
			verified[generation] = true
		}
	}

	return verified, nil
}

// GenerationGuestContracts is the recorded guest contract of each generation.
func (c *Client) GenerationGuestContracts(
	ctx context.Context,
	image string,
) (map[string]string, error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return nil, fmt.Errorf("ceph: no image to read guest contracts of")
	}

	out, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "list", name)
	if err != nil {
		if isNoSuchFile(err) {
			return map[string]string{}, nil
		}

		return nil, fmt.Errorf("ceph: read the guest contracts of %s/%s: %w",
			c.cfg.ImagePool, name, err)
	}

	contracts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}

		generation, isContract := strings.CutPrefix(key, GuestContractKey+".")
		if !isContract {
			continue
		}
		if _, isGeneration := ParseGeneration(generation); isGeneration {
			contracts[generation] = strings.TrimSpace(value)
		}
	}

	return contracts, nil
}

// Generations lists every generation billet published of an image.
func (c *Client) Generations(ctx context.Context, image string) ([]Generation, error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return nil, fmt.Errorf("ceph: no image to list generations of")
	}

	out, err := c.rbdCmd(ctx, true, "-p", c.cfg.ImagePool, "snap", "ls", name)
	if err != nil {
		if isNoSuchFile(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("ceph: list the generations of %s/%s: %w", c.cfg.ImagePool, name, err)
	}

	var snaps []snapshot
	if err := json.Unmarshal(trimSpace(out), &snaps); err != nil {
		return nil, fmt.Errorf("ceph: %s did not answer with a json snapshot list; is it the "+
			"rbd command?", c.bin)
	}

	generations := make([]Generation, 0, len(snaps))

	for _, snap := range snaps {
		if gen, ok := ParseGeneration(snap.Name); ok {
			generations = append(generations, gen)
		}
	}

	return generations, nil
}
