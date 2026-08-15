package main

import (
	"context"
	"errors"
	"fmt"
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
	allowStale := fs.Bool("allow-stale", false,
		"import even if the baked runner is past github's thirty days")

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

	manifest, dir, cleanup, err := stageImage(ctx, cfg, *from, *source, *staging)
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

	fmt.Printf("\npublished %s@%s (runner %s, kernel %s)\n",
		image, generation, manifest.RunnerVersion, manifest.Kernel.Version)
	fmt.Println()
	fmt.Println("Nothing boots it yet. Verify it, which marks it so `@verified` resolves to it:")
	fmt.Println()
	fmt.Printf("    billet images verify %s@%s\n", image, generation)
	fmt.Println()
	fmt.Printf("The kernel it was built against is staged at %s.\n",
		filepath.Join(dir, manifest.Kernel.Name))
	fmt.Println("Point node.firecracker.kernel_image at it; the two are a matched pair and a")
	fmt.Println("guest booted with a different kernel fails in the middle of somebody's job.")

	return nil
}

// stageImage puts the manifest and its assets on local disk, verified.
func stageImage(
	ctx context.Context,
	cfg *config.Config,
	from, sourceFlag, staging string,
) (*imagesource.Manifest, string, func(), error) {
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

	src, err := resolveSource(cfg, sourceFlag)
	if err != nil {
		return nil, "", nil, err
	}

	client := &imagesource.Client{Source: src}

	fmt.Printf("fetching %s\n", src.ManifestURL())

	manifest, err := client.Manifest(ctx)
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
