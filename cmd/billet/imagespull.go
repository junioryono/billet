package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs" // aliased: `fs` in this package is the flag set every command builds
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/store/ceph"
)

// DefaultStagingDir is where a pull unpacks before it imports.
//
// /var/tmp RATHER THAN /tmp, AND THIS IS NOT PEDANTRY. /tmp is tmpfs on most
// modern distributions — it is RAM — and a guest image decompresses to four
// gigabytes. Staging there on a machine with 8GB would either exhaust memory or
// push the box into swap partway through an import that holds a cluster-wide
// lock. /var/tmp is disk-backed by convention and survives across reboots, which
// is also what the FHS says it is for.
const DefaultStagingDir = "/var/tmp/billet-images"

// DefaultKernelDir is kept as the CLI package's name for the shared config
// default. Image pull, verification, and provider launch must resolve one place.
//
// DURABLE, UNLIKE THE STAGING DIRECTORY. The kernel and the root filesystem are a
// matched pair -- a generation booted with a different kernel fails in the middle
// of somebody's job -- so the kernel has to outlive the pull that fetched it, for
// as long as any generation built against it can still be booted.
//
// The first version of this reported the kernel's path inside the staging
// directory, which is removed when the pull finishes. It told the operator to
// point node.firecracker.kernel_image at a file it had just deleted. Found by
// running it against a real cluster; nothing in the code could have said so.
const DefaultKernelDir = config.DefaultKernelDir

// generationPublisher is what a pull needs from the cluster: the one call that makes
// this machine's work visible to every other node.
//
// AN INTERFACE SO THE BOUNDARY IS TESTABLE. The property that matters is not what
// ImportGeneration does but WHEN the pull is allowed to reach it — never before the
// kernel it will name is durable on this disk — and that is only observable if a
// test can fail a local step and then ask whether anything remote was attempted.
type generationPublisher interface {
	ImportGeneration(
		ctx context.Context,
		image, rawPath, runnerVersion, kernel, guestContract string,
		now time.Time,
	) (string, error)
}

// openGenerationPublisher builds the cluster client this config names.
var openGenerationPublisher = func(cfg *config.Config) (generationPublisher, error) {
	return ceph.New(*cfg.Node.Ceph)
}

// kernelInstaller is the durable install the pull performs before it publishes.
//
// A PACKAGE VARIABLE SO A TEST CAN BREAK ONE STEP OF THE REAL COMMAND. Every failure
// it stands in for — a full disk, a read-only remount, an I/O error on fsync — is
// real and none can be provoked from a test otherwise, and the thing being proved is
// what cmdImagesPull does NEXT when one happens.
var kernelInstaller = durablefile.Installer{}

// cmdImagesPull fetches a published guest image and publishes it as a generation.
//
// THE STAGES ARE SEPARATE ON PURPOSE: fetch, verify, unpack, import. The obvious
// shape — streaming the download straight into `rbd import` — imports unverified
// bytes into shared storage, where undoing it is a cluster operation rather than
// deleting a file. Staging costs one disk write and makes every failure before
// the import a no-op.
func cmdImagesPull(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images pull")
	cfgPath := addConfigFlag(fs)

	from := fs.String("from", "",
		"pull from a directory holding manifest.json and its assets, instead of over the network")
	source := fs.String("source", "",
		"where to fetch from, overriding the config and the built-in default")
	staging := fs.String("staging-dir", DefaultStagingDir,
		"where to unpack before importing; needs room for the decompressed image")
	keep := fs.Bool("keep-staging", false,
		"leave the downloaded and decompressed files behind for inspection")
	kernelDir := fs.String("kernel-dir", "",
		"where to keep the kernel this image is paired with (default: the node config)")
	allowStale := fs.Bool("allow-stale", false,
		"import even when github is refusing the baked runner; also skips asking github at all")
	verify := fs.Bool("verify", false,
		"boot the imported generation and promote it only after the guest proves it works")
	resultFile := fs.String("result-file", "",
		"write the exact imported generation here after every requested verification succeeds")
	signingIdentity := fs.String("signing-identity", "",
		"certificate SAN pattern a valid signature must carry (for a non-default source)")
	signingIssuer := fs.String("signing-issuer", "",
		"OIDC issuer that certificate must come from")
	skipSignature := fs.Bool("skip-signature-verification", false,
		"import without proving who published the manifest; for a source trusted by other means")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if *resultFile != "" {
		if err := os.Remove(*resultFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("billet images pull: cannot clear stale result file %s: %w",
				*resultFile, err)
		}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// THE TART BACKEND ANSWERS THE SAME QUESTION DIFFERENTLY, and it is checked
	// before the Ceph requirement below — a tart node has no cluster, so without
	// this it was told to configure storage it must not have.
	if cfg.Node != nil && cfg.Node.Provider == config.ProviderTart {
		return pullTartImages(ctx, cfg, rest)
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images pull: this config names no ceph cluster, so there is " +
			"nowhere to import an image to")
	}

	// RESOLVED FROM THE NODE CONFIG, NOT FROM THE CONSTANT, and this used to default
	// straight to DefaultKernelDir -- which ignored node.firecracker.kernel_dir while
	// the reaper, configuredKernelName and the LAUNCH all honour it. A host that set
	// that key had its kernels installed where the launch does not look, so every
	// microVM on the new generation failed to start, and where the reaper does not
	// look either, so they accumulated forever. The ansible host role runs this
	// command with no --kernel-dir, so the packaged upgrade path reached it.
	//
	// nodeKernelDir is the one answer for the whole process, and it says so.
	if *kernelDir == "" {
		*kernelDir = nodeKernelDir(cfg)
	}

	image := rest
	if image == "" {
		configured, err := firecrackerTierImages(cfg)
		if err != nil {
			return err
		}
		if len(configured) > 1 {
			return fmt.Errorf("billet images pull: this deployment names %d distinct firecracker images; give the bare image name to refresh explicitly",
				len(configured))
		}
		if len(configured) == 1 {
			image = configured[0]
		}
		if name, generation, found := strings.Cut(image, "@"); found && generation == ceph.Verified {
			// A TIER NAMING @verified IS THE ORDINARY AUTOMATIC-UPDATE SHAPE. The pull
			// publishes a new generation of that image and verification advances the
			// alias; requiring an operator to repeat the bare name defeats the role's
			// ability to prepare an upgrade without deployment-specific knowledge.
			image = name
		}
	}

	if image == "" {
		return errors.New("billet images pull: no image name given and no firecracker tier " +
			"names one")
	}

	// A GENERATION IS ASSIGNED BY THE IMPORT, never accepted from the caller: it
	// records when this cluster published the image, and a caller-chosen name would
	// let two different images share one.
	if strings.Contains(image, "@") {
		return fmt.Errorf("billet images pull: %q names a generation, and a pull assigns its "+
			"own; give the image name alone", image)
	}

	manifest, dir, cleanup, err := stageImage(ctx, cfg, stageOptions{
		from:     *from,
		source:   *source,
		staging:  *staging,
		identity: *signingIdentity,
		issuer:   *signingIssuer,
		skipSig:  *skipSignature,
	})
	if err != nil {
		return err
	}

	if !*keep {
		defer cleanup()
	}

	if err := manifest.Usable(firecracker.GuestContract, hostArch()); err != nil {
		return err
	}

	// REFUSED ONLY ON EVIDENCE THIS CAN ACTUALLY ESTABLISH. An image whose runner is
	// past github's window produces microVMs that register and are never given work,
	// which is a confusing failure to debug and a pointless one to import — so the
	// baked version is resolved against the release history, and a proved expiry is
	// a refusal. The override exists because a deployment recovering from an outage
	// may genuinely want the newest thing that exists, expired or not.
	//
	// THIS USED TO REFUSE AT built_at + 30 DAYS, which is not evidence about github
	// in either direction: the window opens when a NEWER release appears, so an
	// image built the day a release shipped is still current a year later if nothing
	// else shipped, and one built yesterday around a runner three releases behind is
	// already refused. It rejected images that worked and accepted images that could
	// not.
	if err := refuseExpiredRunner(ctx, manifest.RunnerVersion, *allowStale); err != nil {
		return err
	}

	// AGE IS MAINTENANCE INFORMATION AND SAYS SO. It is a fact about the artifact and
	// about billet's weekly build, not about whether github will queue to it.
	if manifest.Aging(time.Now()) {
		fmt.Printf("note: this image was built %d days ago; a newer one is probably "+
			"published\n\n", int(manifest.Age(time.Now()).Hours()/24))
	}

	raw, err := unpackRootfs(ctx, manifest, dir)
	if err != nil {
		return err
	}

	store, err := openGenerationPublisher(cfg)
	if err != nil {
		return err
	}

	// THE KERNEL DIRECTORY IS HELD FROM BEFORE THE INSTALL THROUGH THE PUBLISH, and
	// the span is the whole point rather than the lock.
	//
	// A concurrent `billet images reap` decides what to delete from the generations
	// that exist AT THE MOMENT IT LOOKS, and until ImportGeneration returns, nothing
	// names the kernel this is about to install -- so the reap correctly concludes it
	// is an orphan and unlinks it, and the generation is then published naming a file
	// that is gone. Taking the lock only around the install would not close that: the
	// window runs to the publish, because that is when the pairing becomes visible.
	// See kernelLock for why the Ceph publish lock could not be used instead and
	// why re-checking the file just before the import is not a fix.
	//
	// IT WAITS. Contention here is another billet doing ordinary maintenance on this
	// directory, not a mistake -- and the ansible host role runs this command as a
	// required step of a transactional upgrade, where a refusal rolls the upgrade
	// back.
	kernelLock, err := takeKernelDirLock(ctx, *kernelDir,
		"install the kernel this generation will name")
	if err != nil {
		return err
	}

	// RELEASED EARLY BELOW AND DEFERRED HERE, which needs no flag of its own because
	// kernelLock.release is idempotent -- the second call finds nothing to give back.
	//
	// WARNED, NOT RETURNED, and the reason is the early release site rather than this
	// one: by then the generation is published, so failing the command would send an
	// operator to re-run a pull that succeeded. The flock is gone with the process
	// either way, so nothing is left held.
	release := func() {
		if err := kernelLock.release(); err != nil {
			fmt.Printf("warning: the kernel directory lock was not released: %v\n", err)
		}
	}

	defer release()

	// INSTALLED BEFORE THE IMPORT, so its name can be recorded inside the lock the
	// import holds. Done afterwards, as it was, the pairing is written while a reap
	// is free to run -- and a generation published without one is taken up by every
	// node and booted against whatever each is configured with.
	//
	// AND THE ORDER IS A DURABILITY BOUNDARY AS WELL AS A LOCKING ONE. Everything
	// below this line is REMOTE and outlives this machine; the kernel is local and
	// must be committed to disk first, because a generation naming a kernel a crash
	// can take away is a cluster-wide record of a pair that does not exist. Nothing
	// between here and ImportGeneration may publish anything.
	kernel, err := installKernel(kernelInstaller, manifest, dir, *kernelDir)
	if err != nil {
		return fmt.Errorf("billet images pull: the kernel could not be kept: %w", err)
	}

	fmt.Printf("importing %s into %s/%s\n", filepath.Base(raw), cfg.Node.Ceph.ImagePool, image)

	generation, err := store.ImportGeneration(ctx, image, raw, manifest.RunnerVersion,
		kernelFileName(manifest), manifest.GuestContract, time.Now())
	if err != nil {
		return err
	}

	// RELEASED AS SOON AS THE GENERATION NAMES THE KERNEL, rather than on the way out
	// of this function. From here the kernel is in every reap's needed set, so nothing
	// can collect it -- and what follows is a verification that boots a microVM for
	// minutes and takes the Ceph publish lock, neither of which this may be held
	// across.
	release()

	fmt.Printf("\npublished %s@%s (runner %s, kernel %s)\n",
		image, generation, manifest.RunnerVersion, manifest.Kernel.Version)
	exact := image + "@" + generation

	if *verify {
		fmt.Printf("\nboot-verifying %s before promotion\n", exact)

		if err := cmdImagesVerify(ctx, []string{"--config", *cfgPath, exact}); err != nil {
			return err
		}
	} else {
		fmt.Println()
		fmt.Println("Nothing boots it yet. Verify it, which marks it so `@verified` resolves to it:")
		fmt.Println()
		fmt.Printf("    billet images verify %s\n", exact)
	}

	if *resultFile != "" {
		if err := writeImageResult(*resultFile, exact); err != nil {
			return err
		}
	}

	fmt.Println()
	fmt.Printf("The kernel it is paired with is kept at:\n\n    %s\n\n", kernel)
	fmt.Println("Point node.firecracker.kernel_image at that path. The two are a matched pair,")
	fmt.Println("and a guest booted with a different kernel fails in the middle of somebody's job.")

	return nil
}

// refuseExpiredRunner decides whether the runner baked into an image is one GitHub
// will still hand jobs to.
//
// THREE ANSWERS, AND ONLY ONE OF THEM REFUSES. Proved past the ordinary window is a
// refusal; inside it is a note; and "could not find out" — no egress, a rate limit,
// a version older than the history billet reads — is said out loud and lets the
// import proceed. A machine with no route to github must not have its image refused
// on the strength of a question nobody could answer, and the previous rule, which
// refused from the image's own build date, was exactly that dressed up as evidence.
//
// AND IT IS THE ORDINARY WINDOW. GitHub may enforce a critical security release at
// once, so a pass here is the best mechanical estimate rather than a promise.
func refuseExpiredRunner(ctx context.Context, version string, allowStale bool) error {
	if allowStale {
		// NOT ASKED AT ALL. The answer could not change anything, and an air-gapped
		// host should not pay a timeout to be told what it already said it accepts.
		fmt.Println("note: --allow-stale, so this did not ask github whether the baked " +
			"runner is still accepted")
		fmt.Println()

		return nil
	}

	fresh, err := resolveRunnerFreshness(ctx, nil, version)
	if err != nil {
		fmt.Printf("note: cannot determine whether runner %s is still accepted by github, "+
			"so this import is not judging it: %v\n\n", version, err)

		return nil
	}

	// THE MODEL'S SPELLING, for the reason Freshness.Installed gives: it answered
	// about the version it normalized, and a message naming a different one
	// attributes the verdict to something else.
	if fresh.Installed != "" {
		version = fresh.Installed
	}

	now := time.Now()

	switch {
	case fresh.Expired(now):
		bound := ""
		if !fresh.InstalledKnown {
			bound = fmt.Sprintf(" (%s is older than the history billet reads, so that is the "+
				"latest the window could have closed)", version)
		}

		return fmt.Errorf("this image bakes runner %s, and github stopped queueing jobs to "+
			"it on %s%s: %s was published %s and started the ordinary 30-day window. "+
			"Importing it would produce microVMs that register and are never given work"+
			"\n\nPass --allow-stale to import it anyway",
			version, fresh.Deadline().Format(time.DateOnly), bound,
			fresh.FirstNewer, fresh.FirstNewerPublished.Format(time.DateOnly))

	// BEHIND WITH NO WINDOW TO COUNT, which is not a refusal: nothing says github has
	// stopped queueing to it, only that a higher release exists and predates it.
	case fresh.BehindWithoutAWindow():
		fmt.Printf("note: runner %s is behind %s, which was published before it — so it "+
			"was already available when this runner shipped and there is no ordinary "+
			"window to count\n\n", version, fresh.Latest)

	// SAME TWO NON-VERDICTS AS `runner check`, and here they are a NOTE rather than a
	// refusal: the expiry above is the only thing this proves, and an import must not
	// be refused by a question billet could not finish asking.
	case !fresh.InstalledKnown:
		fmt.Printf("note: cannot determine whether runner %s is still accepted: github's "+
			"release history does not name it. The newest release is %s\n\n",
			version, fresh.Latest)

	case !fresh.HistoryComplete:
		fmt.Printf("note: cannot determine whether runner %s is still accepted: billet "+
			"reached the end of the history it reads before the end of github's. The "+
			"newest release is %s\n\n", version, fresh.Latest)

	case fresh.Due(now):
		fmt.Printf("warning: runner %s is inside github's ordinary window until %s (%d days); "+
			"%s is the newest release\n\n",
			version, fresh.Deadline().Format(time.DateOnly),
			int(fresh.Remaining(now).Hours()/24), fresh.Latest)

	case !fresh.Current():
		fmt.Printf("runner %s; %s was published %s, and there are %d days to take it up\n\n",
			version, fresh.FirstNewer, fresh.FirstNewerPublished.Format(time.DateOnly),
			int(fresh.Remaining(now).Hours()/24))
	}

	return nil
}

func writeImageResult(path, image string) error {
	return writeImageResults(path, []string{image})
}

func writeImageResults(path string, images []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("billet images pull: cannot create the result directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".image-result-*")
	if err != nil {
		return fmt.Errorf("billet images pull: cannot stage the result in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("billet images pull: cannot protect the staged result: %w", err)
	}
	for _, image := range images {
		if _, err := fmt.Fprintln(tmp, image); err != nil {
			_ = tmp.Close()

			return fmt.Errorf("billet images pull: cannot write the staged result: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("billet images pull: cannot close the staged result: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("billet images pull: cannot install the result at %s: %w", path, err)
	}

	return nil
}

// stageImage puts the manifest and its assets on local disk, verified.
// stageOptions is what stageImage needs, as a struct because the list had reached
// the length where a caller can transpose two strings and get a working call that
// does the wrong thing.
type stageOptions struct {
	from     string
	source   string
	staging  string
	identity string
	issuer   string
	skipSig  bool
}

func stageImage(
	ctx context.Context,
	cfg *config.Config,
	opts stageOptions,
) (*imagesource.Manifest, string, func(), error) {
	from, staging := opts.from, opts.staging

	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, "", nil, fmt.Errorf("billet images pull: cannot use %s: %w", staging, err)
	}

	// FROM A DIRECTORY, FOR A DEPLOYMENT WITH NO ROUTE TO THE INTERNET. The same
	// manifest, the same digests, the same refusals — only the fetch is skipped.
	// Present from the first release rather than added later, because the seam it
	// needs is the one being built right now.
	if from != "" {
		data, err := os.ReadFile(filepath.Join(from, imagesource.ManifestName))
		if err != nil {
			return nil, "", nil, fmt.Errorf("billet images pull: cannot read a manifest in %s: %w",
				from, err)
		}

		// THE SIGNATURE IS CHECKED HERE TOO, AND THIS WAS MISSING.
		//
		// A sideloaded directory is not more trustworthy than a download -- it is
		// less, because nothing about how it arrived is even in principle
		// observable. And the digests inside a manifest prove nothing about the
		// manifest: whoever supplied the directory chose them. Skipping the
		// signature here left the whole verification chain bypassable by putting
		// files in a folder.
		policy, err := sideloadPolicy(cfg, opts)
		if err != nil {
			return nil, "", nil, err
		}

		var signature []byte

		if policy.Required {
			signature, err = os.ReadFile(filepath.Join(from, imagesource.BundleName))
			if err != nil {
				return nil, "", nil, fmt.Errorf("billet images pull: %s holds no %s, and this "+
					"source requires a signature. Copy it from the release alongside the "+
					"manifest, or pass --skip-signature-verification if this directory is "+
					"trusted by other means: %w", from, imagesource.BundleName, err)
			}
		}

		if err := imagesource.VerifySignature(data, signature, policy); err != nil {
			return nil, "", nil, err
		}

		manifest, err := imagesource.ParseManifest(data)
		if err != nil {
			return nil, "", nil, err
		}

		// COPIED INTO PRIVATE STAGING WHILE BEING HASHED, rather than verified where
		// they sit and read again later.
		//
		// The old verifier hashed each asset BY PATH, and the import reopened it BY
		// PATH some minutes later -- so whoever owns the directory could swap a file,
		// or retarget a symlink, in between. The bytes that reached the cluster would
		// then be bytes nothing checked, without anybody forging a signature. That is
		// the same shape as verifying a download after renaming it, which the network
		// path is careful not to do.
		//
		// The copy is what makes the digest binding, so it is not an optimisation to
		// remove later.
		staged, cleanup, err := copyStagedAssets(from, staging, manifest)
		if err != nil {
			return nil, "", nil, err
		}

		return manifest, staged, cleanup, nil
	}

	src, err := resolveSource(cfg, opts.source)
	if err != nil {
		return nil, "", nil, err
	}
	client := &imagesource.Client{Source: src}
	if err := client.Resolve(ctx); err != nil {
		return nil, "", nil, err
	}
	src = client.Source

	identity, issuer := opts.identity, opts.issuer

	if cfg.Images != nil {
		if identity == "" {
			identity = cfg.Images.SigningIdentity
		}

		if issuer == "" {
			issuer = cfg.Images.SigningIssuer
		}
	}

	policy, err := imagesource.PolicyFor(src, identity, issuer, opts.skipSig)
	if err != nil {
		return nil, "", nil, err
	}

	fmt.Printf("fetching %s\n", src.ManifestURL())

	if policy.Required {
		fmt.Printf("requiring a signature from %s\n", policy.Identity)
	} else {
		fmt.Println("WARNING: importing without verifying who published this manifest")
	}

	manifest, err := client.Manifest(ctx, policy)
	if err != nil {
		return nil, "", nil, err
	}

	dir, err := os.MkdirTemp(staging, "pull-*")
	if err != nil {
		return nil, "", nil, fmt.Errorf("billet images pull: cannot stage in %s: %w", staging, err)
	}

	cleanup := func() { _ = os.RemoveAll(dir) }

	// EVERY ASSET THE MANIFEST NAMES, whichever schema published it. A schema 2
	// image arrives as several parts and is joined by unpackRootfs, which is the
	// only thing that produces a usable root filesystem path -- and which checks
	// the digest of the assembled file, not merely of the pieces.
	for _, asset := range manifest.Downloads() {
		fmt.Printf("downloading %s (%s)\n", asset.Name, humanBytes(asset.Size))

		if _, err := client.Download(ctx, asset, dir); err != nil {
			cleanup()

			return nil, "", nil, err
		}
	}

	return manifest, dir, cleanup, nil
}

// sideloadPolicy decides what a directory on disk must prove.
//
// THE SAME POLICY THE NETWORK PATH USES, resolved against the source the operator
// would otherwise have pulled from -- because a sideloaded copy of billet's own
// image is still billet's own image, and should have to prove it. An operator
// distributing their own builds says so the same way they would for a mirror.
func sideloadPolicy(cfg *config.Config, opts stageOptions) (imagesource.Policy, error) {
	src, err := resolveSource(cfg, opts.source)
	if err != nil {
		return imagesource.Policy{}, err
	}

	identity, issuer := opts.identity, opts.issuer

	if cfg.Images != nil {
		if identity == "" {
			identity = cfg.Images.SigningIdentity
		}

		if issuer == "" {
			issuer = cfg.Images.SigningIssuer
		}
	}

	return imagesource.PolicyFor(src, identity, issuer, opts.skipSig)
}

// resolveSource decides where to fetch from.
//
// THE ORDER IS FLAG, THEN CONFIG, THEN THE BUILT-IN. A deployment that mirrors
// internally says so once in its config; the flag is for a one-off.
func resolveSource(cfg *config.Config, flagValue string) (imagesource.Source, error) {
	if strings.TrimSpace(flagValue) != "" {
		return imagesource.ParseSource(flagValue)
	}

	if cfg.Images != nil && strings.TrimSpace(cfg.Images.Source) != "" {
		return imagesource.ParseSource(cfg.Images.Source)
	}

	return imagesource.DefaultSource(), nil
}

// unpackRootfs joins the published root filesystem and decompresses it if packed.
//
// THE JOIN COMES FIRST AND IS NOT OPTIONAL. A schema 2 image is published as
// parts because GitHub caps a release asset at 2 GiB, and AssembleRootfs is what
// puts them back together AND checks the digest of the result. A schema 1 image
// goes through the same call, which verifies it in place -- so the whole-file
// check runs for both layouts and there is no path here that decompresses bytes
// nothing has vouched for as a complete image.
func unpackRootfs(ctx context.Context, manifest *imagesource.Manifest, dir string) (string, error) {
	img := manifest.RootfsImage()

	if img.Assembled() {
		fmt.Printf("assembling %s from %d parts\n", img.Name, len(img.Parts))
	}

	packed, err := imagesource.AssembleRootfs(dir, img)
	if err != nil {
		return "", fmt.Errorf("billet images pull: %w", err)
	}

	if img.Compression == "" {
		return packed, nil
	}

	// SHELLED OUT, LIKE rbd. This codebase already runs the cluster through the rbd
	// binary rather than a bound library, and the same reasoning applies: a
	// dependency-free binary is easier to reason about than a compression library
	// linked into a control-plane process, and zstd's own tool decompresses in
	// parallel.
	if _, err := exec.LookPath("zstd"); err != nil {
		return "", fmt.Errorf("billet images pull: this image is packed with zstd and the zstd "+
			"command is not installed: %w", err)
	}

	raw := strings.TrimSuffix(packed, ".zst")
	if raw == packed {
		raw = packed + ".raw"
	}

	fmt.Printf("decompressing %s\n", img.Name)

	// -f BECAUSE A RETRY MUST NOT PROMPT. An interrupted pull leaves a partial
	// output, and zstd would otherwise stop to ask about it on a machine nobody is
	// watching.
	cmd := exec.CommandContext(ctx, "zstd", "-d", "-f", "-o", raw, packed)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("billet images pull: could not decompress %s: %w",
			img.Name, err)
	}

	return raw, nil
}

// hostArch is the machine this is running on, spelled as a manifest spells it.
//
// TRANSLATED FROM GO'S NAMES, which are not uname's: Go calls x86-64 "amd64" and
// aarch64 "arm64". A manifest records what `uname -m` says, because that is what
// the build recorded.
func hostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// humanBytes renders a size the way an operator reads one.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// installKernel copies the pulled kernel somewhere it will outlive the pull.
//
// NAMED BY VERSION AND DIGEST. Version alone is not enough: two builds can produce
// the same kernel version from different sources, and silently overwriting one
// with the other would repoint every generation that was verified against the
// first. The digest makes re-pulling the same image idempotent and makes two
// genuinely different kernels two files.
//
// DURABLE BEFORE IT RETURNS, AND THE CALLER DEPENDS ON THAT. What happens next is
// ImportGeneration, which commits Ceph metadata naming this file — so a return here
// is a promise that the name exists on the other side of a power loss. It did not
// used to be one: the mode was set after the sync that would have committed it, and
// nothing flushed the directory at all, which fsync(2) says plainly is required for
// the entry rather than the contents. The sequence lives in durablefile now, so no
// future call site has to remember it.
func installKernel(
	installer durablefile.Installer,
	manifest *imagesource.Manifest,
	from, dir string,
) (string, error) {
	// THE DIRECTORY'S OWN NAME IS COMMITTED TOO. Flushing this directory is what
	// makes the kernel inside it durable and says nothing about the entry naming the
	// directory in its parent -- and on a fresh host nothing else creates it, so a
	// crash could take the whole thing away and leave a published generation naming
	// a kernel that is gone. Same failure, one level up.
	if err := installer.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot use %s: %w", dir, err)
	}

	final := filepath.Join(dir, kernelFileName(manifest))

	// ALREADY THERE IS SUCCESS ONLY IF IT IS ACTUALLY THAT KERNEL.
	//
	// The name carries the digest, so re-pulling the same image or pulling two
	// images that share a kernel must not copy again. But the name is proof of
	// content only if something checks it, and the first version of this checked
	// nothing: a truncated copy from an interrupted run, or a file an operator
	// dropped in, would then be booted by every generation paired with that digest
	// -- silently, because putting the digest in the name is precisely the claim
	// that it identifies the bytes.
	//
	// AND WHAT IT FINDS IS REPAIRED RATHER THAN CERTIFIED, which is three things
	// rather than one. This branch is exactly what a retry after an interrupted
	// install reaches, and the state it left behind can be wrong in ways the content
	// check cannot see: the OLD implementation set the mode after the sync that
	// would have committed it, so a recovered kernel can hold the right bytes at
	// 0600 -- unreadable by whatever boots it -- and nothing had flushed the
	// directory at all, so the name may not be committed either. Returning success
	// here would make the retry that is supposed to fix that the thing that hides
	// it.
	if kept, err := reuseInstalledKernel(installer, final, manifest.Kernel.SHA256); kept || err != nil {
		if err != nil {
			return "", err
		}

		if err := installer.SyncDirectory(dir); err != nil {
			return "", err
		}

		return final, nil
	}

	staged := filepath.Join(from, manifest.Kernel.Name)

	src, err := os.Open(staged)
	if err != nil {
		return "", fmt.Errorf("cannot read the staged kernel: %w", err)
	}

	defer func() { _ = src.Close() }()

	// RE-HASHED ON THE WAY IN, THOUGH THE DOWNLOAD ALREADY CHECKED IT.
	//
	// The kernel was verified when it was fetched, and this reads it from the same
	// staging directory a moment later -- so on the happy path the check is
	// redundant. It is here because "a moment later" is where a bug lives: the
	// decompression step writes into that directory under a name it derives, and
	// when that name collided with the kernel's, `zstd -f` overwrote an
	// already-verified file with root filesystem bytes and this function installed
	// them under a kernel name. The collision is refused now, in the manifest
	// validation that owns every staged name. This is the second line: it costs
	// one pass over fifty megabytes and makes "the kernel is what the manifest
	// says" true at the moment it is installed rather than only at the moment it
	// was downloaded.
	//
	// CHECKED INSIDE THE WRITE, so a mismatch aborts before the file is given the
	// name that says which kernel it is.
	return installer.Install(dir, kernelFileName(manifest), kernelMode, func(w io.Writer) error {
		sum := sha256.New()

		if _, err := io.Copy(io.MultiWriter(w, sum), src); err != nil {
			return fmt.Errorf("cannot copy the kernel: %w", err)
		}

		if got := hex.EncodeToString(sum.Sum(nil)); got != manifest.Kernel.SHA256 {
			return fmt.Errorf("the staged kernel hashes to %s and the manifest publishes %s; "+
				"it was verified when it was downloaded, so something in the staging directory "+
				"overwrote it between then and now", got, manifest.Kernel.SHA256)
		}

		return nil
	})
}

// kernelMode is what an installed kernel must be readable as.
//
// ONE CONSTANT, BECAUSE TWO PLACES DECIDE IT: the install writes it and the repair
// path restores it. Held apart, a repair that "fixed" a kernel to a mode the install
// does not produce would make every retry change the file back and forth.
// TYPED, so it renders as `-rw-r--r--` in a diagnostic rather than as 420.
const kernelMode iofs.FileMode = 0o644

// reuseInstalledKernel decides whether a kernel already on disk is that kernel, and
// leaves it durable if it is.
//
// EVERYTHING IS DONE THROUGH ONE DESCRIPTOR, opened O_NOFOLLOW. Statting a path,
// hashing the path again and then chmod'ing the path is three lookups of a name
// something else may be changing, and this runs as root in a directory a generation
// will name; a symlink under that name would have the check pass against one file
// and the mode applied to another.
//
// It answers false only when there is nothing there at all. Anything else that is
// not a verifiable regular file is an error, because the name is the claim that
// these bytes are that kernel and a caller cannot re-establish it by writing
// another file beside it.
func reuseInstalledKernel(
	installer durablefile.Installer,
	path, want string,
) (bool, error) {
	// O_RDONLY, WHICH IS ENOUGH FOR ALL THREE THINGS THIS DOES. MEASURED on Linux:
	// chmod and fsync both succeed through a read-only descriptor -- a chmod needs
	// ownership rather than write permission -- while O_RDWR FAILS with permission
	// denied on a kernel an operator has left at 0444. Asking for write access billet
	// does not need would refuse a correct artifact, which is the failure ADR-005
	// names, because the next thing anybody does is delete the check.
	// O_NONBLOCK, BECAUSE THE OPEN ITSELF CAN BLOCK FOREVER. MEASURED: opening a
	// FIFO for reading with no writer never returns, so the IsRegular check below --
	// the thing that would reject it -- is never reached, and a pull hangs with
	// nothing able to cancel it, since a filesystem open does not take a context. On
	// a regular file the flag does nothing at all.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("%s exists and cannot be opened as an ordinary file; a "+
			"generation would name it as its kernel: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("cannot inspect %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file, and a generation would name it "+
			"as its kernel", path)
	}

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return false, fmt.Errorf("cannot read %s: %w", path, err)
	}

	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return false, fmt.Errorf("%s already exists and hashes to %s, which is not the "+
			"digest its own name carries; refusing to boot generations against it. Remove "+
			"it if it is a partial copy from an interrupted pull", path, got)
	}

	// THE MODE AND THE FLUSH, IN THAT ORDER, for the reason durablefile gives: a
	// mode set after the sync is a metadata change nothing committed, which is how a
	// kernel installed by the previous implementation can be sitting here at 0600.
	//
	// AND ONLY WHEN IT IS WRONG. A chmod to the mode a file already has changes
	// nothing, so its failure proves nothing about the artifact -- and it CAN fail:
	// MEASURED on a read-only bind mount, chmod to the identical mode returns EROFS
	// while both fsyncs succeed. Attempting it unconditionally would refuse a correct
	// kernel on a read-only kernel directory over an operation that was a no-op,
	// which is the failure ADR-005 names.
	if info.Mode().Perm() != kernelMode {
		if err := installer.SetModeOn(f, kernelMode); err != nil {
			return false, fmt.Errorf("%s is %v and a guest must be able to read it as %v: %w",
				path, info.Mode().Perm(), kernelMode, err)
		}
	}

	if err := installer.SyncFileHandle(f); err != nil {
		return false, fmt.Errorf("cannot flush %s: %w", path, err)
	}

	return true, nil
}

// kernelFileName is what a pulled kernel is called on disk.
//
// ONE FUNCTION, BECAUSE TWO THINGS DEPEND ON THE ANSWER AGREEING: installKernel
// writes this name, and the generation records it so the reaper can decide what is
// still needed. Computed separately, the reaper would compare a value nothing on
// disk is called -- and a reaper that matches nothing either deletes everything or
// nothing, depending on which way it fails.
//
// VERSION AND DIGEST, because the version alone does not identify a file: two
// builds can produce the same kernel version from different sources, and reaping
// on version alone removes a kernel some generation is verified against.
func kernelFileName(manifest *imagesource.Manifest) string {
	return fmt.Sprintf("vmlinux-%s-%s", manifest.Kernel.Version, manifest.Kernel.SHA256[:12])
}

// copyStagedAssets copies a sideloaded directory's assets into private staging,
// verifying each as it goes.
//
// HASHED FROM THE COPY, NOT FROM THE SOURCE. Hashing the operator's file and then
// reading it again later leaves a window in which it can be replaced; hashing what
// was actually written closes it, because that copy is what the import reads and
// nothing else can reach it.
func copyStagedAssets(
	from, staging string,
	manifest *imagesource.Manifest,
) (string, func(), error) {
	dir, err := os.MkdirTemp(staging, "sideload-*")
	if err != nil {
		return "", nil, fmt.Errorf("billet images pull: cannot stage in %s: %w", staging, err)
	}

	cleanup := func() { _ = os.RemoveAll(dir) }

	// THE SAME ASSET SET THE NETWORK PATH FETCHES. A sideloaded schema 2 image is
	// several parts in the operator's directory, and taking only a "rootfs" here
	// would copy nothing and leave the assembly to fail on missing files.
	for _, asset := range manifest.Downloads() {
		if err := copyVerified(filepath.Join(from, asset.Name),
			filepath.Join(dir, asset.Name), asset); err != nil {
			cleanup()

			return "", nil, err
		}
	}

	return dir, cleanup, nil
}

// copyVerified copies one asset and proves the copy is what the manifest named.
func copyVerified(src, dst string, asset imagesource.Asset) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("billet images pull: the manifest names %s and it cannot be read: %w",
			asset.Name, err)
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("billet images pull: cannot stage %s: %w", asset.Name, err)
	}

	defer func() { _ = out.Close() }()

	sum := sha256.New()

	// SIZE+1 SO A LONGER FILE IS AN ERROR rather than a truncation to the promised
	// length, which would let a digest match a prefix of something larger.
	written, err := io.Copy(io.MultiWriter(out, sum), io.LimitReader(in, asset.Size+1))
	if err != nil {
		return fmt.Errorf("billet images pull: cannot copy %s: %w", asset.Name, err)
	}

	if written != asset.Size {
		return fmt.Errorf("billet images pull: %s is %d bytes and the manifest says %d",
			asset.Name, written, asset.Size)
	}

	if got := hex.EncodeToString(sum.Sum(nil)); got != asset.SHA256 {
		return fmt.Errorf("billet images pull: %s hashes to %s and the manifest published %s; "+
			"it is not the file that was signed", asset.Name, got, asset.SHA256)
	}

	return out.Close()
}
