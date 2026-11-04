package images

import (
	"context"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// archChecker is what billet check asks the cluster about the architectures the
// generations of an image record.
type archChecker interface {
	Generations(ctx context.Context, image string) ([]ceph.Generation, error)
	GenerationArches(ctx context.Context, image string) (map[string]string, error)
}

// openArchChecker is a variable so a test can answer for the cluster.
var openArchChecker = func(cfg *config.Config) (archChecker, error) {
	return ceph.New(*cfg.Node.Ceph)
}

// ReportImageArchitectures says, for each image this node's microVM tiers boot,
// which generations record no architecture and which record another machine's than
// this host's (ADR-022), before an import meets one: a pull reads the first kind
// from the generation itself and refuses an import while one cannot be read, and
// refuses one into an image holding the second. ADVISORY: it reports and decides
// nothing.
func ReportImageArchitectures(ctx context.Context, env cli.Env, cfg *config.Config) {
	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return
	}

	uses, err := firecrackerTierUses(cfg)
	if err != nil || len(uses) == 0 {
		return
	}

	store, err := openArchChecker(cfg)
	if err != nil {
		fmt.Fprintf(env.Stdout, "images   could not read which architectures the cluster's "+
			"generations record: %v\n", err)

		return
	}

	seen := map[string]bool{}

	for _, use := range uses {
		image, _, _ := strings.Cut(use.image, "@")
		if seen[image] {
			continue
		}

		seen[image] = true

		for _, line := range archLines(ctx, store, image, hostArch()) {
			fmt.Fprintf(env.Stdout, "images   %s\n", line)
		}
	}
}

// archLines is what ReportImageArchitectures says about one image on a host of
// machine.
func archLines(ctx context.Context, store archChecker, image, machine string) []string {
	generations, err := store.Generations(ctx, image)
	if err != nil {
		return []string{fmt.Sprintf("%s: could not tell which generations exist: %v", image, err)}
	}

	arches, err := store.GenerationArches(ctx, image)
	if err != nil {
		return []string{fmt.Sprintf("%s: could not tell which architectures its generations record: %v",
			image, err)}
	}

	var unknown, other []string

	for _, gen := range generations {
		switch recorded := arches[gen.Name]; recorded {
		case machine:
		case "":
			unknown = append(unknown, gen.Name)
		default:
			other = append(other, gen.Name+" ("+recorded+")")
		}
	}

	var lines []string

	if len(unknown) > 0 {
		lines = append(lines, fmt.Sprintf("%s holds generations that record no architecture (%s); the "+
			"next `billet images pull` or refresh on a node reads it from them, and while one cannot be "+
			"read no image is imported into %s until it is unpromoted and reaped", image,
			strings.Join(unknown, ", "), image))
	}

	if len(other) > 0 {
		lines = append(lines, fmt.Sprintf("%s holds generations built for another machine than this %s "+
			"host (%s), which no launch here boots, and no %s image is imported into it while they "+
			"remain: an image holds one architecture", image, machine, strings.Join(other, ", "), machine))
	}

	return lines
}
