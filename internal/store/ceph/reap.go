package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Retention says which generations survive a reap.
type Retention struct {
	// Keep is how many VERIFIED generations to leave behind, newest first.
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
func PlanReap(all []Generation, verified map[string]bool, keep Retention) []Reapable {
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
	kept := 0

	for _, gen := range sorted {
		switch {
		case pinned[gen.Name]:
			// A TIER SAYS IT BOOTS THIS ONE. Whether it was ever verified is not the
			// question: somebody chose it, and the choice outranks this.
			plan = append(plan, Reapable{Generation: gen, Reason: "a tier pins it"})

		case verified[gen.Name] && kept < keep.Keep:
			kept++

			reason := "verified, and it is what @" + Verified + " resolves to"
			if kept > 1 {
				reason = fmt.Sprintf("verified, and kept as rollback %d of %d", kept-1, keep.Keep-1)
			}

			plan = append(plan, Reapable{Generation: gen, Reason: reason})

		default:
			plan = append(plan, Reapable{Generation: gen})
		}
	}

	return plan
}

// Reap removes the generations a plan does not keep, oldest first.
//
// OLDEST FIRST, so that an interrupted reap has removed the least useful things
// rather than a random half.
func (c *Client) Reap(ctx context.Context, image string, plan []Reapable) ([]string, error) {
	name, _, _ := strings.Cut(strings.TrimSpace(image), "@")
	if name == "" {
		return nil, fmt.Errorf("ceph: no image to reap generations of")
	}

	doomed := make([]Generation, 0, len(plan))

	for _, item := range plan {
		if item.Reason == "" {
			doomed = append(doomed, item.Generation)
		}
	}

	sort.Slice(doomed, func(i, j int) bool { return doomed[i].Built.Before(doomed[j].Built) })

	removed := make([]string, 0, len(doomed))

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

		// AND ITS METADATA, or the keys outlive the thing they describe: a
		// verification claim for a generation nothing can boot would keep answering
		// `@verified` with a name that is gone.
		for _, key := range []string{VerifiedKey + "." + gen.Name, RunnerVersionKey + "." + gen.Name} {
			//nolint:errcheck // the generation is already gone; a stale key is not worth failing on
			_, _ = c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "image-meta", "remove", name, key)
		}

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
