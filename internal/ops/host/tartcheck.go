package host

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	opsimages "github.com/junioryono/billet/internal/ops/images"
	"github.com/junioryono/billet/internal/provider/tart"
)

// tartVerifiedBeforeEveryHost refuses a catalogue naming a Tart `@verified` while a
// registered host runs a billet that predates it (ADR-021).
//
// A MAC'S OWN IMAGE COMMANDS READ ITS FILE: the daily refresh, and the image step of a
// host upgrade, which runs before the new binary is installed, hand the reference to
// tart as a name. A launch is held at dispatch; this holds the file. Judged by the newest
// wire a host said it speaks, as archBeforeEveryHost judges an architecture, and a host
// whose registration predates recording the range cannot say, so it is held too.
func tartVerifiedBeforeEveryHost(cfg *config.Config, hosts []alloc.NodeWire) error {
	if !config.CatalogueNamesTartVerified(cfg.Tiers) {
		return nil
	}

	var older []string

	for _, h := range hosts {
		if h.Decommissioned == "" && h.Max < nodeapi.VersionTartVerified {
			release := h.Release
			if release == "" {
				release = "a release it did not name"
			}

			older = append(older, h.Name+" ("+release+")")
		}
	}

	if len(older) == 0 {
		return nil
	}

	slices.Sort(older)

	return fmt.Errorf("this file names a verified Tart image, and %d registered host(s) run a billet "+
		"from before tiers could: %s. Such a billet hands `<image>@verified` to tart as a name, so "+
		"upgrade every host before writing it into a file any of them reads",
		len(older), strings.Join(older, ", "))
}

// reportUnverifiedTartTiers names each Tart tier booting an image nobody verified, on a
// control plane, where the catalogue is.
func reportUnverifiedTartTiers(env cli.Env, cfg *config.Config) {
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if !t.AcceptsProvider(config.ProviderTart) {
			continue
		}

		image := t.ImageFor(config.ProviderTart)
		if _, verified := config.TartVerifiedImage(image); !verified {
			fmt.Fprintf(env.Stdout, "tart     tier %s boots %s, which nobody verified; naming it "+
				"%s@verified boots only a copy of what billet booted\n", t.Label, image, image)
		}
	}
}

// openTartReport builds the provider the report reads through, a variable so a test can
// answer for the host.
var openTartReport = func(cfg *config.Config) (tartReporter, error) {
	return opsimages.OpenTartProvider(cfg)
}

// tartReporter is what `billet check` asks of a provider about verified images.
type tartReporter interface {
	VerifiedImages(ctx context.Context) ([]tart.VerifiedImage, error)
	RefreshDeadlines(ctx context.Context, window tart.RunnerWindow) ([]string, error)
	Slots(ctx context.Context) (tart.SlotReport, error)
	StoreSpace(rec tart.VerifiedRecord) (uint64, int64, error)
}

// reportTartVerified says what this node holds of each verified image. A report: a tier
// with no usable record refuses its own launches naming the command, and a host that
// cannot be read is said to be unread, never healthy.
func reportTartVerified(ctx context.Context, env cli.Env, cfg *config.Config, tierImages []string) {
	if _, known, err := app.PeekNodeDeploymentID(cfg); err != nil || !known {
		fmt.Fprintf(env.Stdout, "verified not read: this node has no deployment identity yet\n")

		return
	}

	p, err := openTartReport(cfg)
	if err != nil {
		fmt.Fprintf(env.Stdout, "verified UNKNOWN: %v\n", err)

		return
	}

	unknownRunners, err := p.RefreshDeadlines(ctx, opsimages.TartRunnerWindow(time.Now))
	if err != nil {
		fmt.Fprintf(env.Stdout, "verified UNKNOWN: %v\n", err)
	}

	images, err := p.VerifiedImages(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "verified UNKNOWN: could not read %s: %v\n", cfg.Node.StateDir, err)

		return
	}

	byKey := make(map[string]tart.VerifiedImage, len(images))
	for _, image := range images {
		byKey[image.Key] = image
	}

	for _, ref := range tierImages {
		if _, verified := config.TartVerifiedImage(ref); !verified {
			continue
		}

		reportTartImage(env, p, config.TartImageKey(ref), byKey[config.TartImageKey(ref)], unknownRunners)
	}

	reportTartSlots(ctx, env, p)
}

// reportTartImage reports one tier's verified image.
func reportTartImage(env cli.Env, p tartReporter, key string, image tart.VerifiedImage, unknownRunners []string) {
	var newest *tart.VerifiedRecord

	for i := range image.Records {
		if image.Records[i].Invalid == "" && !slices.Contains(image.Withdrawn, image.Records[i].Digest) {
			newest = &image.Records[i]

			break
		}
	}

	if newest == nil {
		fmt.Fprintf(env.Stdout, "image    %-56s NOT verified on this node; every job on its tier will "+
			"fail to launch until `billet images verify %s`\n", key, key)

		return
	}

	fmt.Fprintf(env.Stdout, "image    %-56s verified %s as %s, runner %s\n", key,
		newest.VerifiedAt.Format(time.DateOnly), newest.Base, newest.Runner)

	switch deadline, ok := image.Deadlines[newest.Runner]; {
	case ok && !time.Now().Before(deadline):
		fmt.Fprintf(env.Stdout, "         runner %s went past GitHub's window on %s, so every job on its "+
			"tier refuses until an image with a newer one is verified\n", newest.Runner, deadline.Format(time.DateOnly))
	case ok:
		fmt.Fprintf(env.Stdout, "         runner %s stops being sent jobs on %s\n", newest.Runner, deadline.Format(time.DateOnly))
	case slices.Contains(unknownRunners, newest.Runner):
		fmt.Fprintf(env.Stdout, "         runner %s: its window is UNKNOWN, billet could not ask\n", newest.Runner)
	}

	if image.Yields > 0 {
		fmt.Fprintf(env.Stdout, "         its last %d verification(s) yielded to a job; if the macOS slots stay "+
			"busy, stop every node on this store with `billet local down --node` and run `billet images "+
			"verify --no-yield`\n", image.Yields)
	}

	if newest.GuestOS == "darwin" {
		fmt.Fprintf(env.Stdout, "         billet keeps two bases of this macOS image and a clone per running job; "+
			"how many stopped copies the macOS licence allows is your own position\n")
	}

	if free, size, err := p.StoreSpace(*newest); err != nil {
		fmt.Fprintf(env.Stdout, "         free space UNKNOWN: %v\n", err)
	} else if int64(min(free, 1<<62)) < size { //nolint:gosec // capped just above
		fmt.Fprintf(env.Stdout, "         only %d GiB free and its base is %d GiB, so the next verification "+
			"cannot hold its private store\n", free>>30, size>>30)
	}
}

// reportTartSlots reports the store's slots and the guests they do not account for.
func reportTartSlots(ctx context.Context, env cli.Env, p tartReporter) {
	report, err := p.Slots(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "slots    UNKNOWN: %v\n", err)

		return
	}

	if report.Recorded > 0 {
		fmt.Fprintf(env.Stdout, "slots    %d of %d macOS guest slots held\n", report.Held, report.Recorded)
	}

	for owner, limit := range report.Limits {
		if report.Recorded > 0 && limit != report.Recorded {
			fmt.Fprintf(env.Stdout, "slots    deployment %s names a limit of %d and this store has %d slots, so "+
				"it refuses every macOS guest until they agree\n", owner, limit, report.Recorded)
		}
	}

	for _, guest := range report.Uncounted {
		fmt.Fprintf(env.Stdout, "slots    a guest billet did not coordinate is running (%s), and counts as "+
			"a macOS guest\n", guest)
	}
}
