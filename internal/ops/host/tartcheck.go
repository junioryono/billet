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
		if _, verified := config.TartVerifiedImage(image); verified {
			continue
		}

		// A DIGEST CANNOT TAKE `@verified`, which validation refuses, so what to name differs.
		advice := "naming it " + image + "@verified"
		if strings.Contains(image, "@") {
			advice = "naming its tag @verified, rather than a digest,"
		}

		fmt.Fprintf(env.Stdout, "tart     tier %s boots %s, which nobody verified; %s boots only a "+
			"copy of what billet booted\n", t.Label, image, advice)
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
	OrphanProbes(ctx context.Context) ([]string, error)
	StoreSpace(rec tart.VerifiedRecord) (uint64, int64, error)
}

// reportTartVerified says what this node holds of each verified image. A report: a tier
// with no usable record refuses its own launches naming the command, and a host that
// cannot be read is said to be unread, never healthy.
//
// A MAINTENANCE PROBE ASKS GITHUB NOTHING AND WRITES NOTHING: it runs offline during an
// upgrade, and it reports the dates already stored.
func reportTartVerified(ctx context.Context, env cli.Env, cfg *config.Config, tierImages []string, skipNetwork bool) {
	_, known, err := app.PeekNodeDeploymentID(cfg)
	if err != nil {
		fmt.Fprintf(env.Stdout, "verified UNKNOWN: could not read this node's deployment identity: %v\n", err)

		return
	}

	if !known {
		fmt.Fprintf(env.Stdout, "verified not read: this node has no deployment identity yet\n")

		return
	}

	p, err := openTartReport(cfg)
	if err != nil {
		fmt.Fprintf(env.Stdout, "verified UNKNOWN: %v\n", err)

		return
	}

	var unknownRunners []string

	if !skipNetwork {
		if unknownRunners, err = p.RefreshDeadlines(ctx, opsimages.TartRunnerWindow(time.Now)); err != nil {
			fmt.Fprintf(env.Stdout, "verified UNKNOWN: %v\n", err)
		}
	}

	images, err := p.VerifiedImages(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "verified UNKNOWN: could not read %s: %v\n", cfg.Node.StateDir, err)

		return
	}

	byKey := make(map[string]tart.VerifiedImage, len(images))

	var keys []string

	// EVERY RECORD AS WELL AS EVERY TIER OF THIS FILE: the tiers a launch uses are the control
	// plane's, so this file need not name what its jobs boot, and a base it does not name
	// still holds tens of gigabytes.
	for _, image := range images {
		byKey[image.Key] = image

		if len(image.Records) > 0 || image.Pending != nil || image.Failed != nil {
			keys = append(keys, image.Key)
		}
	}

	for _, ref := range tierImages {
		if _, verified := config.TartVerifiedImage(ref); verified {
			keys = append(keys, config.TartImageKey(ref))
		}
	}

	slices.Sort(keys)

	for _, key := range slices.Compact(keys) {
		reportTartImage(env, p, key, byKey[key], unknownRunners)
	}

	orphans, err := p.OrphanProbes(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "verified UNKNOWN: %v\n", err)
	}

	for _, probe := range orphans {
		fmt.Fprintf(env.Stdout, "probe    %s is left from a verification that died, and may be running "+
			"unreserved; the image's next verification removes it\n", probe)
	}

	reportTartSlots(ctx, env, p)
}

// tartRecordedKeys are the images this node has records of, for a check that would otherwise
// call one nobody verified because this file names it plainly. Nothing, if it cannot be told:
// reportTartVerified says why.
func tartRecordedKeys(ctx context.Context, cfg *config.Config) map[string]bool {
	if _, known, err := app.PeekNodeDeploymentID(cfg); err != nil || !known {
		return nil
	}

	p, err := openTartReport(cfg)
	if err != nil {
		return nil
	}

	images, err := p.VerifiedImages(ctx)
	if err != nil {
		return nil
	}

	keys := make(map[string]bool, len(images))

	for i := range images {
		if len(images[i].Records) > 0 {
			keys[images[i].Key] = true
		}
	}

	return keys
}

// reportTartImage reports one image's records.
func reportTartImage(env cli.Env, p tartReporter, key string, image tart.VerifiedImage, unknownRunners []string) {
	var newest *tart.VerifiedRecord

	for i := range image.Records {
		rec := &image.Records[i]

		switch {
		case rec.Invalid != "":
			fmt.Fprintf(env.Stdout, "image    %-56s base %s is invalid (%s) and no launch resolves it; "+
				"the next verification removes it\n", key, rec.Base, rec.Invalid)
		case slices.Contains(image.Withdrawn, rec.Digest):
		case newest == nil:
			newest = rec
		default:
			fmt.Fprintf(env.Stdout, "image    %-56s also holds %s, verified %s, the rollback target\n",
				key, rec.Base, rec.VerifiedAt.Format(time.DateOnly))
		}
	}

	if image.Pending != nil {
		fmt.Fprintf(env.Stdout, "image    %-56s holds %s, pulled and waiting for a macOS slot to be "+
			"probed in; the next verification of %s boots it\n", key, image.Pending.Base, image.Pending.Digest)
	}

	if image.Failed != nil {
		fmt.Fprintf(env.Stdout, "image    %-56s %s failed its verification on %s; the refresh leaves it "+
			"for a week, and `billet images verify %s` tries it now\n", key, image.Failed.Digest,
			image.Failed.At.Format(time.DateOnly), key)
	}

	if newest == nil {
		fmt.Fprintf(env.Stdout, "image    %-56s NOT verified on this node; every job on its tier will "+
			"fail to launch until `billet images verify %s`\n", key, key)

		return
	}

	fmt.Fprintf(env.Stdout, "image    %-56s verified %s as %s, runner %s\n", key,
		newest.VerifiedAt.Format(time.DateOnly), newest.Base, newest.Runner)

	if problem := image.BaseProblems[newest.Base]; problem != "" {
		fmt.Fprintf(env.Stdout, "         %s: %s, so the next job on its tier fails to launch; run `billet "+
			"images verify %s`\n", newest.Base, problem, key)
	}

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
		fmt.Fprintf(env.Stdout, "         its last %d verification(s) gave way to a job or found no slot; if the "+
			"macOS slots stay busy, stop every node on this store with `billet local down --node` and run "+
			"`billet images verify --no-yield`\n", image.Yields)
	}

	if newest.GuestOS == "darwin" {
		fmt.Fprintf(env.Stdout, "         billet keeps two bases of this macOS image and a clone per running job; "+
			"how many stopped copies the macOS licence allows is your own position\n")
	}

	if free, size, err := p.StoreSpace(*newest); err != nil {
		fmt.Fprintf(env.Stdout, "         free space UNKNOWN: %v\n", err)
	} else if int64(min(free, 1<<62)) < size {
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

	if !report.LimitKnown {
		fmt.Fprintf(env.Stdout, "slots    this node has no record of its macOS limit, which its control plane "+
			"sends at registration: a launch uses Apple's default of %d and a macOS verification refuses\n",
			config.DefaultMacOSVMLimit)
	}

	for owner, limit := range report.Limits {
		if report.Recorded > 0 && limit != report.Recorded {
			fmt.Fprintf(env.Stdout, "slots    deployment %s names a limit of %d and this store has %d slots, so "+
				"every deployment here refuses a macOS guest until they agree; if it no longer uses this "+
				"store, remove its macos-limit file under .billet\n", owner, limit, report.Recorded)
		}
	}

	for _, guest := range report.Uncounted {
		fmt.Fprintf(env.Stdout, "slots    a guest billet did not coordinate is running (%s), and counts as "+
			"a macOS guest\n", guest)
	}
}
