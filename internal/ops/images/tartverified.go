package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/lifeops/launchd"
	"github.com/junioryono/billet/internal/provider/tart"
)

// tartVerifier is what the commands ask of the Tart provider, a seam so a test can drive
// each command's decisions without a guest.
type tartVerifier interface {
	Verify(ctx context.Context, opts tart.VerifyOptions) (*tart.VerifiedRecord, error)
	Unpromote(ctx context.Context, image, base string) ([]string, error)
	ImageDue(ctx context.Context, image string) (tart.VerifiedDue, error)
	RefreshDeadlines(ctx context.Context, window tart.RunnerWindow) ([]string, error)
	VerifiedImages(ctx context.Context) ([]tart.VerifiedImage, error)
}

// OpenTartVerifier builds this node's provider as a launch would, with its real
// deployment, since a base and its records are that deployment's. A variable so a test
// can answer for it.
var OpenTartVerifier = func(cfg *config.Config) (tartVerifier, error) {
	return openTartProvider(cfg)
}

// peekTartDeployment reads which deployment this node is in without making one up. A variable
// so a test can answer for a node's identity.
var peekTartDeployment = app.PeekNodeDeploymentID

// openTartProvider never mints an identity: a node that has not enrolled would then refuse
// the certificate it is issued, for a command that could not have done anything anyway.
func openTartProvider(cfg *config.Config) (*tart.Provider, error) {
	deployment, known, err := peekTartDeployment(cfg)
	if err != nil {
		return nil, fmt.Errorf("could not tell which deployment this node is in: %w", err)
	}

	if !known {
		return nil, errors.New("this node has no deployment identity yet, and a verified image is " +
			"its deployment's; start it with `billet local up`, or finish `billet join`, first")
	}

	prov, err := app.NewProvider(cfg, deployment)
	if err != nil {
		return nil, err
	}

	p, ok := prov.(*tart.Provider)
	if !ok {
		return nil, errors.New("this node's provider is not tart")
	}

	return p, nil
}

// OpenTartProvider is OpenTartVerifier's real answer, for `billet check`'s report.
func OpenTartProvider(cfg *config.Config) (*tart.Provider, error) { return openTartProvider(cfg) }

// tartNodeRunning reports whether this Mac's node is running. A variable because the
// answer is launchd's.
var tartNodeRunning = func(ctx context.Context) (lifeops.Tristate, error) {
	c := launchd.New()
	_, node := c.Services()

	facts, err := c.RunningOne(ctx, node)
	if err != nil {
		return lifeops.Unknown, err
	}

	return facts.Active, nil
}

// TartRunnerWindow asks GitHub where a runner release sits, in the provider's words.
//
// Expired IS A PROOF WHATEVER WAS READ, since anything unread can only have opened the
// window earlier. Anything short of it on a history that was not read whole is could-not-
// tell, and a release newer than this one with no ordinary window has no date to store.
func TartRunnerWindow(now func() time.Time) tart.RunnerWindow {
	return func(ctx context.Context, runner string) (tart.RunnerAnswer, error) {
		fresh, err := resolveRunnerFreshness(ctx, &http.Client{Timeout: time.Minute}, runner)
		if err != nil {
			return tart.RunnerAnswer{}, err
		}

		switch {
		case fresh.Expired(now()):
			return tart.RunnerAnswer{Deadline: fresh.Deadline(), Expired: true}, nil
		case !fresh.InstalledKnown, !fresh.HistoryComplete:
			return tart.RunnerAnswer{}, fmt.Errorf("billet read less of the runner's release history "+
				"than an answer about %s needs", runner)
		case fresh.BehindWithoutAWindow():
			return tart.RunnerAnswer{}, fmt.Errorf("a release newer than %s exists with no ordinary "+
				"window to count from", runner)
		case fresh.Current():
			return tart.RunnerAnswer{}, nil
		default:
			return tart.RunnerAnswer{Deadline: fresh.Deadline()}, nil
		}
	}
}

// tartVerifyOptions is what one reference is verified with: the command and trust of the tiers
// this node would boot it for, which another node's tiers say nothing about. A tier that accepts
// untrusted work boots its guests under isolation, so a probe of its image does too. Two tiers
// naming one image with different commands refuse, since a probe asks where one runner is.
//
// A NODE'S FILE NEED NOT NAME THE TIER, which reaches it in each launch from the control plane's.
// So a reference no local tier names is asked as its last verification was. One never verified
// either is an operator's own, and boots the published images' default command as trusted work;
// the answer says so, because the node cannot see whether a tier takes untrusted work.
func tartVerifyOptions(
	ctx context.Context, cfg *config.Config, p tartVerifier, image string,
) (tart.VerifyOptions, bool, error) {
	opts := tart.VerifyOptions{
		Image:   image,
		Command: (config.Tier{}).RunnerCommandFor(config.ProviderTart),
		Window:  TartRunnerWindow(time.Now),
	}

	tiers, err := tartLocalTiers(cfg)
	if err != nil {
		return opts, false, err
	}

	key := config.TartImageKey(image)
	named := false

	for _, t := range tiers {
		if config.TartImageKey(t.ImageFor(config.ProviderTart)) != key {
			continue
		}

		command := t.RunnerCommandFor(config.ProviderTart)
		if named && !slices.Equal(command, opts.Command) {
			return opts, false, fmt.Errorf("the tiers on this node booting %s name different commands, "+
				"%v and %v, and a probe asks where one runner is; give them one command, or one image "+
				"each", image, opts.Command, command)
		}

		opts.Command, named = command, true

		if t.Trust != config.WorkloadTrusted {
			opts.GuestUntrusted = true
		}

		if t.GuestOS == config.GuestMacOS {
			opts.MacOS = true
		}
	}

	if named {
		return opts, true, nil
	}

	images, err := p.VerifiedImages(ctx)
	if err != nil {
		return opts, false, err
	}

	for i := range images {
		if images[i].Key != key || len(images[i].Command) == 0 {
			continue
		}

		opts.Command, opts.GuestUntrusted, opts.MacOS = images[i].Command, images[i].Untrusted, images[i].MacOS

		return opts, true, nil
	}

	return opts, false, nil
}

// cmdImagesVerifyTart is `billet images verify <image>` on a Mac.
func cmdImagesVerifyTart(
	ctx context.Context, env cli.Env, cfg *config.Config, image, digest string, noYield, untrusted bool,
) error {
	if noYield {
		active, err := tartNodeRunning(ctx)

		// COULD NOT TELL REFUSES, because this flag holds a slot against every job: it is
		// for a Mac no control plane can place anything on, and only a stopped node is that.
		if err != nil {
			return fmt.Errorf("billet images verify --no-yield: could not tell whether this Mac's "+
				"node is running: %w", err)
		}

		if active != lifeops.No {
			return errors.New("billet images verify --no-yield: this Mac's node is running, so a " +
				"job could need the slot this holds; stop it first with `billet local down --node`, " +
				"on every deployment's node sharing this store")
		}
	}

	p, err := OpenTartVerifier(cfg)
	if err != nil {
		return fmt.Errorf("billet images verify: %w", err)
	}

	opts, known, err := tartVerifyOptions(ctx, cfg, p, image)
	if err != nil {
		return fmt.Errorf("billet images verify: %w", err)
	}

	if untrusted {
		opts.GuestUntrusted = true
	}

	// SAID, because the tier that decides this may be in another machine's file, and a probe
	// of an untrusted tier's image should not have taken the default network unnoticed.
	if !known && !opts.GuestUntrusted {
		fmt.Fprintf(env.Stdout, "note     no tier in this file names %s, so its probe boots as trusted work "+
			"with the default command; if a tier on the control plane takes untrusted work with it, run "+
			"this with --untrusted\n", image)
	}

	opts.Digest, opts.NoYield, opts.Progress = digest, noYield, env.Stderr

	rec, err := p.Verify(ctx, opts)
	if err != nil {
		return fmt.Errorf("billet images verify: %w", err)
	}

	printTartRecord(env, rec)

	return nil
}

func printTartRecord(env cli.Env, rec *tart.VerifiedRecord) {
	fmt.Fprintf(env.Stdout, "verified %s as %s\n", rec.Image, rec.Base)
	fmt.Fprintf(env.Stdout, "         runner %s, %s guest, content %s\n", rec.Runner, rec.GuestOS, rec.ContentHash)

	if rec.Digest != "" {
		fmt.Fprintf(env.Stdout, "         digest %s\n", rec.Digest)
	}
}

// cmdImagesUnpromoteTart is `billet images unpromote <image>@<base>` on a Mac.
func cmdImagesUnpromoteTart(ctx context.Context, env cli.Env, cfg *config.Config, spec string) error {
	image, base, err := splitTartBase(spec)
	if err != nil {
		return err
	}

	p, err := OpenTartVerifier(cfg)
	if err != nil {
		return fmt.Errorf("billet images unpromote: %w", err)
	}

	dropped, err := p.Unpromote(ctx, image, base)
	if err != nil {
		return fmt.Errorf("billet images unpromote: %w", err)
	}

	for _, name := range dropped {
		fmt.Fprintf(env.Stdout, "removed  %s\n", name)
	}

	return nil
}

// splitTartBase reads `<image>@<base>`, whose base is a name billet made, so the last @
// is the separator however the image is spelled.
func splitTartBase(spec string) (string, string, error) {
	for i := len(spec) - 1; i > 0; i-- {
		if spec[i] == '@' {
			return spec[:i], spec[i+1:], nil
		}
	}

	return "", "", fmt.Errorf("billet images unpromote: %q names no base; name one as "+
		"<image>@<base>, as `billet check` lists them", spec)
}

// refreshTartVerified asks GitHub again for every recorded runner, whatever release.automatic
// says, since a deadline is a fact rather than an update, and answers the runners whose window
// it could not tell. It follows the records rather than the file: the tiers a launch uses are
// the control plane's, so a node's file can name no `@verified` while its jobs boot one.
func refreshTartVerified(ctx context.Context, env cli.Env, cfg *config.Config) error {
	if _, known, err := peekTartDeployment(cfg); err != nil {
		return fmt.Errorf("billet images refresh: %w", err)
	} else if !known {
		return nil
	}

	p, err := OpenTartVerifier(cfg)
	if err != nil {
		return fmt.Errorf("billet images refresh: %w", err)
	}

	unknown, err := p.RefreshDeadlines(ctx, TartRunnerWindow(time.Now))
	if err != nil {
		return fmt.Errorf("billet images refresh: %w", err)
	}

	for _, runner := range unknown {
		fmt.Fprintf(env.Stdout, "runner   %s: could not tell where it sits in GitHub's window, so any "+
			"date already stored stands\n", runner)
	}

	return nil
}

// verifyDueTartImages verifies each remote `@verified` reference whose tag now names a new
// digest, macOS included: those this file's tiers name and those this node still wants. One
// that fails stops none of the others. A verification that yielded to a job or found no slot is
// not a failure: the next refresh runs it again.
func verifyDueTartImages(ctx context.Context, env cli.Env, cfg *config.Config, dryRun bool) error {
	named, err := tartVerifiedImages(cfg)
	if err != nil {
		return fmt.Errorf("billet images refresh: %w", err)
	}

	if _, known, err := peekTartDeployment(cfg); err != nil {
		return fmt.Errorf("billet images refresh: %w", err)
	} else if !known {
		for _, image := range named {
			fmt.Fprintf(env.Stdout, "image    %-56s not verified: this node has no deployment identity yet\n", image)
		}

		return nil
	}

	p, err := OpenTartVerifier(cfg)
	if err != nil {
		return fmt.Errorf("billet images refresh: %w", err)
	}

	recorded, err := p.VerifiedImages(ctx)
	if err != nil {
		return fmt.Errorf("billet images refresh: %w", err)
	}

	images := slices.Clone(named)

	// AN IMAGE STILL WANTED, whether or not a verification of it ever finished: one that yielded
	// or failed has no record yet and is just as much this Mac's to finish. One nothing has
	// launched or verified for a while is not, whatever is left of it here.
	for i := range recorded {
		key := recorded[i].Key
		if recorded[i].Wanted && config.TartImageIsRemote(key) &&
			!slices.ContainsFunc(images, func(image string) bool { return config.TartImageKey(image) == key }) {
			images = append(images, key+"@verified")
		}
	}

	var failures []error

	for _, image := range images {
		due, err := p.ImageDue(ctx, image)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", image, err))

			continue
		}

		fmt.Fprintf(env.Stdout, "image    %-56s %s\n", image, due.Why)

		if !due.Verify || dryRun {
			continue
		}

		opts, _, err := tartVerifyOptions(ctx, cfg, p, image)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", image, err))

			continue
		}

		opts.Automatic, opts.Progress = true, env.Stderr

		rec, err := p.Verify(ctx, opts)

		switch {
		case errors.Is(err, tart.ErrYielded), errors.Is(err, tart.ErrNoSlot):
			fmt.Fprintf(env.Stdout, "image    %-56s %v\n", image, err)
		case err != nil:
			failures = append(failures, fmt.Errorf("%s: %w", image, err))
		default:
			printTartRecord(env, rec)
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("billet images refresh: %w", errors.Join(failures...))
	}

	return nil
}

// tartVerifiedImages are the images this node's tiers name `@verified`, as written.
func tartVerifiedImages(cfg *config.Config) ([]string, error) {
	tierImages, err := TartTierImages(cfg)
	if err != nil {
		return nil, err
	}

	var images []string

	for _, image := range tierImages {
		if _, verified := config.TartVerifiedImage(image); verified {
			images = append(images, image)
		}
	}

	return images, nil
}

// tartPullAbsent is the refresh's pull of unverified references. A node whose tiers all
// name `@verified` has nothing to pull, which is not the fault a pull with no image is.
func tartPullAbsent(ctx context.Context, env cli.Env, cfg *config.Config) error {
	tierImages, err := TartTierImages(cfg)
	if err != nil {
		return err
	}

	verified, err := tartVerifiedImages(cfg)
	if err != nil {
		return err
	}

	// INCLUDING NONE AT ALL, which is a joined Mac whose file names no tier, not a fault.
	if len(verified) == len(tierImages) {
		return nil
	}

	return PullTartImages(ctx, env, cfg, "")
}
