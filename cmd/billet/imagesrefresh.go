package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/store/ceph"
)

// refreshStore is what a refresh asks the cluster: which generation of an image
// is newest.
//
// AN INTERFACE SO THE DECISION IS TESTABLE. What matters here is not what
// NewestGeneration does but what the refresh does with the answer — pull, or not
// — and that is observable only if a test can supply the answer.
type refreshStore interface {
	NewestGeneration(ctx context.Context, image string) (ceph.Generation, bool, error)
}

// The seams a refresh runs through, so a test can see what it decided without a
// cluster, a signed channel or a two-gigabyte download.
var (
	openRefreshStore = func(cfg *config.Config) (refreshStore, error) {
		return ceph.New(*cfg.Node.Ceph)
	}
	fetchImageManifest = func(ctx context.Context, cfg *config.Config) (*imagesource.Manifest, error) {
		manifest, _, err := resolveImageManifest(ctx, cfg, stageOptions{})

		return manifest, err
	}
	refreshPull = func(ctx context.Context, cfgPath, image string) error {
		return cmdImagesPull(ctx, []string{"--config", cfgPath, "--verify", image})
	}
	refreshReap = func(ctx context.Context, cfgPath, image string, keep int) error {
		return cmdImagesReap(ctx, []string{"--config", cfgPath, "--keep", strconv.Itoa(keep),
			image})
	}
)

// cmdImagesRefresh takes up a newer published guest image, when there is one.
//
// THE OTHER HALF OF AN AUTOMATIC UPDATE. A release is a binary; the runner a job
// runs in is a guest image billet publishes weekly, and GitHub stops queueing
// work to a runner about thirty days after a newer one ships. This is what
// billet-images.timer runs daily so a node that updates its binary also updates
// what it boots.
//
// NEWER THAN WHAT IS IMPORTED, NOT MERELY OLD. A generation is named for the
// moment it was imported, which is always after the build it came from, so a
// newest generation older than the channel's build time proves a newer image
// exists and one at or after it proves there is nothing to do. No new cluster
// metadata is needed for that, and the same image is never imported twice.
//
// PULL, VERIFY, PROMOTE, THEN REAP — through the same commands an operator runs,
// so a refresh cannot do anything a person could not. The reap keeps the three
// newest verified generations per guest contract, which is what the
// documentation has always used as its example; it never removes what a tier
// pins or a job is booting.
func cmdImagesRefresh(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images refresh")
	cfgPath := addConfigFlag(fs)
	keep := fs.Int("keep", 3, "how many verified generations to leave per guest contract "+
		"after a pull; 0 reaps nothing")
	dryRun := fs.Bool("dry-run", false, "say what would be pulled and pull nothing")

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil {
		return errors.New("billet images refresh: this config has no node section, so " +
			"there is nothing here that boots a guest image")
	}

	if !cfg.Release.AutomaticUpdates() {
		fmt.Printf("release.automatic is false, so guest images are left to an operator; " +
			"nothing to do.\n")

		return nil
	}

	switch cfg.Node.Provider {
	case config.ProviderTart:
		// PULLED IF ABSENT, AND NOTHING MORE. A tart tier names an OCI image by
		// tag, and this does not re-pull a tag that moved underneath one already
		// in the store — tens of gigabytes on a schedule for an image that may not
		// have changed is a decision a person makes. What it does is make sure
		// every configured image is present.
		if *dryRun {
			fmt.Printf("a tart node pulls only images that are absent; nothing is " +
				"compared against a channel\n")

			return nil
		}

		return pullTartImages(ctx, cfg, "")

	case config.ProviderFirecracker:
		return refreshFirecrackerImages(ctx, cfg, *cfgPath, *keep, *dryRun)

	default:
		fmt.Printf("a %s node boots no image billet publishes; nothing to do.\n",
			cfg.Node.Provider)

		return nil
	}
}

// refreshFirecrackerImages compares each configured image against the channel
// and takes up the ones the channel has moved past.
func refreshFirecrackerImages(ctx context.Context, cfg *config.Config, cfgPath string,
	keep int, dryRun bool,
) error {
	if cfg.Node.Ceph == nil {
		return errors.New("billet images refresh: this config names no ceph cluster, so " +
			"there is nowhere to import an image to")
	}

	configured, err := firecrackerTierImages(cfg)
	if err != nil {
		return err
	}

	if len(configured) == 0 {
		fmt.Printf("no firecracker tier on this node names an image; nothing to do.\n")

		return nil
	}

	manifest, err := fetchImageManifest(ctx, cfg)
	if err != nil {
		return err
	}

	store, err := openRefreshStore(cfg)
	if err != nil {
		return err
	}

	var problems []error

	for _, configured := range configured {
		// THE BARE NAME. A tier says `@verified` or an exact generation; a pull
		// publishes a new generation of the image and verification advances the
		// alias, exactly as `images pull` with no argument already does.
		image, _, _ := strings.Cut(configured, "@")

		due, why, err := refreshDue(ctx, store, image, manifest)
		if err != nil {
			problems = append(problems, err)

			continue
		}

		if !due {
			fmt.Printf("%-24s up to date: %s\n", image, why)

			continue
		}

		if dryRun {
			fmt.Printf("%-24s would pull: %s\n", image, why)

			continue
		}

		fmt.Printf("%-24s pulling: %s\n", image, why)

		if err := refreshPull(ctx, cfgPath, image); err != nil {
			// NO REAP AFTER A FAILED PULL. Reaping is bounded by what exists, and
			// a pull that failed partway left nothing new; running it anyway would
			// be a cleanup nobody asked for on a day something else went wrong.
			problems = append(problems, fmt.Errorf("refresh %s: %w", image, err))

			continue
		}

		if keep > 0 {
			if err := refreshReap(ctx, cfgPath, image, keep); err != nil {
				problems = append(problems, fmt.Errorf("reap %s after its refresh: %w", image, err))
			}
		}
	}

	return errors.Join(problems...)
}

// refreshDue decides whether the channel's image is newer than the newest
// imported generation, and says why in a sentence.
func refreshDue(ctx context.Context, store refreshStore, image string,
	manifest *imagesource.Manifest,
) (bool, string, error) {
	newest, found, err := store.NewestGeneration(ctx, image)
	if err != nil {
		return false, "", fmt.Errorf("refresh %s: %w", image, err)
	}

	built := manifest.BuiltAt.UTC().Format(time.RFC3339)

	if !found {
		return true, "no generation has been imported and the channel publishes one built " +
			built, nil
	}

	if newest.Built.Before(manifest.BuiltAt) {
		return true, fmt.Sprintf("the newest generation %s was imported %s, and the channel "+
			"publishes an image built %s", newest.Name, newest.Built.UTC().Format(time.RFC3339),
			built), nil
	}

	return false, fmt.Sprintf("the newest generation %s was imported %s, after the channel's "+
		"image was built (%s)", newest.Name, newest.Built.UTC().Format(time.RFC3339), built), nil
}
