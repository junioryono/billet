package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/store/ceph"
)

// componentsChecker is what billet check asks the cluster: the generation a tier's
// image reference boots, and what that generation's image left out.
type componentsChecker interface {
	ResolveGeneration(ctx context.Context, image, guestContract string) (string, error)
	Generations(ctx context.Context, image string) ([]ceph.Generation, error)
	componentsReader
}

// openComponentsChecker is a variable so a test can answer for the cluster.
var openComponentsChecker = func(cfg *config.Config) (componentsChecker, error) {
	return ceph.New(*cfg.Node.Ceph)
}

// ReportImageComponents says, for each image this node's microVM tiers boot, what
// the generation they boot was completed with, what its image left out that this
// deployment has not accepted, what it lacks that the deployment accepts, and what
// it carries that the deployment no longer accepts. ADVISORY: it reports and
// decides nothing. A generation whose image left nothing out says nothing.
func ReportImageComponents(ctx context.Context, env cli.Env, cfg *config.Config) {
	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return
	}

	uses, err := firecrackerTierUses(cfg)
	if err != nil || len(uses) == 0 {
		return
	}

	store, err := openComponentsChecker(cfg)
	if err != nil {
		fmt.Fprintf(env.Stdout, "images   could not read what the cluster's generations left out: %v\n", err)

		return
	}

	for _, use := range uses {
		for _, line := range componentLines(ctx, store, use, cfg.Images.Accepted(),
			cfg.Release.AutomaticUpdates()) {
			fmt.Fprintf(env.Stdout, "images   %s\n", line)
		}
	}
}

// componentLines is what ReportImageComponents says about one image reference.
func componentLines(ctx context.Context, store componentsChecker, use tierImageUse,
	accepted []config.ImageComponent, automatic bool,
) []string {
	tiers := "tier " + use.tiers[0]
	if len(use.tiers) > 1 {
		tiers = "tiers " + strings.Join(use.tiers, ", ")
	}

	ref, err := store.ResolveGeneration(ctx, use.image, firecracker.GuestContract)
	if err != nil {
		return []string{fmt.Sprintf("%s (%s): could not tell which generation it boots: %v",
			use.image, tiers, err)}
	}

	image, generation, _ := strings.Cut(ref, "@")

	// THE GENERATION IS PROVED TO EXIST BEFORE ITS RECORD IS READ. An exact pin is
	// resolved unchanged, and a generation that is not there has no record either,
	// which would read as an image that left nothing out.
	generations, err := store.Generations(ctx, image)
	if err != nil {
		return []string{fmt.Sprintf("%s (%s): could not tell which generations of %s exist: %v",
			use.image, tiers, image, err)}
	}
	booted, exists := findGeneration(generations, generation)
	if !exists {
		return []string{fmt.Sprintf("%s (%s) names %s, which is not a generation of %s, so a "+
			"launch of it fails", use.image, tiers, ref, image)}
	}

	record, found, err := store.Components(ctx, image, generation)
	if err != nil {
		return []string{fmt.Sprintf("%s (%s): could not tell what %s left out: %v", use.image, tiers,
			ref, err)}
	}
	if !found {
		return nil
	}

	var installed, notAccepted []string
	for _, name := range record.LeftOut {
		version, isInstalled := record.Installed[name]
		switch {
		case isInstalled:
			installed = append(installed, name+" "+version)
		case !slices.Contains(accepted, config.ImageComponent(name)):
			notAccepted = append(notAccepted, name)
		}
	}
	missing, unaccepted := completionGap(record, found, accepted)

	next := nextCompletion(ctx, store, use.image, image, booted, generations, accepted, automatic)

	who := fmt.Sprintf("%s (%s) boots %s", use.image, tiers, ref)

	var lines []string
	if len(installed) > 0 {
		lines = append(lines, fmt.Sprintf("%s, completed with %s", who, strings.Join(installed, ", ")))
	}
	if len(notAccepted) > 0 {
		lines = append(lines, fmt.Sprintf("%s, whose image leaves out %s: this deployment has not "+
			"accepted them in images.accept, so a job that uses one will not find it", who,
			strings.Join(notAccepted, ", ")))
	}
	if len(missing) > 0 {
		lines = append(lines, fmt.Sprintf("%s, which lacks %s, which this deployment accepts; %s", who,
			strings.Join(missing, ", "), next))
	}
	if len(unaccepted) > 0 {
		lines = append(lines, fmt.Sprintf("%s, which carries %s, which this deployment no longer "+
			"accepts; %s", who, strings.Join(unaccepted, ", "), next))
	}

	return lines
}

// nextCompletion says what will bring the generation a tier boots into line with
// what this deployment accepts.
//
// THE SAME COMPARISON THE REFRESH MAKES, against the newest generation imported,
// not the one @verified resolves to: when a newer generation already matches, a
// refresh sees nothing to do while the channel is unchanged, and what is
// outstanding is that generation's verification, not another pull.
func nextCompletion(ctx context.Context, store componentsChecker, ref, image string,
	booted ceph.Generation, generations []ceph.Generation, accepted []config.ImageComponent,
	automatic bool,
) string {
	// A PINNED TIER KEEPS ITS PIN. A newer completed generation reaches it only when
	// somebody moves the pin, so that is what the sentence has to say.
	if _, alias, _ := strings.Cut(ref, "@"); alias != ceph.Verified {
		return "it is pinned, so a generation completed with what this deployment accepts reaches " +
			"it only when the pin moves"
	}

	newest := booted
	for _, g := range generations {
		if g.Built.After(newest.Built) {
			newest = g
		}
	}

	if newest.Name != booted.Name {
		record, found, err := store.Components(ctx, image, newest.Name)
		if err != nil {
			return fmt.Sprintf("could not tell what the newer generation %s left out: %v", newest.Name, err)
		}
		if missing, unaccepted := completionGap(record, found, accepted); len(missing) == 0 && len(unaccepted) == 0 {
			return fmt.Sprintf("the newer generation %s matches what this deployment accepts and "+
				"@verified has not moved onto it, which no refresh retries while the channel is "+
				"unchanged; `billet images verify %s@%s` proves it", newest.Name, image, newest.Name)
		}
	}

	if !automatic {
		return "release.automatic is false, so `billet images pull --verify " + image +
			"` is what completes a new generation of it"
	}

	return "the refresh timer's next `billet images refresh` completes a new generation of it"
}

// findGeneration is the generation of generations named name.
func findGeneration(generations []ceph.Generation, name string) (ceph.Generation, bool) {
	for _, g := range generations {
		if g.Name == name {
			return g, true
		}
	}

	return ceph.Generation{}, false
}

// ReportChannelImage says whether this billet can take up the guest image the
// channel publishes. One that needs a newer billet is never imported, and the
// tiers keep booting what they have until it ages out of GitHub's runner window,
// so a deployment that does not update itself is told that nothing will change
// until somebody upgrades billet. ADVISORY. It fetches the signed manifest, so
// billet check runs it only with its network probes.
func ReportChannelImage(ctx context.Context, env cli.Env, cfg *config.Config) {
	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return
	}

	if images, err := FirecrackerTierImages(cfg); err != nil || len(images) == 0 {
		return
	}

	// THE FETCH NARRATES ITSELF, which is right for a pull and noise in a check.
	quiet := env
	quiet.Stdout = io.Discard

	manifest, err := fetchImageManifest(ctx, quiet, cfg)
	if err == nil {
		err = manifest.Usable(firecracker.GuestContract, hostArch())
	}

	switch {
	case err == nil:
		fmt.Fprintf(env.Stdout, "images   the channel's image, built %s, is one this billet can import\n",
			manifest.BuiltAt.UTC().Format("2006-01-02 15:04 MST"))
	case errors.Is(err, imagesource.ErrNeedsNewerBillet) && cfg.Release.Pinned():
		fmt.Fprintf(env.Stdout, "images   the channel's image needs a newer billet, and "+
			"release.version pins this deployment at %s, so no newer guest image is taken up until "+
			"the pin moves, and the tiers keep booting what they have until it ages out of GitHub's "+
			"runner window: %v\n", cfg.Release.PinnedVersion(), err)
	case errors.Is(err, imagesource.ErrNeedsNewerBillet) && cfg.Release.AutomaticUpdates():
		fmt.Fprintf(env.Stdout, "images   the channel's image needs a newer billet, which this "+
			"deployment installs by itself when its release channel advances: %v\n", err)
	case errors.Is(err, imagesource.ErrNeedsNewerBillet):
		fmt.Fprintf(env.Stdout, "images   the channel's image needs a newer billet, and "+
			"release.automatic is false, so no newer guest image is taken up until an operator "+
			"upgrades billet, and the tiers keep booting what they have until it ages out of "+
			"GitHub's runner window: %v\n", err)
	default:
		fmt.Fprintf(env.Stdout, "images   could not tell whether this billet can import the channel's "+
			"image: %v\n", err)
	}
}
