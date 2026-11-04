package images

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/imagesource"
)

// standOn makes hostArch answer machine for the rest of the test. Not parallel:
// hostArch is the package's.
func standOn(t *testing.T, machine string) {
	t.Helper()

	restore := hostArch
	t.Cleanup(func() { hostArch = restore })
	hostArch = func() string { return machine }
}

// archTiers is two firecracker tiers naming one image each, one per architecture.
const archTiers = `  - label: on-x64
    provider: firecracker
    vcpu: 2
    memory: 4GiB
    arch: x64
    image: ubuntu-2404-x64@verified
  - label: on-arm64
    provider: firecracker
    vcpu: 2
    memory: 4GiB
    arch: arm64
    image: ubuntu-2404-arm64@verified
`

// archConfig is the refresh's firecracker node config with archTiers as its
// whole catalogue, which a file naming arch must name on every tier.
func archConfig(t *testing.T) *config.Config {
	t.Helper()

	path := firecrackerRefreshConfig(t, "")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	head, _, found := strings.Cut(string(body), "tiers:\n")
	if !found {
		t.Fatal("the refresh config has no tiers block; the fixture has drifted")
	}

	if err := os.WriteFile(path, []byte(head+"tiers:\n"+archTiers), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return cfg
}

// A NODE PULLS, REFRESHES AND CHECKS ONLY THE IMAGES OF TIERS OF ITS OWN MACHINE,
// in a file naming arch (ADR-022).
func TestANodeTakesUpOnlyTheImagesOfItsOwnMachinesTiers(t *testing.T) {
	for machine, want := range map[string]string{
		"x86_64":  "ubuntu-2404-x64@verified",
		"aarch64": "ubuntu-2404-arm64@verified",
	} {
		t.Run(machine, func(t *testing.T) {
			standOn(t, machine)

			images, err := FirecrackerTierImages(archConfig(t))
			if err != nil {
				t.Fatalf("FirecrackerTierImages: %v", err)
			}

			if len(images) != 1 || images[0] != want {
				t.Errorf("a %s node takes up %v, want only %s", machine, images, want)
			}
		})
	}
}

// AND A PULL OF AN IMAGE ANOTHER MACHINE'S TIERS NAME IS REFUSED, the first import
// into it included, before anything is asked of the cluster or downloaded.
func TestAPullOfAnImageAnotherMachinesTiersNameIsRefused(t *testing.T) {
	standOn(t, "x86_64")
	cfg := archConfig(t)

	err := refuseImageOfAnotherMachine(cfg, "ubuntu-2404-arm64")
	if err == nil || !strings.Contains(err.Error(), "run on aarch64, and this host is x86_64") {
		t.Fatalf("refuseImageOfAnotherMachine = %v, want the arm64 image refused on x86_64", err)
	}

	for _, image := range []string{"ubuntu-2404-x64", "an-image-no-tier-names"} {
		if err := refuseImageOfAnotherMachine(cfg, image); err != nil {
			t.Errorf("%s was refused on x86_64: %v", image, err)
		}
	}
}

// A SIDELOADED DIRECTORY IS READ AS A RELEASE IS: the manifest named for this
// machine, before manifest.json, which is read only when it is all there is.
func TestASideloadReadsTheManifestNamedForThisMachine(t *testing.T) {
	standOn(t, "aarch64")

	fixture := newPullFixture(t)
	named, _ := imagesource.ManifestNamesFor("aarch64")

	if err := os.Rename(filepath.Join(fixture.from, imagesource.ManifestName),
		filepath.Join(fixture.from, named)); err != nil {
		t.Fatal(err)
	}

	// A DECOY manifest.json that would refuse the pull if it were read.
	writeAsset(t, filepath.Join(fixture.from, imagesource.ManifestName), []byte("{not a manifest"))

	usePullSeams(t, fixture.publisher, durablefile.Installer{}, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("a sideload holding the manifest named for this machine was refused: %v", err)
	}

	if got := fixture.publisher.last().machine; got != "aarch64" {
		t.Errorf("imported as %q, want aarch64", got)
	}

	// AND WITH ONLY manifest.json, THAT IS READ.
	fallback := newPullFixture(t)
	usePullSeams(t, fallback.publisher, durablefile.Installer{}, currentRunner)

	if err := fallback.pull(t); err != nil {
		t.Fatalf("a sideload holding only manifest.json was refused: %v", err)
	}
}

// A CHANNEL WHOSE RELEASE CARRIES NO IMAGE FOR THIS MACHINE IS NOTHING TO DO FOR A
// REFRESH, not a fault: final until the channel moves.
func TestARefreshWhoseChannelCarriesNoImageForThisMachineDoesNothing(t *testing.T) {
	standOn(t, "aarch64")
	cfg := archConfig(t)

	restore := fetchImageManifest
	t.Cleanup(func() { fetchImageManifest = restore })
	fetchImageManifest = func(context.Context, cli.Env, *config.Config) (*imagesource.Manifest, error) {
		return nil, fmt.Errorf("%w: lists [x86_64]", imagesource.ErrNoImageForMachine)
	}

	pulled := false
	restorePull := refreshPull
	t.Cleanup(func() { refreshPull = restorePull })
	refreshPull = func(context.Context, cli.Env, string, string) error {
		pulled = true

		return nil
	}

	var out strings.Builder

	err := refreshFirecrackerImages(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, cfg, "", 3, false)
	if err != nil {
		t.Fatalf("a channel with no image for this machine failed the refresh: %v", err)
	}

	if pulled || !strings.Contains(out.String(), "carries no guest image for aarch64") {
		t.Errorf("pulled=%v, said:\n%s\nwant nothing pulled and the reason said", pulled, out.String())
	}

	// AND ANY OTHER FAILURE TO FETCH IT IS STILL ONE.
	fetchImageManifest = func(context.Context, cli.Env, *config.Config) (*imagesource.Manifest, error) {
		return nil, errors.New("imagesource: could not tell whether it carries an image for aarch64")
	}

	if err := refreshFirecrackerImages(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, cfg, "", 3, false); err == nil {
		t.Error("a manifest that could not be fetched was taken for nothing to do")
	}
}

// THE PULL'S CLIENT FETCHES THE IMAGE NAMED FOR THIS HOST'S MACHINE.
func TestThePullsClientFetchesThisHostsMachine(t *testing.T) {
	standOn(t, "aarch64")

	if got := imageClient(imagesource.DefaultSource()).Machine; got != "aarch64" {
		t.Errorf("the pull's client fetches %q, want this host's aarch64", got)
	}
}

// AND THE PULL ITSELF REFUSES AN IMAGE ANOTHER MACHINE'S TIERS NAME, before it has
// staged anything.
func TestAPullRefusesAnImageAnotherMachinesTiersNameBeforeStaging(t *testing.T) {
	standOn(t, "aarch64")

	fixture := newPullFixture(t)

	body, err := os.ReadFile(fixture.cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	head, _, found := strings.Cut(string(body), "tiers:\n")
	if !found {
		t.Fatal("the pull fixture's config has no tiers block; the fixture has drifted")
	}

	if err := os.WriteFile(fixture.cfgPath, []byte(head+"tiers:\n"+archTiers), 0o600); err != nil {
		t.Fatal(err)
	}

	usePullSeams(t, fixture.publisher, durablefile.Installer{}, currentRunner)

	err = fixture.pull(t)
	if err == nil || !strings.Contains(err.Error(), "run on x86_64, and this host is aarch64") {
		t.Fatalf("images pull of the x64 image on aarch64 = %v, want it refused", err)
	}

	assertNothingStaged(t, fixture)

	if fixture.publisher.calls() != 0 || len(fixture.publisher.learned) != 0 {
		t.Errorf("the refused pull reached the cluster: imports %d, asked %v", fixture.publisher.calls(),
			fixture.publisher.learned)
	}
}

// BILLET CHECK SAYS WHEN THE CHANNEL CARRIES NO IMAGE FOR THIS MACHINE, as such.
func TestCheckSaysTheChannelCarriesNoImageForThisMachine(t *testing.T) {
	standOn(t, "aarch64")
	cfg := archConfig(t)

	restore := fetchImageManifest
	t.Cleanup(func() { fetchImageManifest = restore })
	fetchImageManifest = func(context.Context, cli.Env, *config.Config) (*imagesource.Manifest, error) {
		return nil, fmt.Errorf("%w: lists [x86_64]", imagesource.ErrNoImageForMachine)
	}

	var out strings.Builder
	ReportChannelImage(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, cfg)

	if !strings.Contains(out.String(), "the channel's current release carries no guest image for aarch64") {
		t.Errorf("the check said:\n%s\nwant the channel's answer for this machine", out.String())
	}
}

// TIERS NAMING ONE IMAGE WITH TWO ARCHITECTURES ARE REFUSED ON EVERY HOST, pinned to
// different nodes of the site or not, since one image holds one; a tier of
// another site, another pool's, and a tier of another backend naming the same
// word, do not count.
func TestAnImageWhoseTiersNameTwoArchitecturesIsRefusedEverywhere(t *testing.T) {
	cfg := archConfig(t)

	shared := func(label string, arch config.Arch, node, site string, provider config.ProviderKind) config.Tier {
		return config.Tier{Label: label, Provider: provider, Arch: arch, Node: node, Site: site,
			Image: "shared@verified"}
	}

	for _, machine := range []string{"x86_64", "aarch64"} {
		standOn(t, machine)

		cfg.Tiers = []config.Tier{
			shared("a", config.ArchX64, "node-a", "", config.ProviderFirecracker),
			shared("b", config.ArchARM64, "node-b", "", config.ProviderFirecracker),
		}
		if err := refuseImageOfAnotherMachine(cfg, "shared"); err == nil ||
			!strings.Contains(err.Error(), "one image holds one architecture") {
			t.Errorf("on %s, tiers naming shared with two architectures = %v, want refused", machine, err)
		}
	}

	standOn(t, "x86_64")

	cfg.Tiers = []config.Tier{
		shared("here", config.ArchX64, "", "", config.ProviderFirecracker),
		shared("elsewhere", config.ArchARM64, "", "another-site", config.ProviderFirecracker),
		shared("docker", config.ArchARM64, "", "", config.ProviderDocker),
	}
	if err := refuseImageOfAnotherMachine(cfg, "shared"); err != nil {
		t.Errorf("another site's tier, or another backend's, was counted: %v", err)
	}

	cfg.Tiers = []config.Tier{shared("docker", config.ArchARM64, "", "", config.ProviderDocker)}
	if err := refuseImageOfAnotherMachine(cfg, "shared"); err != nil {
		t.Errorf("a docker tier's image refused a firecracker pull: %v", err)
	}
}

// A SIDELOADED DIRECTORY'S SIGNATURE IS THE ONE BESIDE THE MANIFEST READ: the
// machine's own, or manifest.json's when that is what was read.
func TestASideloadsSignatureIsTheOneBesideTheManifestRead(t *testing.T) {
	standOn(t, "aarch64")

	for _, tc := range []struct {
		name, manifest, bundle string
	}{
		{"named", "manifest-aarch64.json", "manifest-aarch64.sigstore.json"},
		{"fallback", imagesource.ManifestName, imagesource.BundleName},
	} {
		fixture := newPullFixture(t)
		if tc.manifest != imagesource.ManifestName {
			if err := os.Rename(filepath.Join(fixture.from, imagesource.ManifestName),
				filepath.Join(fixture.from, tc.manifest)); err != nil {
				t.Fatal(err)
			}
		}

		cfg, err := config.Load(fixture.cfgPath)
		if err != nil {
			t.Fatal(err)
		}

		_, _, _, err = stageImage(t.Context(), processEnv(), cfg, stageOptions{
			from: fixture.from, staging: fixture.staging,
		})

		// THE FILE IT OPENED, not the name its message gives.
		var opened *fs.PathError
		if !errors.As(err, &opened) || opened.Path != filepath.Join(fixture.from, tc.bundle) {
			t.Errorf("%s: stageImage = %v, want it to have opened %s", tc.name, err, tc.bundle)
		}
	}
}

// THE PULL'S OWN MANIFEST FETCH ASKS FOR THIS MACHINE'S MANIFEST.
func TestThePullsManifestFetchAsksForThisMachinesManifest(t *testing.T) {
	standOn(t, "aarch64")
	cfg := archConfig(t)

	_, _, err := resolveImageManifest(t.Context(), cli.Env{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}},
		cfg, stageOptions{source: "https://127.0.0.1:1/images", skipSig: true})
	if err == nil || !strings.Contains(err.Error(), "/images/manifest-aarch64.json") {
		t.Fatalf("resolveImageManifest = %v, want the fetch of this machine's manifest", err)
	}
}

// A NODE WHOSE TIERS ARE ALL ANOTHER MACHINE'S HAS NOTHING TO PREPARE for a host
// upgrade, and says so; one with no tiers at all is still asked to name an image.
func TestImagesCompatibleOnANodeWhoseTiersAreAllAnotherMachinesPreparesNothing(t *testing.T) {
	standOn(t, "x86_64")

	path := firecrackerRefreshConfig(t, "")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	head, _, _ := strings.Cut(string(body), "tiers:\n")
	armOnly := "  - label: on-arm64\n    provider: firecracker\n    vcpu: 2\n    memory: 4GiB\n" +
		"    arch: arm64\n    image: ubuntu-2404-arm64@verified\n"

	if err := os.WriteFile(path, []byte(head+"tiers:\n"+armOnly), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := cmdImagesCompatible(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, []string{"--config", path}); err != nil {
		t.Fatalf("images compatible on a node whose tiers are all arm64 = %v, want nothing to prepare", err)
	}

	if !strings.Contains(out.String(), "nothing to prepare") {
		t.Errorf("it said:\n%s\nwant nothing to prepare", out.String())
	}
}

// AND ONE WITH NO FIRECRACKER TIER AT ALL IS STILL ASKED TO NAME AN IMAGE: nothing
// says this node needs none.
func TestImagesCompatibleOnANodeWithNoTiersStillAsksForAnImage(t *testing.T) {
	standOn(t, "x86_64")

	path := firecrackerRefreshConfig(t, "")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	head, _, _ := strings.Cut(string(body), "tiers:\n")
	docker := "  - label: dockerish\n    provider: docker\n    vcpu: 2\n    memory: 4GiB\n    image: busybox\n"

	if err := os.WriteFile(path, []byte(head+"tiers:\n"+docker), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := cmdImagesCompatible(t.Context(), cli.Env{Stdout: &out, Stderr: &out},
		[]string{"--config", path}); err == nil || !strings.Contains(err.Error(), "no firecracker tier names one") {
		t.Fatalf("images compatible on a node with no firecracker tier = %v, want it asked for an image", err)
	}
}

// AND A PULL WITH NO ARGUMENT ON SUCH A NODE SAYS WHY IT HAS NOTHING TO PULL.
func TestAPullWithNoArgumentOnANodeOfOnlyAnotherMachinesTiersSaysWhy(t *testing.T) {
	standOn(t, "x86_64")

	fixture := newPullFixture(t)

	body, err := os.ReadFile(fixture.cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	head, _, _ := strings.Cut(string(body), "tiers:\n")
	armOnly := "  - label: on-arm64\n    provider: firecracker\n    vcpu: 2\n    memory: 4GiB\n" +
		"    arch: arm64\n    image: ubuntu-2404-arm64@verified\n"

	if err := os.WriteFile(fixture.cfgPath, []byte(head+"tiers:\n"+armOnly), 0o600); err != nil {
		t.Fatal(err)
	}

	err = cmdImagesPull(t.Context(), processEnv(), []string{"--config", fixture.cfgPath, "--from", fixture.from,
		"--staging-dir", fixture.staging, "--kernel-dir", fixture.kernelDir, "--skip-signature-verification"})
	if err == nil || !strings.Contains(err.Error(), "every firecracker tier on this node is another machine's") {
		t.Fatalf("a pull with no argument = %v, want why it has nothing to pull", err)
	}
}
