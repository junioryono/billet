package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deploymentid"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/provider/tart"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/store/ceph"
)

// cmdImages works on the golden images microVM guests boot from.
//
// VERIFICATION IS THE ONLY OPERATION HERE THAT COULD NOT BE A SHELL SCRIPT, and it
// is the one that matters. Building an image is debootstrap and apt, which a script
// does well. Proving one WORKS means launching a real microVM the way billet
// launches one — the same provider, the same jail, the same metadata service — and
// then believing the guest rather than the host.
//
// THE HOST CANNOT SEE INSIDE A GUEST, which is the whole difficulty. Every host-side
// signal was green for the image that booted, read its registration, and ran nothing:
// the jailer exited 0, the API accepted every call, the VMM answered, the DHCP lease
// appeared. The only thing that knew otherwise was the guest, and the only way to ask
// it is to give it something to say and a place to say it.
func cmdImages(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet images <pull|refresh|compatible|verify|due|list|reap|promote|unpromote>")
	}

	switch args[0] {
	case "pull":
		return cmdImagesPull(ctx, args[1:])
	case "refresh":
		return cmdImagesRefresh(ctx, args[1:])
	case "compatible":
		return cmdImagesCompatible(ctx, args[1:])
	case "verify":
		return cmdImagesVerify(ctx, args[1:])
	case "due":
		return cmdImagesDue(ctx, args[1:])
	case "list":
		return cmdImagesList(ctx, args[1:])
	case "reap":
		return cmdImagesReap(ctx, args[1:])
	case "promote":
		return cmdImagesPromote(ctx, args[1:], true)
	case "unpromote":
		return cmdImagesPromote(ctx, args[1:], false)
	default:
		return fmt.Errorf("billet images: unknown subcommand %q", args[0])
	}
}

// cmdImagesCompatible proves that the selected generation speaks this binary's
// guest contract without downloading another image when the answer is already on
// the generation. A pre-metadata generation is boot-verified once and backfilled;
// after that every ordinary host converge is a metadata read.
func cmdImagesCompatible(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images compatible")
	cfgPath := addConfigFlag(fs)
	wait := fs.Duration("wait", 3*time.Minute, "how long to give an unrecorded guest to prove itself")
	resultFile := fs.String("result-file", "",
		"write the bare names of floating images that need replacement here")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}
	if *resultFile != "" {
		if err := os.Remove(*resultFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("billet images compatible: cannot clear stale result file %s: %w",
				*resultFile, err)
		}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images compatible: this config names no ceph cluster")
	}

	images := []string{rest}
	if rest == "" {
		images, err = firecrackerTierImages(cfg)
		if err != nil {
			return err
		}
	}
	if len(images) == 0 {
		return errors.New("billet images compatible: no image given and no firecracker tier names one")
	}

	refresh := make([]string, 0, len(images))
	for _, image := range images {
		err := checkImageCompatible(ctx, cfg, *cfgPath, image, *wait)
		if err == nil {
			continue
		}
		if exitStatus(err) != 2 {
			return err
		}

		name, _, _ := strings.Cut(image, "@")
		refresh = append(refresh, name)
		fmt.Println(err)
	}

	if len(refresh) == 0 {
		return nil
	}
	if *resultFile != "" {
		if err := writeImageResults(*resultFile, refresh); err != nil {
			return err
		}
	}

	return &exitError{code: 2, msg: fmt.Sprintf("%d configured guest image(s) need a compatible generation", len(refresh))}
}

func checkImageCompatible(
	ctx context.Context,
	cfg *config.Config,
	cfgPath, image string,
	wait time.Duration,
) error {
	name, generation, found := strings.Cut(image, "@")
	if !found || name == "" || generation == "" {
		return fmt.Errorf("billet images compatible: %q does not name an exact generation or @%s",
			image, ceph.Verified)
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	floating := generation == ceph.Verified
	var verified bool
	if floating {
		newest, ok, err := store.NewestVerifiedForContract(ctx, name, firecracker.GuestContract)
		if err != nil {
			return err
		}
		if !ok {
			newest, ok, err = store.NewestForContract(ctx, name, firecracker.GuestContract)
			if err != nil {
				return err
			}
			if ok {
				generation = newest.Name
			} else {
				// A GENERATION VERIFIED BEFORE CONTRACT METADATA EXISTED GETS ONE REAL
				// compatibility boot instead of forcing a multi-gigabyte replacement.
				newest, ok, err = store.NewestVerified(ctx, name)
				if err != nil {
					return err
				}
				if !ok {
					return incompatibleGuest(image, "no generation has passed verification")
				}

				generation = newest.Name
				verified = true
			}
		} else {
			generation = newest.Name
			verified = true
		}
	} else {
		generations, err := store.Generations(ctx, name)
		if err != nil {
			return err
		}

		present := false
		for _, candidate := range generations {
			if candidate.Name == generation {
				present = true

				break
			}
		}
		if !present {
			return fmt.Errorf("billet images compatible: pinned generation %s@%s no longer exists; update the exact pin before upgrading",
				name, generation)
		}

		verifiedGenerations, err := store.VerifiedGenerations(ctx, name)
		if err != nil {
			return err
		}

		verified = verifiedGenerations[generation]
	}

	contract, recorded, err := store.GuestContract(ctx, name, generation)
	if err != nil {
		return err
	}

	exact := name + "@" + generation
	needsBoot, err := guestNeedsCompatibilityBoot(exact, contract, recorded, verified, floating)
	if err != nil {
		return err
	}
	if !needsBoot {
		fmt.Printf("%s speaks guest contract %s; no image download is needed\n",
			exact, firecracker.GuestContract)

		return nil
	}

	// A GENERATION FROM BEFORE CONTRACT METADATA IS NOT ASSUMED INCOMPATIBLE. A
	// real boot is cheaper than a multi-gigabyte replacement and produces the fact
	// future upgrades can read without booting again.
	if err := cmdImagesVerify(ctx, []string{
		"--config", cfgPath,
		"--wait", wait.String(),
		exact,
	}); err != nil {
		return compatibilityBootFailure(exact, floating, err)
	}

	fmt.Printf("%s passed a compatibility boot and now records guest contract %s\n",
		exact, firecracker.GuestContract)

	return nil
}

func compatibilityBootFailure(image string, floating bool, err error) error {
	if !floating {
		return fmt.Errorf("%s is an exact pin with no recorded guest contract and did not pass a compatibility boot; update the exact generation pin before upgrading: %w",
			image, err)
	}

	return incompatibleGuest(image,
		fmt.Sprintf("it has no recorded guest contract and did not pass a compatibility boot: %v", err))
}

func guestNeedsCompatibilityBoot(
	image, contract string,
	recorded, verified, floating bool,
) (bool, error) {
	if recorded && contract != firecracker.GuestContract {
		if !floating {
			return false, fmt.Errorf("%s is pinned to guest contract %s, while this binary requires %s; update the exact generation pin before upgrading",
				image, contract, firecracker.GuestContract)
		}

		return false, incompatibleGuest(image, fmt.Sprintf("it speaks guest contract %s, while this binary requires %s",
			contract, firecracker.GuestContract))
	}

	// A SIGNED MANIFEST SAYS WHAT THE GUEST WAS BUILT TO SPEAK, not that this
	// exact imported generation ever booted on the host. Both facts are required
	// before an upgrade can skip the compatibility boot.
	return !recorded || !verified, nil
}

func incompatibleGuest(image, reason string) error {
	return &exitError{code: 2, msg: fmt.Sprintf("%s is not compatible: %s", image, reason)}
}

// cmdImagesDue reports whether the golden image is old enough to rebuild.
//
// THIS IS WHAT LETS EVERY NODE CARRY THE TIMER. A schedule on one machine is a
// schedule that stops when that machine does, and the thing it protects against --
// GitHub refusing jobs to a runner thirty days behind a release -- does not pause
// while a node is down. So the timer belongs on every node.
//
// THE CLUSTER LOCK ALONE DOES NOT MAKE THAT WORK, which is the part that is easy to
// get wrong. The lock stops two builds OVERLAPPING, and the second node then waits
// and rebuilds the same thing: with the timer's jitter, node B usually starts after
// node A has finished and released, and publishes a second identical generation. N
// nodes do N builds. This question is what turns them into one build with N-1
// machines standing by.
//
// AGE COMES FROM THE GENERATION'S NAME, which billet writes in UTC, rather than from
// the cluster's own timestamp, which `rbd snap ls` prints as a local-time string with
// no offset -- two nodes in different zones would otherwise disagree about the age of
// the same snapshot.
//
// EXIT 2 MEANS "NOTHING TO DO" RATHER THAN FAILURE. A node that finds a fresh
// generation has succeeded at its job, and a unit reporting failure every week on
// every machine but one teaches an operator to ignore it.
func cmdImagesDue(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images due")
	cfgPath := addConfigFlag(fs)
	maxAge := fs.Duration("max-age", 6*24*time.Hour,
		"rebuild when the newest generation is older than this")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images due: this config names no ceph cluster, so there are " +
			"no published generations to date")
	}

	image := rest
	if image == "" {
		image, err = firecrackerTierImage(cfg)
		if err != nil {
			return err
		}
	}

	if image == "" {
		return errors.New("billet images due: no image given and no firecracker tier names one")
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	newest, found, err := store.NewestGeneration(ctx, image)
	if err != nil {
		return err
	}

	if !found {
		fmt.Printf("no generation of %s has been published; a build is due\n", image)

		return nil
	}

	age := time.Since(newest.Built)
	if age < *maxAge {
		fmt.Printf("%s was published %s ago, which is inside %s; nothing to do\n",
			newest.Name, age.Round(time.Minute), *maxAge)

		return errNothingToBuild
	}

	fmt.Printf("the newest generation %s is %s old; a build is due\n",
		newest.Name, age.Round(time.Minute))

	return nil
}

// errNothingToBuild says a rebuild is not due. It is an ANSWER rather than a
// failure, which is why it carries a status of its own.
var errNothingToBuild = &exitError{code: 2, msg: "a recent generation already exists"}

// firecrackerTierImage is the first image this deployment's microVM tiers boot.
func firecrackerTierImage(cfg *config.Config) (string, error) {
	images, err := firecrackerTierImages(cfg)
	if err != nil {
		return "", err
	}
	if len(images) > 0 {
		return images[0], nil
	}

	return "", nil
}

// firecrackerTierImages are the distinct images this deployment's microVM tiers boot.
func firecrackerTierImages(cfg *config.Config) ([]string, error) {
	if cfg.Node == nil || cfg.Node.Provider != config.ProviderFirecracker {
		return nil, nil
	}

	if _, err := nodeBundle(cfg); err != nil {
		return nil, fmt.Errorf("select firecracker tier images: resolve node identity: %w", err)
	}

	images := make([]string, 0, len(cfg.Tiers))
	seen := map[string]bool{}

	for i := range cfg.Tiers {
		if cfg.Tiers[i].Node != "" && cfg.Tiers[i].Node != cfg.Node.Name {
			continue
		}
		if cfg.Tiers[i].Site != "" && cfg.Tiers[i].Site != cfg.Node.Site {
			continue
		}
		if image := cfg.Tiers[i].ImageFor(config.ProviderFirecracker); cfg.Tiers[i].AcceptsProvider(config.ProviderFirecracker) && image != "" {
			if !seen[image] {
				images = append(images, image)
				seen[image] = true
			}
		}
	}

	return images, nil
}

// tartTierImages are the distinct images this node's tart tiers boot.
//
// The same selection firecrackerTierImages makes, and it has to be: an operator
// asking billet to fetch what this host needs must get what this host would
// actually launch, not every image in the deployment.
func tartTierImages(cfg *config.Config) ([]string, error) {
	if cfg.Node == nil || cfg.Node.Provider != config.ProviderTart {
		return nil, nil
	}

	// IDENTITY FIRST, exactly as firecrackerTierImages resolves it. A TLS node
	// is ENCOURAGED to omit node.name — the certificate carries it, and
	// nodeBundle fills it in — so comparing tier pins against an unresolved
	// cfg.Node.Name skips every tier pinned to this machine, and the images
	// those tiers need are never fetched. The failure lands later, as jobs that
	// cannot launch on a host somebody just prepared.
	if _, err := nodeBundle(cfg); err != nil {
		return nil, fmt.Errorf("select tart tier images: resolve node identity: %w", err)
	}

	images := make([]string, 0, len(cfg.Tiers))
	seen := map[string]bool{}

	for i := range cfg.Tiers {
		if cfg.Tiers[i].Node != "" && cfg.Tiers[i].Node != cfg.Node.Name {
			continue
		}

		if cfg.Tiers[i].Site != "" && cfg.Tiers[i].Site != cfg.Node.Site {
			continue
		}

		image := cfg.Tiers[i].ImageFor(config.ProviderTart)
		if !cfg.Tiers[i].AcceptsProvider(config.ProviderTart) || image == "" || seen[image] {
			continue
		}

		images = append(images, image)
		seen[image] = true
	}

	return images, nil
}

// pullTartImages fetches the images this node's tart tiers name.
//
// A SEPARATE IMPLEMENTATION BEHIND THE SAME COMMAND, because the two backends
// mean different things by an image. A firecracker image is a manifest billet
// verifies, unpacks and imports into Ceph as a generation; a tart image is an
// OCI reference tart pulls into its own store, with the registry's own
// provenance. Sharing the operator's command while not sharing the pipeline is
// the honest arrangement: "fetch what my tiers need" is one question.
func pullTartImages(ctx context.Context, cfg *config.Config, only string) error {
	// A PULL OWNS NOTHING. The deployment identity exists so a VM billet creates
	// carries a marker saying whose it is; a pull writes into tart's shared OCI
	// cache, which every deployment on this Mac reads and none owns. So this is
	// the same placeholder `billet check` uses, for the same reason.
	p, err := tart.New(deploymentForCheck, tart.WithLogger(slog.Default()))
	if err != nil {
		return err
	}

	images, err := tartTierImages(cfg)
	if err != nil {
		return err
	}

	if only != "" {
		images = []string{only}
	}

	if len(images) == 0 {
		return errors.New("billet images pull: no image given and no tart tier on this node " +
			"names one")
	}

	for _, image := range images {
		// ALREADY PRESENT IS NOT AN ERROR AND NOT A RE-FETCH. These are tens of
		// gigabytes; re-pulling one an operator already has because a second tier
		// mentions it is an hour nobody asked for.
		if p.Pulled(ctx, image) {
			fmt.Printf("image    %-56s already pulled\n", image)

			continue
		}

		fmt.Printf("image    %-56s pulling\n", image)

		if err := p.Pull(ctx, image, os.Stderr); err != nil {
			return fmt.Errorf("billet images pull: %w", err)
		}

		fmt.Printf("image    %-56s pulled\n", image)
	}

	// AND ONE MORE PASS OVER ALL OF THEM, because verifying each image straight
	// after its own pull is not enough when there are two. tart reclaims space
	// from its own cache to make an operation fit, so fetching the second image
	// can evict the first — each check passes at the moment it runs, and the
	// host still ends up unable to launch one of its tiers. This host holds a
	// 19GB Linux image and an 81GB macOS one, which is exactly the shape where
	// that happens.
	var gone []string

	for _, image := range images {
		if !p.Pulled(ctx, image) {
			gone = append(gone, image)
		}
	}

	if len(gone) > 0 {
		return fmt.Errorf("billet images pull: fetched every image and %s %s no longer in "+
			"the store; tart reclaims its own cache to make room, so this disk cannot hold "+
			"all of this node's tier images at once — free space, or give the tiers that "+
			"share a host images it can hold together",
			strings.Join(gone, ", "), plural(len(gone), "is", "are"))
	}

	return nil
}

// plural picks a verb form, so a diagnostic reads as a sentence.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

// cmdImagesVerify boots one microVM from an image and makes the guest prove it works.
func cmdImagesVerify(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images verify")
	cfgPath := addConfigFlag(fs)
	wait := fs.Duration("wait", 3*time.Minute, "how long to give the guest to report back")
	record := fs.Bool("record", true,
		"on success, mark this generation verified so `@verified` resolves to it")
	allowUnpaired := fs.Bool("allow-unpaired", false,
		"mark verified even when the kernel that proved it is not one billet manages")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if rest == "" {
		return errors.New("usage: billet images <pull|refresh|compatible|verify|due|list|reap|promote|unpromote>")
	}

	// THE GENERATION IS REQUIRED, not defaulted, for the same reason the provider
	// refuses a bare image name: a clone of "the image" is a clone of whatever it
	// happens to be right now, and the point of a generation is that a job holds a
	// clone of something that cannot change underneath it. Verifying an unnamed
	// thing verifies nothing in particular.
	if !strings.Contains(rest, "@") {
		return fmt.Errorf("billet images verify: %q names no generation; verify a specific one, "+
			"like ubuntu-2404-x64@g20260814061844, because that is what a tier boots", rest)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// BOTH SECTIONS ARE OPTIONAL, and a remote microVM node is exactly the shape
	// that has one and not the other: it dials a control plane it does not run, so
	// it carries `node:` and no `server:` at all. Dereferencing either without
	// asking turned the ordinary case into a panic.
	if cfg.Node == nil {
		return errors.New("billet images verify: this config has no node section, so it " +
			"describes no machine that could boot a guest image")
	}

	if cfg.Node.Provider != config.ProviderFirecracker {
		return fmt.Errorf("billet images verify: this node's provider is %s, and only firecracker "+
			"boots a guest image; run this on a machine that runs microVMs", cfg.Node.Provider)
	}

	// THE SAME DERIVATION `billet node` USES, rather than a second one that agrees
	// with it by luck. The identity decides which jails this command can see, so a
	// verification that derived it differently would either see nothing of its own
	// or, worse, claim someone else's.
	deployment, err := verifyDeploymentID(cfg)
	if err != nil {
		return err
	}

	// AN IDENTITY OF ITS OWN, AND THIS IS NOT TIDINESS.
	//
	// A probe's lease is invented here and the allocator has never heard of it. Run
	// under the node's identity, the node daemon's sweep — which lists every
	// instance this deployment owns and destroys any whose lease it cannot account
	// for — finds the probe, correctly concludes it is an orphan, and kills it. The
	// node's own sweep is on a five-minute loop, but the control plane broadcasts one
	// after every successful reap and that ticks every thirty seconds — so on a
	// healthy registered node a boot-to-report window of a minute or so normally spans
	// one, and what an operator sees is the weekly gate reporting that a perfectly
	// good image "does NOT work".
	//
	// A separate identity puts the probe outside what that sweep will touch: a jail
	// whose owner marker is another billet's is "not ours to report and emphatically
	// not ours to destroy". This command cleans up its own, below and on the way out.
	prov, err := newProvider(cfg, probeDeployment(deployment))
	if err != nil {
		return err
	}

	lease, err := probeLeaseID(deployment)
	if err != nil {
		return err
	}

	// SAID OUT LOUD, BECAUSE THE PROBE'S OWNER IS DERIVED RATHER THAN LEGIBLE. A jail
	// a killed run leaves behind carries an owner marker matching no deployment on
	// this host — deliberately, since that is what keeps the node's sweep off it — so
	// the run that creates one is the only place the two can be connected.
	fmt.Printf("probe %s is owned by %s, not by this node's %s, so the node's sweep "+
		"will leave it alone\n",
		provider.InstanceName(lease), probeDeployment(deployment), deployment)

	// ONE VERIFICATION AT A TIME ON THIS MACHINE. The probe's name is the same every
	// run, so two at once are two names for one microVM.
	//
	// KEYED ON THE DEPLOYMENT rather than on the probe identity derived from it. What
	// exclusion needs is that every run which would be the SAME microVM takes the same
	// key, and the probe's name is derived from the deployment — so every run for this
	// deployment on this machine contends here. The derived owner would satisfy that
	// too, and it names, in the path an operator is handed when the lock is held, a
	// hash that appears nowhere else.
	lock, err := takeProbeLock(cfg.Node.LockDir, deployment)
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.release(); err != nil {
			fmt.Printf("warning: the verification lock was not released: %v\n", err)
		}
	}()

	// AND ANYTHING AN EARLIER RUN LEFT BEHIND GOES FIRST. The probe's name is the
	// same on this host every time, so this is one idempotent Destroy of one name
	// rather than a sweep of whatever happened to be there — which could not be made
	// safe, because the provider deliberately reports jails with no owner marker and
	// those are indistinguishable from a real node launch caught mid-creation.
	if err := destroyProbe(ctx, prov, provider.InstanceName(lease)); err != nil {
		return err
	}

	if err := verifyGuestImage(ctx, prov, cfg.Node.Firecracker.Bridge,
		cfg.Node.Firecracker.ImageVerifyPort, rest, lease, *wait); err != nil {
		return err
	}

	// RECORDED, WHICH IS WHAT MAKES THIS A PROMOTION RATHER THAN A REPORT.
	//
	// Without it the whole schedule ends in a sentence on a terminal nobody is
	// watching: build, verify, print "point a tier at it when you are ready", and
	// then nothing happens. A fleet goes on booting whatever generation somebody last
	// typed into a config file, while a verified image is published every week beside
	// it. That is the state this command existed in until now.
	//
	// A tier naming `@verified` takes it up from here with no config edit and no
	// restart, and one that pins a generation is unaffected.
	if *record {
		store, err := ceph.New(*cfg.Node.Ceph)
		if err != nil {
			return err
		}

		// THE KERNEL IS RECORDED BEFORE THE VERIFICATION, AND THAT ORDER IS THE POINT.
		//
		// This boot is the only evidence anywhere that a particular kernel runs a
		// particular filesystem -- it just did. Marking the generation verified
		// without recording which kernel proved it publishes a claim the fleet acts
		// on while discarding the fact that makes the claim true, and the next launch
		// falls back to whatever this node happens to be configured with.
		//
		// So a failure here stops the generation becoming @verified. A generation that
		// is verified but unpaired is worse than one that is neither: every node takes
		// it up, and each boots it against its own kernel.
		// `rest` is `<image>@<generation>`, which the check above proved.
		imageName, generation, _ := strings.Cut(rest, "@")

		// WHAT THE LAUNCH ACTUALLY BOOTED, not what this node is configured with.
		//
		// The provider resolves a generation's recorded kernel in preference to the
		// configuration, so on a normally-pulled generation the thing that just
		// booted is the thing already recorded. Overwriting it here would prove one
		// kernel and record another -- and then publish that through @verified.
		existing, _, err := store.Kernel(ctx, imageName, generation)
		if err != nil {
			return err
		}

		// THE SAME DIRECTORY THE LAUNCH WILL USE, taken from the node's config rather
		// than a flag of this command's own. Two sources for one path means verify can
		// classify a kernel as managed while the launch looks somewhere else, and the
		// pairing then names a file the launch will never find.
		record, note := kernelToRecord(existing, configuredKernelName(cfg, nodeKernelDir(cfg)))

		// ONE CALL, UNDER THE PUBLISH LOCK, having proved the generation still
		// exists. The pairing and the verification were two unordered writes, and a
		// reap landing between them leaves a generation verified but unpaired --
		// every node takes it up and each boots it against its own kernel.
		if err := store.RecordVerification(ctx, imageName, generation, record,
			firecracker.GuestContract,
			existing != "", *allowUnpaired, time.Now()); err != nil {
			return fmt.Errorf("%s booted and ran a container, but the result could not be "+
				"recorded, so it has NOT been marked verified: %w", rest, err)
		}

		if record != "" {
			fmt.Printf("\nrecorded %s as the kernel this generation was proved against\n", record)
		}

		if note != "" {
			fmt.Printf("\nnote: %s\n", note)
		}

		fmt.Printf("\nrecorded %s as verified; a tier naming @%s will boot it\n",
			rest, ceph.Verified)
	}

	return nil
}

// verifyGuestImage launches one microVM and waits for the guest to report on itself.
func verifyGuestImage(
	ctx context.Context, prov provider.Provider, bridge string, port int,
	image, lease string, wait time.Duration,
) error {
	// A LISTENER ON THIS MACHINE, because the assertion has to be made BY THE GUEST.
	// Anything the host can check on its own was already green for an image that ran
	// no job at all.
	report := make(chan string, 1)

	// THE SECRET IS MINTED BEFORE THE LISTENER, because the listener uses it to
	// decide what is even worth keeping.
	secret, err := verificationSecret()
	if err != nil {
		return err
	}

	srv, addr, serveErr, err := listenForGuestReport(ctx, bridge, port, secret, report)
	if err != nil {
		return err
	}

	defer func() {
		if err := srv.Close(); err != nil {
			fmt.Printf("warning: the report listener did not close: %v\n", err)
		}
	}()

	name := provider.InstanceName(lease)

	// WHAT THE GUEST IS ASKED TO SAY, and each part answers a way the image can be
	// broken while looking fine:
	//
	//   whoami      the agent dropped to the unprivileged account rather than
	//               running somebody's CI as root
	//   jit         the registration reached the command's environment intact,
	//               which is the whole delivery path in one value
	//   runner      the actions runner binary EXECUTES — a debootstrap rootfs is
	//               exactly where a .NET binary fails for a missing libicu, and
	//               that surfaces as every job failing to start
	//   docker      the daemon is up on this kernel, which check-config.sh can only
	//               predict, and a container actually ran
	//   buildx      the CLI plugin required by billet's persistent builder exists
	//   compose     the CLI plugin common multi-container workflows invoke exists
	probe := strings.Join([]string{
		`echo "whoami=$(whoami)"`,
		`echo "jit=$ACTIONS_RUNNER_INPUT_JITCONFIG"`,
		`echo "runner=$(cd /home/runner/runner && ./bin/Runner.Listener --version 2>&1 | head -1)"`,
		`echo "docker=$(docker info --format '{{.ServerVersion}} storage={{.Driver}} ` +
			`cgroups={{.CgroupVersion}}' 2>&1 | head -1)"`,
		`echo "buildx=$(docker buildx version 2>&1 | head -1)"`,
		`echo "compose=$(docker compose version --short 2>&1 | head -1)"`,
		`echo "container=$(docker run --rm hello-world 2>&1 | grep -ci 'working correctly' || echo 0)"`,
	}, "; ")

	spec := provider.Spec{
		Name:   name,
		Image:  image,
		VCPU:   2,
		Memory: 2 * config.GiB,
		// RUN ONCE AND POSTED, rather than run for a console that does not exist and
		// then run again to be sent: billet passes no console= to a guest, so the
		// first copy's output went nowhere while doubling the time to report and
		// pulling the test container twice.
		Command: []string{"/bin/sh", "-c",
			`report=$(` + probe + `); curl -sf --max-time 20 --data-binary "$report" http://` +
				addr + `/report`},
		Trust:     provider.TrustTrusted,
		JITConfig: secret,
	}

	fmt.Printf("verifying %s\n", image)

	if _, err := prov.Launch(ctx, spec); err != nil {
		// A LAUNCH ERROR IS NOT PROOF THAT NOTHING STARTED, which the provider says
		// in as many words: a cancelled context can kill this process after the work
		// was accepted, and the backend's own unwind can itself fail. So the probe is
		// reconciled rather than assumed away — otherwise a failed verification is
		// also a leaked uid, device name and cloned disk.
		return errors.Join(
			fmt.Errorf("billet images verify: %s did not launch: %w", image, err),
			destroyProbe(ctx, prov, name),
		)
	}

	verdict := awaitGuestReport(ctx, report, serveErr, image, secret, wait)

	// CLEANUP IS PART OF THE RESULT, NOT A WARNING BESIDE IT. As a bare defer this
	// printed a line and returned success, so the weekly job would announce a
	// verified image while probes accumulated — each holding a uid, a device name
	// and a cloned disk, and each invisible to the node's sweep by design.
	return errors.Join(verdict, destroyProbe(ctx, prov, name))
}

// awaitGuestReport waits for the guest to say something, or for a reason it cannot.
func awaitGuestReport(
	ctx context.Context, report <-chan string, serveErr <-chan error,
	image, secret string, wait time.Duration,
) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case body := <-report:
		return checkGuestReport(body, secret)

	case err := <-serveErr:
		// THE LISTENER DIED, WHICH IS NOT THE IMAGE'S FAULT. Without this the wait
		// runs to its deadline and the verdict blames a guest that was reporting to
		// a socket nobody was reading.
		return fmt.Errorf("billet images verify: the listener the guest reports to failed, so "+
			"nothing here can say whether %s works: %w", image, err)

	case <-timer.C:
		return fmt.Errorf("billet images verify: %s booted and never reported back within %s, "+
			"so it cannot be shown to run a job; boot it by hand with a console "+
			"(console=ttyS0 systemd.journald.forward_to_console=1) to read the agent's own "+
			"account of itself", image, wait)

	case <-ctx.Done():
		return ctx.Err()
	}
}

// destroyProbe removes a verification's microVM, asking first whether there is one.
//
// A BOUNDED CONTEXT OF ITS OWN. Cleanup has to outlive a cancelled or expired parent
// — that is the whole reason it does not inherit one — but WithoutCancel alone
// removes the deadline as well, so a teardown that wedges would hang a scheduled job
// until systemd killed it two hours later.
func destroyProbe(ctx context.Context, prov provider.Provider, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	// DESTROYED WITHOUT ASKING FIRST, because "there is no jail" is not "there is
	// nothing left". The firecracker backend handles exactly that state: it releases
	// the uid and device-name claims and discards the root disk for a launch that got
	// far enough to take them and then lost its jail. A Find-then-skip returned
	// success over precisely that residue, and since the probe is deliberately not
	// owned by the node, nothing else would ever collect it.
	//
	// Destroy is idempotent, so this is also what clears anything an earlier run
	// left: the probe's name is the same on this host every time.
	//
	// The teardown state is not consulted: this is the firecracker backend, whose
	// Destroy is synchronous, and the probe holds no lease whose capacity could be
	// released early if it were not.
	if _, err := prov.Destroy(ctx, name); err != nil {
		return fmt.Errorf("billet images verify: the probe %s was not cleaned up and holds a "+
			"uid, a device name and a cloned disk; nothing else will reap it, because it is "+
			"deliberately not owned by the node: %w", name, err)
	}

	return nil
}

// listenForGuestReport serves the address the guest posts its report to.
func listenForGuestReport(
	ctx context.Context, bridge string, port int, secret string, report chan<- string,
) (*http.Server, string, <-chan error, error) {
	// ON THE BRIDGE'S OWN ADDRESS AND THE DECLARED PORT. The guest reaches this over
	// the bridge, so loopback would be a listener it cannot see — and binding every
	// interface would expose it to networks that have no business reaching it. The
	// port is fixed because host policy must be able to admit exactly this callback;
	// the verification lock guarantees there cannot be two of these listeners here.
	host, err := hostAddrOnBridge(bridge, port)
	if err != nil {
		return nil, "", nil, err
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", host)
	if err != nil {
		return nil, "", nil, fmt.Errorf("billet images verify: listen for the guest's report on "+
			"%s: %w", host, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		// A REPORT IS A POST. Anything else on this bridge poking the port is not
		// the guest, and reading a body from it is work done on somebody else's
		// behalf.
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		// BOUNDED, because this is a report from a guest running somebody's image
		// and the alternative is letting it decide how much this process allocates.
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "could not read the report", http.StatusBadRequest)

			return
		}

		// THE SECRET IS CHECKED HERE, NOT ONLY IN THE VERDICT, because the channel
		// holds ONE report and the first arrival wins it. The bridge this listens on
		// is carrying other guests running somebody's CI, so a stray POST — or a
		// scanner — would take the slot, the real report would be dropped by the
		// non-blocking send, and the verdict would be a confident FAIL saying the
		// registration never arrived. Wrong, and misdiagnosed.
		//
		// A false PASS is not reachable either way: the secret travels only in MMDS
		// V2, which is session-token gated and per-interface, so nothing else on the
		// bridge can learn it.
		if !strings.Contains(string(body), "jit="+secret) {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		select {
		case report <- string(body):
		default:
		}

		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// SERVE'S FAILURE IS REPORTED RATHER THAN DISCARDED. Swallowed, a listener that
	// died on startup left the verification waiting out its full deadline and then
	// blaming the image for not reporting to a socket nobody was reading.
	failed := make(chan error, 1)

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	return srv, ln.Addr().String(), failed, nil
}

// checkGuestReport turns what the guest said into a verdict.
func checkGuestReport(body, secret string) error {
	fmt.Println()

	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		fmt.Println("  " + strings.TrimSpace(line))
	}

	fmt.Println()

	var failures []string

	// THE REGISTRATION FIRST, because it is the one that proves the report is about
	// this launch. Everything else could be true of any guest on the host.
	if !strings.Contains(body, "jit="+secret) {
		failures = append(failures, "the registration did not reach the command's environment, "+
			"so a real job would start no runner")
	}

	if !strings.Contains(body, "whoami=runner") {
		failures = append(failures, "the command did not run as the unprivileged runner account")
	}

	if !hasVersion(body, "runner=") {
		failures = append(failures, "the actions runner binary did not report a version, so a "+
			"job would fail at startup (a debootstrap rootfs missing libicu looks exactly "+
			"like this)")
	}

	if !hasVersion(body, "docker=") {
		failures = append(failures, "the docker daemon did not answer on this kernel")
	}

	if !hasDockerStorageDriver(body, "overlay2") {
		failures = append(failures, "docker is not using overlay2, so pulled image content can "+
			"bypass the independently fenced /var/lib/docker cache")
	}

	if !hasBuildxVersion(body) {
		failures = append(failures, "the Docker Buildx CLI plugin did not report a version")
	}

	if !hasVersion(body, "compose=") {
		failures = append(failures, "the Docker Compose CLI plugin did not report a version")
	}

	if strings.Contains(body, "container=0") {
		failures = append(failures, "docker did not run a container (this one needs egress to a "+
			"registry, so it can also mean the bridge has none)")
	}

	if len(failures) > 0 {
		return fmt.Errorf("billet images verify: this image cannot run a job:\n  - %s",
			strings.Join(failures, "\n  - "))
	}

	fmt.Println("this image boots, takes its registration from the metadata service, runs the")
	fmt.Println("actions runner and runs a container.")

	return nil
}

// hasVersion reports whether a field of the guest's report starts with a digit.
//
// NOT A PREFIX MATCH ON TODAY'S MAJOR. `runner=2.` and `docker=2` were both true of
// the versions in front of me and would turn the first runner 3.x or Docker 30.x
// into a weekly gate that fails until somebody edits this file — a false alarm about
// a healthy image, which is the failure this whole command exists to avoid producing.
//
// What actually distinguishes a working answer from a broken one is that the program
// ANSWERED: a missing binary or a missing shared library leaves the field empty or
// carrying an error message, and neither begins with a digit.
func hasVersion(body, field string) bool {
	_, after, found := strings.Cut(body, field)
	if !found {
		return false
	}

	value, _, _ := strings.Cut(after, "\n")

	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}

	return value[0] >= '0' && value[0] <= '9'
}

// hasDockerStorageDriver reports whether Docker named the backend required by
// the image-store cache. The version and driver share one probe line so another
// report field cannot satisfy this check accidentally.
func hasDockerStorageDriver(body, want string) bool {
	_, after, found := strings.Cut(body, "docker=")
	if !found {
		return false
	}
	line, _, _ := strings.Cut(after, "\n")
	for _, field := range strings.Fields(line) {
		if field == "storage="+want {
			return true
		}
	}

	return false
}

// hasBuildxVersion recognises upstream and distribution-packaged Buildx output.
// Upstream prefixes the second field with v; Ubuntu deliberately does not.
func hasBuildxVersion(body string) bool {
	_, after, found := strings.Cut(body, "buildx=")
	if !found {
		return false
	}
	line, _, _ := strings.Cut(after, "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	version := strings.TrimPrefix(fields[1], "v")

	return version != "" && version[0] >= '0' && version[0] <= '9'
}

// verifyDeploymentID is the identity this machine's microVMs are owned by.
//
// THE SAME RULE `billet node` FOLLOWS, deliberately reusing it rather than agreeing
// with it by luck: the certificate outranks the config file, a `server:` section
// answers when there is no certificate, and a node that has neither can only be
// speaking for its own state directory.
//
// It decides which jails this command can see, so a verification that derived it
// differently would either find nothing of its own or claim somebody else's.
//
// AND IT IS PARSED BEFORE IT IS RETURNED, because one of the two sources does not
// parse it. `state.DeploymentID` reads a file it validates; a bundle's answer is a
// certificate's organization field, which nothing between there and here looks at.
// Everything this command does with the value is derived from it — the probe's
// owner, its lease, the lock's filename — so an unparsed one produces derivations
// that all look well-formed, and the first thing that would object is a filename.
func verifyDeploymentID(cfg *config.Config) (string, error) {
	// A bundle is proof issued BY the control plane. Loading it is best-effort here:
	// a machine that has not enrolled yet can still verify an image, and refusing on
	// that would make this command need a control plane to check a local artifact.
	bundle, err := nodeBundle(cfg)
	if err != nil {
		// A machine that has not enrolled yet can still verify an image, and refusing
		// here would make a local check depend on a control plane. The next rule
		// answers instead.
		bundle = nil
	}

	deployment, err := nodeDeploymentID(cfg, bundle)
	if err != nil {
		return "", err
	}

	if deployment == "" {
		// Its own directory is the only answer available — the same fallback the node
		// documents at node.state_dir.
		if deployment, err = state.DeploymentID(cfg.Node.StateDir); err != nil {
			return "", err
		}
	}

	if err := deploymentid.Validate(deployment); err != nil {
		return "", fmt.Errorf("billet images verify: this machine's deployment identity is "+
			"not one billet mints, so nothing here can say which microVMs are its own: %w", err)
	}

	return deployment, nil
}

// cmdImagesPromote and cmdImagesUnpromote are the manual half of promotion.
//
// THE AUTOMATIC PATH IS `verify --record`, and this exists for taking a verified
// generation back or deliberately restoring one that already records this binary's
// guest contract. Rollback is one command against the cluster rather than an edit on
// every node.
func cmdImagesPromote(ctx context.Context, args []string, verified bool) error {
	name := "billet images promote"
	if !verified {
		name = "billet images unpromote"
	}

	fs := newFlagSet(name)
	cfgPath := addConfigFlag(fs)

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if rest == "" {
		return fmt.Errorf("usage: %s <image@generation>", name)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New(name + ": this config names no ceph cluster")
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	if verified {
		if err := store.MarkVerified(ctx, rest, firecracker.GuestContract, time.Now()); err != nil {
			return err
		}

		fmt.Printf("%s is verified; a tier naming @%s will boot it\n", rest, ceph.Verified)

		return nil
	}

	if err := store.UnmarkVerified(ctx, rest); err != nil {
		return err
	}

	newest, found, err := store.NewestVerifiedForContract(ctx, rest, firecracker.GuestContract)
	if err != nil {
		return err
	}

	// SAYING WHAT HAPPENS NEXT, because withdrawing a verification is done in a hurry
	// and the question immediately after it is "so what boots now".
	if found {
		fmt.Printf("%s is no longer verified; @%s now resolves to %s\n",
			rest, ceph.Verified, newest.Name)

		return nil
	}

	fmt.Printf("%s is no longer verified, and NO generation is — a tier naming @%s now has "+
		"nothing to boot\n", rest, ceph.Verified)

	return nil
}

// cmdImagesReap removes generations nothing needs.
//
// SAFE FOR RUNNING JOBS BY CONSTRUCTION, which is the measurement the whole design
// rests on: clone v2 removes a snapshot with a live child, returns 0, and the child
// stays usable. So this never has to ask whether a generation is in use — retention
// is only about what might still be BOOTED, and there is no liveness check here to
// get wrong.
//
// THE PLAN AND THE ACTION SHARE ONE FUNCTION. A `--dry-run` computed by different
// code than the operation is a preview that eventually stops describing it, which
// for an irreversible command against a cluster is the property most worth having.
func cmdImagesReap(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images reap")
	cfgPath := addConfigFlag(fs)
	keep := fs.Int("keep", 3,
		"how many VERIFIED generations to leave per guest contract, newest first")
	dryRun := fs.Bool("dry-run", false, "print what would be removed and remove nothing")
	kernelDir := fs.String("kernel-dir", "",
		"where pulled kernels are kept; orphans there are reaped too (default: node config)")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images reap: this config names no ceph cluster")
	}

	image := rest
	if image == "" {
		image, err = firecrackerTierImage(cfg)
		if err != nil {
			return err
		}
	}

	if image == "" {
		return errors.New("billet images reap: no image given and no firecracker tier names one")
	}

	store, err := openReapStore(cfg)
	if err != nil {
		return err
	}

	dir := *kernelDir
	if dir == "" {
		dir = nodeKernelDir(cfg)
	}

	return runImagesReap(ctx, store, cfg, image, dir, *keep, *dryRun)
}

// reapStore is what a reap needs from the cluster.
//
// AN INTERFACE SO THE BOUNDARY IS TESTABLE, for the reason `generationPublisher` in
// imagespull.go gives: the property that matters is not what any of these calls does
// but that the KERNEL DIRECTORY IS HELD ACROSS ALL OF THEM, and that is only
// observable if a test can ask, from inside one of them, whether the lock is taken.
type reapStore interface {
	Generations(ctx context.Context, image string) ([]ceph.Generation, error)
	VerifiedGenerations(ctx context.Context, image string) (map[string]bool, error)
	GenerationGuestContracts(ctx context.Context, image string) (map[string]string, error)
	Reap(
		ctx context.Context,
		image string,
		plan []ceph.Reapable,
		retention ceph.Retention,
	) ([]string, error)
	Images(ctx context.Context) ([]string, error)
	NeededKernels(
		ctx context.Context,
		image string,
		generations []ceph.Generation,
	) (map[string]bool, int, error)
}

// openReapStore builds the cluster client this config names.
var openReapStore = func(cfg *config.Config) (reapStore, error) {
	return ceph.New(*cfg.Node.Ceph)
}

// runImagesReap is the reap itself, with the cluster behind an interface.
//
// THE KERNEL DIRECTORY IS HELD ACROSS BOTH HALVES, and holding it across the
// GENERATION half as well is what gives the two commands one order: this lock is
// always taken OUTSIDE the Ceph publish lock. That is not tidiness. Taken only
// around the kernel half, a reap holding the publish lock while a pull holds the
// kernel lock makes that pull's ImportGeneration fail outright, because
// TakePublishLock never waits -- so the narrower scope closes the orphan-reap race
// and leaves a pull that a reap can still break.
//
// Nothing takes the publish lock and then wants this one, which is what makes the
// order safe to state: RecordVerification, MarkVerified and UnmarkVerified take the
// publish lock and never touch the kernel directory, and `billet images pull
// --verify` releases this lock before it verifies.
func runImagesReap(
	ctx context.Context,
	store reapStore,
	cfg *config.Config,
	image, kernelDir string,
	keep int,
	dryRun bool,
) error {
	// A DRY RUN TAKES NO LOCK. It deletes nothing, so there is nothing to exclude,
	// and nothing a concurrent pull can make wrong beyond a preview being out of date
	// by the time somebody reads it -- which is the same reason the restore planner is
	// deliberately lock-free. What it would cost is real: taking a lock means creating
	// a file, so a preview would start refusing on a read-only kernel directory over
	// an operation that changes nothing, which is the failure ADR-005 names.
	//
	// The plan that is ACTED on is the one computed below, inside the exclusion.
	if !dryRun {
		lock, err := takeKernelDirLock(ctx, kernelDir, "collect kernels")
		if err != nil {
			return err
		}

		defer func() {
			if err := lock.release(); err != nil {
				fmt.Printf("warning: the kernel directory lock was not released: %v\n", err)
			}
		}()
	}

	all, err := store.Generations(ctx, image)
	if err != nil {
		return err
	}

	verified, err := store.VerifiedGenerations(ctx, image)
	if err != nil {
		return err
	}

	contracts, err := store.GenerationGuestContracts(ctx, image)
	if err != nil {
		return err
	}

	// EVERY TIER'S IMAGE, not just the one being reaped: a deployment can pin
	// several, and a generation kept for one tier is kept.
	pinned := make([]string, 0, len(cfg.Tiers))
	for i := range cfg.Tiers {
		if image := cfg.Tiers[i].ImageFor(config.ProviderFirecracker); cfg.Tiers[i].AcceptsProvider(config.ProviderFirecracker) && image != "" {
			pinned = append(pinned, image)
		}
	}

	retention := ceph.Retention{Keep: keep, Pinned: pinned}
	plan := ceph.PlanReap(all, verified, contracts, retention)

	for _, item := range plan {
		if item.Reason != "" {
			fmt.Printf("  keep    %s  (%s)\n", item.Generation.Name, item.Reason)
		}
	}

	removing := 0

	for _, item := range plan {
		if item.Reason == "" {
			removing++

			fmt.Printf("  remove  %s\n", item.Generation.Name)
		}
	}

	switch {
	case removing == 0:
		fmt.Println("no generation needs reaping")
	case dryRun:
		fmt.Printf("\n%d generation(s) would be removed; this was a dry run\n", removing)
	default:
		removed, reapErr := store.Reap(ctx, image, plan, retention)

		fmt.Printf("\nremoved %d generation(s)\n", len(removed))

		if reapErr != nil {
			return reapErr
		}
	}

	// KERNELS ARE REAPED WHETHER OR NOT A GENERATION WAS.
	//
	// An orphaned kernel does not require a generation to have been removed just
	// now: one reaped on an earlier run, or a pull whose generation was never kept,
	// leaves a kernel behind. Returning early when no generation needs reaping --
	// which is the common case -- would mean the kernels were never collected at
	// all, and the directory grows by 46MB a week regardless.
	return reapKernels(ctx, store, cfg, kernelDir, dryRun)
}

// reapKernels removes pulled kernels no surviving generation is paired with.
//
// EVERY IMAGE IN THE POOL IS CONSULTED, not just the one being reaped. The kernel
// directory is shared and the pool is not one image, so gathering from a single
// image would let `billet images reap ubuntu-2404-x64` delete a kernel that some
// other image's generations are paired with -- and the failure lands on the next
// job that boots the other image, with nothing connecting it to the reap.
//
// READ AFTER THE GENERATIONS ARE REAPED, deliberately. The needed set has to
// describe what SURVIVES: computed first, it would keep the kernel of a generation
// that is about to be removed.
//
// AND READ UNDER THE KERNEL DIRECTORY LOCK, which runImagesReap holds around this
// whole call. The needed set is a statement about the generations that exist at the
// moment it is read, so computing it outside the lock and deleting inside one would
// act on an answer a concurrent pull has already made wrong -- which is why taking
// the lock around the unlinks alone would not have closed the race.
func reapKernels(
	ctx context.Context,
	store reapStore,
	cfg *config.Config,
	kernelDir string,
	dryRun bool,
) error {
	images, err := store.Images(ctx)
	if err != nil {
		return err
	}

	needed := map[string]bool{}
	total, unknown := 0, 0

	for _, name := range images {
		generations, err := store.Generations(ctx, name)
		if err != nil {
			return err
		}

		imageNeeded, imageUnknown, err := store.NeededKernels(ctx, name, generations)
		if err != nil {
			return err
		}

		for kernel := range imageNeeded {
			needed[kernel] = true
		}

		total += len(generations)
		unknown += imageUnknown
	}

	removed, err := reapKernelDir(kernelDir, needed, total, unknown,
		configuredKernelName(cfg, kernelDir), dryRun)
	if err != nil {
		return err
	}

	if len(removed) == 0 {
		return nil
	}

	verb := "removed"
	if dryRun {
		verb = "would remove"
	}

	fmt.Printf("\n%s %d kernel(s) no generation is paired with:\n", verb, len(removed))

	for _, name := range removed {
		fmt.Printf("  %s\n", name)
	}

	return nil
}

// nodeKernelDir is where this node keeps managed kernels.
//
// ONE ANSWER FOR THE WHOLE PROCESS. The launch resolves a generation's recorded
// kernel against this directory, so anything that RECORDS a pairing has to
// classify against the same one -- otherwise verify calls a kernel managed while
// the launch looks elsewhere, and the pairing names a file nothing will find.
func nodeKernelDir(cfg *config.Config) string {
	if cfg.Node != nil && cfg.Node.Firecracker != nil {
		if dir := strings.TrimSpace(cfg.Node.Firecracker.KernelDir); dir != "" {
			return dir
		}
	}

	return DefaultKernelDir
}

// configuredKernelName is the managed kernel this node is set to boot, if it is
// one billet is responsible for.
//
// SHARED BY THE REAPER AND BY VERIFICATION, because the two must agree about what
// "managed" means. The reaper protects this file from deletion; verification
// records it as a generation's pairing. If they disagreed, one would record a name
// the other would happily delete.
//
// EMPTY WHEN THE CONFIGURED KERNEL LIVES OUTSIDE THE MANAGED DIRECTORY, because
// then it is outside this reaper's authority entirely -- and treating an unrelated
// path as "protected" would silently protect a file of the same base name that IS
// managed.
func configuredKernelName(cfg *config.Config, kernelDir string) string {
	if cfg.Node == nil || cfg.Node.Firecracker == nil {
		return ""
	}

	configured := strings.TrimSpace(cfg.Node.Firecracker.KernelImage)
	if configured == "" {
		return ""
	}

	// SYMLINKS ARE RESOLVED, NOT JUST CLEANED. filepath.Abs makes a path absolute
	// and does NOT follow links, which an earlier comment here claimed it did -- so
	// a stable `current` symlink pointing at one kernel could be recorded as the
	// pairing and then retargeted, proving kernel A and booting kernel B. The
	// pairing has to name the file, not a name that points at a file.
	//
	// A path that cannot be resolved is not managed: EvalSymlinks fails when the
	// target does not exist, and a configured kernel that is not there is a launch
	// failure rather than something to record.
	resolvedDir, err := filepath.EvalSymlinks(kernelDir)
	if err != nil {
		return ""
	}

	resolved, err := filepath.EvalSymlinks(configured)
	if err != nil {
		return ""
	}

	if filepath.Dir(resolved) != resolvedDir {
		return ""
	}

	return filepath.Base(resolved)
}

// cmdImagesList shows what the fleet's image state actually is.
//
// THE QUESTION THIS ANSWERS IS "WHAT IS MY FLEET BOOTING", and until now answering
// it took three rbd commands and knowing which metadata keys billet writes. That is
// the first thing somebody wants when a job boots the wrong image, and the last
// thing they should have to reconstruct.
//
// A TIER SAYING `@verified` IS NOT AN ANSWER to what it boots, so this resolves it.
func cmdImagesList(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images list")
	cfgPath := addConfigFlag(fs)

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images list: this config names no ceph cluster, so there is " +
			"nothing to list")
	}

	image := rest
	if image == "" {
		image, err = firecrackerTierImage(cfg)
		if err != nil {
			return err
		}
	}

	if image == "" {
		return errors.New("billet images list: no image given and no firecracker tier names one")
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	name, _, _ := strings.Cut(image, "@")

	all, err := store.Generations(ctx, image)
	if err != nil {
		return err
	}

	verified, err := store.VerifiedGenerations(ctx, image)
	if err != nil {
		return err
	}

	current, hasCurrent, err := store.NewestVerifiedForContract(ctx, image, firecracker.GuestContract)
	if err != nil {
		return err
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Built.After(all[j].Built) })

	fmt.Println(name)

	if len(all) == 0 {
		fmt.Println("  (no generations published)")
	}

	for _, gen := range all {
		runner, known, err := store.RunnerVersion(ctx, name+"@"+gen.Name)
		if err != nil {
			return err
		}

		// UNKNOWN RATHER THAN BLANK. A generation published before billet recorded
		// this, or by hand, has nothing to say — and an empty column reads as though
		// the question had been asked and answered.
		runnerText := "runner unknown"
		if known {
			runnerText = "runner " + runner
		}

		marks := ""
		if verified[gen.Name] {
			marks = "  verified"
		}

		if hasCurrent && gen.Name == current.Name {
			marks += "  <- @" + ceph.Verified
		}

		fmt.Printf("  %s  %-8s  %-16s%s\n", gen.Name,
			shortAge(time.Since(gen.Built)), runnerText, marks)
	}

	return listTierImages(ctx, store, cfg)
}

// listTierImages says which generation each tier will actually boot.
func listTierImages(ctx context.Context, store *ceph.Client, cfg *config.Config) error {
	shown := false

	for i := range cfg.Tiers {
		tier := cfg.Tiers[i]
		image := tier.ImageFor(config.ProviderFirecracker)
		if image == "" || !tier.AcceptsProvider(config.ProviderFirecracker) {
			continue
		}

		if !shown {
			fmt.Println("\ntiers")

			shown = true
		}

		resolved, err := store.ResolveGeneration(ctx, image, firecracker.GuestContract)
		if err != nil {
			// NOT FATAL, and printed rather than returned: a tier that cannot resolve
			// is exactly what somebody is running this command to find out about, and
			// stopping at the first one would hide the others.
			fmt.Printf("  %-20s %s -> cannot resolve: %v\n", tier.Label, image, err)

			continue
		}

		if resolved == image {
			fmt.Printf("  %-20s %s\n", tier.Label, image)

			continue
		}

		fmt.Printf("  %-20s %s -> %s\n", tier.Label, image, resolved)
	}

	return nil
}

// shortAge renders a duration the way somebody reads it aloud.
func shortAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
