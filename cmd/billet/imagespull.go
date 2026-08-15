package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
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

// DefaultKernelDir is where pulled kernels are kept.
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
const DefaultKernelDir = "/var/lib/billet/kernels"

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
	kernelDir := fs.String("kernel-dir", DefaultKernelDir,
		"where to keep the kernel this image is paired with")
	allowStale := fs.Bool("allow-stale", false,
		"import even if the baked runner is past github's thirty days")
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

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images pull: this config names no ceph cluster, so there is " +
			"nowhere to import an image to")
	}

	image := rest
	if image == "" {
		image = firecrackerTierImage(cfg)
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

	// REFUSED BY DEFAULT, OVERRIDABLE ON PURPOSE. An image whose runner is past
	// github's thirty days produces microVMs that register and are never given
	// work, which is a confusing failure to debug and a pointless one to import.
	// The override exists because a deployment recovering from an outage may
	// genuinely want the newest thing that exists, stale or not.
	if err := manifest.Stale(time.Now()); err != nil {
		if !*allowStale {
			return fmt.Errorf("%w\n\nPass --allow-stale to import it anyway", err)
		}

		fmt.Printf("warning: %v\n\n", err)
	}

	if manifest.Aging(time.Now()) {
		fmt.Printf("note: this image is %d days old; a newer one is probably published\n\n",
			int(manifest.Age(time.Now()).Hours()/24))
	}

	raw, err := unpackRootfs(ctx, manifest, dir)
	if err != nil {
		return err
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	fmt.Printf("importing %s into %s/%s\n", filepath.Base(raw), cfg.Node.Ceph.ImagePool, image)

	generation, err := store.ImportGeneration(ctx, image, raw, manifest.RunnerVersion, time.Now())
	if err != nil {
		return err
	}

	// INSTALLED BEFORE THE STAGING DIRECTORY IS REMOVED, which is the whole point:
	// the deferred cleanup runs on the way out of this function.
	kernel, err := installKernel(manifest, dir, *kernelDir)
	if err != nil {
		return fmt.Errorf("%s@%s was published, but its kernel could not be kept: %w",
			image, generation, err)
	}

	// RECORDED AGAINST THE GENERATION, like the runner version, because which
	// kernel a generation needs is a property of that generation rather than of the
	// image. Two generations can want different kernels, and an operator pointing
	// one config at both has no other way to know.
	if err := store.SetKernel(ctx, image, generation, kernelFileName(manifest)); err != nil {
		return fmt.Errorf("%s@%s was published, but the kernel it needs could not be "+
			"recorded: %w", image, generation, err)
	}

	fmt.Printf("\npublished %s@%s (runner %s, kernel %s)\n",
		image, generation, manifest.RunnerVersion, manifest.Kernel.Version)
	fmt.Println()
	fmt.Println("Nothing boots it yet. Verify it, which marks it so `@verified` resolves to it:")
	fmt.Println()
	fmt.Printf("    billet images verify %s@%s\n", image, generation)
	fmt.Println()
	fmt.Printf("The kernel it is paired with is kept at:\n\n    %s\n\n", kernel)
	fmt.Println("Point node.firecracker.kernel_image at that path. The two are a matched pair,")
	fmt.Println("and a guest booted with a different kernel fails in the middle of somebody's job.")

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

		if err := verifyLocal(from, manifest); err != nil {
			return nil, "", nil, err
		}

		// NOTHING TO CLEAN UP: these are the operator's files, sitting where the
		// operator put them, and removing them would be a surprising thing for a
		// pull to do.
		return manifest, from, func() {}, nil
	}

	src, err := resolveSource(cfg, opts.source)
	if err != nil {
		return nil, "", nil, err
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

	policy, err := imagesource.PolicyFor(src, identity, issuer, opts.skipSig)
	if err != nil {
		return nil, "", nil, err
	}

	client := &imagesource.Client{Source: src}

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

	for _, asset := range []imagesource.Asset{manifest.Rootfs, manifest.Kernel} {
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

	return imagesource.ParseSource(imagesource.DefaultBaseURL)
}

// verifyLocal checks a sideloaded directory against its manifest.
//
// THE SAME DIGESTS THE NETWORK PATH CHECKS. A file that arrived on a USB stick is
// no more trustworthy than one that arrived over http — less, arguably, since
// nothing about its journey is even in principle observable.
func verifyLocal(dir string, manifest *imagesource.Manifest) error {
	for _, asset := range []imagesource.Asset{manifest.Rootfs, manifest.Kernel} {
		path := filepath.Join(dir, asset.Name)

		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("billet images pull: the manifest names %s and it is not in %s: %w",
				asset.Name, dir, err)
		}

		if info.Size() != asset.Size {
			return fmt.Errorf("billet images pull: %s is %d bytes and the manifest says %d",
				asset.Name, info.Size(), asset.Size)
		}

		if err := imagesource.VerifyFile(path, asset.SHA256); err != nil {
			return err
		}
	}

	return nil
}

// unpackRootfs decompresses the root filesystem if it is packed.
func unpackRootfs(ctx context.Context, manifest *imagesource.Manifest, dir string) (string, error) {
	packed := filepath.Join(dir, manifest.Rootfs.Name)

	if manifest.Rootfs.Compression == "" {
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

	fmt.Printf("decompressing %s\n", manifest.Rootfs.Name)

	// -f BECAUSE A RETRY MUST NOT PROMPT. An interrupted pull leaves a partial
	// output, and zstd would otherwise stop to ask about it on a machine nobody is
	// watching.
	cmd := exec.CommandContext(ctx, "zstd", "-d", "-f", "-o", raw, packed)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("billet images pull: could not decompress %s: %w",
			manifest.Rootfs.Name, err)
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
func installKernel(manifest *imagesource.Manifest, from, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if _, err := os.Stat(final); err == nil {
		if err := imagesource.VerifyFile(final, manifest.Kernel.SHA256); err != nil {
			return "", fmt.Errorf("%s already exists and its content does not match the "+
				"digest its own name carries; refusing to boot generations against it. Remove "+
				"it if it is a partial copy from an interrupted pull: %w", final, err)
		}

		return final, nil
	}

	staged := filepath.Join(from, manifest.Kernel.Name)

	src, err := os.Open(staged)
	if err != nil {
		return "", fmt.Errorf("cannot read the staged kernel: %w", err)
	}

	defer func() { _ = src.Close() }()

	// STAGED AND RENAMED, for the reason the download path does it: a crash partway
	// through a copy would otherwise leave a truncated kernel under a name that
	// says which kernel it is, and nothing would ever check it again.
	tmp, err := os.CreateTemp(dir, ".vmlinux-*")
	if err != nil {
		return "", fmt.Errorf("cannot stage the kernel in %s: %w", dir, err)
	}

	committed := false

	defer func() {
		_ = tmp.Close()

		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		return "", fmt.Errorf("cannot copy the kernel: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("cannot flush the kernel: %w", err)
	}

	if err := tmp.Chmod(0o644); err != nil {
		return "", fmt.Errorf("cannot set the kernel's mode: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cannot close the kernel: %w", err)
	}

	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", fmt.Errorf("cannot place the kernel: %w", err)
	}

	committed = true

	return final, nil
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
