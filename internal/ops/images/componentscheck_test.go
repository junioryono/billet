package images

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/store/ceph"
)

// fakeComponentsChecker resolves @verified to one generation, returns an exact one
// unchanged, and answers each generation's components record.
//
// The generations that exist are the one @verified names and every one with a
// record, unless generations says otherwise.
type fakeComponentsChecker struct {
	verified       string
	resolveErr     error
	records        map[string]ceph.GenerationComponents
	generations    []string
	generationsErr error
	// contracts is each generation's guest contract; one not named speaks this
	// billet's.
	contracts   map[string]string
	contractErr error
}

func (f fakeComponentsChecker) GuestContract(_ context.Context, _, generation string) (string, bool, error) {
	if f.contractErr != nil {
		return "", false, f.contractErr
	}
	if contract, named := f.contracts[generation]; named {
		return contract, contract != "", nil
	}

	return firecracker.GuestContract, true, nil
}

func (f fakeComponentsChecker) Generations(context.Context, string) ([]ceph.Generation, error) {
	if f.generationsErr != nil {
		return nil, f.generationsErr
	}

	names := f.generations
	if names == nil {
		names = append(names, f.verified)
		for name := range f.records {
			names = append(names, name)
		}
	}

	var out []ceph.Generation
	for _, name := range names {
		if g, ok := ceph.ParseGeneration(name); ok {
			out = append(out, g)
		}
	}

	return out, nil
}

func (f fakeComponentsChecker) ResolveGeneration(_ context.Context, image, _ string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}

	name, generation, _ := strings.Cut(image, "@")
	if generation == ceph.Verified {
		return name + "@" + f.verified, nil
	}

	return image, nil
}

func (f fakeComponentsChecker) Components(_ context.Context, _, generation string,
) (ceph.GenerationComponents, bool, error) {
	record, found := f.records[generation]

	return record, found, nil
}

// checkComponents runs ReportImageComponents against store under the config
// firecrackerNodeConfig is, with extra spliced in before its node block and tiers
// appended, and returns what it printed.
func checkComponents(t *testing.T, store fakeComponentsChecker, extra, tiers string) string {
	t.Helper()

	restore := openComponentsChecker
	t.Cleanup(func() { openComponentsChecker = restore })
	openComponentsChecker = func(*config.Config) (componentsChecker, error) { return store, nil }

	cfg, err := config.Load(componentsCheckConfig(t, extra, tiers))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var out bytes.Buffer
	ReportImageComponents(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, cfg)

	return out.String()
}

func componentsCheckConfig(t *testing.T, extra, tiers string) string {
	t.Helper()

	path := firecrackerRefreshConfig(t, extra)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(path, append(body, []byte(tiers)...), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

const completedGeneration = "g20260902120000"

// THE GENERATION A TIER BOOTS IS DESCRIBED AGAINST WHAT THIS DEPLOYMENT ACCEPTS:
// what it was completed with, what its image left out unaccepted, and what it
// lacks that is accepted, with what will complete it.
func TestCheckDescribesTheGenerationATierBootsAgainstWhatIsAccepted(t *testing.T) {
	store := fakeComponentsChecker{verified: completedGeneration, records: map[string]ceph.GenerationComponents{
		completedGeneration: {
			LeftOut:   []string{"android-sdk", "codeql", "google-chrome"},
			Installed: map[string]string{"google-chrome": "155.0.8059.84"},
		},
	}}

	out := checkComponents(t, store, "images:\n  accept: [google-chrome, android-sdk]\n", "")

	for _, want := range []string{
		"ubuntu-2404-x64@verified (tier small) boots ubuntu-2404-x64@" + completedGeneration +
			", completed with google-chrome 155.0.8059.84",
		"whose image leaves out codeql: this deployment has not accepted them in images.accept",
		"which lacks android-sdk, which this deployment accepts; the refresh timer's next " +
			"`billet images refresh` completes a new generation of it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the check did not say %q; it said:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no longer accepts") {
		t.Errorf("the check reported an accepted component as withdrawn:\n%s", out)
	}
}

// WHAT COMPLETES A GAP DEPENDS ON WHO PULLS: the refresh timer, an operator when
// the deployment does not update itself, or a pin that has to move.
func TestCheckSaysWhatWillCompleteAGeneration(t *testing.T) {
	store := fakeComponentsChecker{verified: completedGeneration, records: map[string]ceph.GenerationComponents{
		completedGeneration: {LeftOut: []string{"google-chrome"}, Installed: map[string]string{}},
	}}

	out := checkComponents(t, store, "release:\n  automatic: false\nimages:\n  accept: [google-chrome]\n", "")
	if !strings.Contains(out, "lacks google-chrome, which this deployment accepts; release.automatic "+
		"is false, so `billet images pull --verify ubuntu-2404-x64` is what completes") {
		t.Errorf("a deployment that does not update itself was not told to pull:\n%s", out)
	}

	pinned := `  - label: pinned
    provider: firecracker
    vcpu: 2
    memory: 4GiB
    image: ubuntu-2404-x64@` + completedGeneration + "\n"
	out = checkComponents(t, store, "images:\n  accept: [google-chrome]\n", pinned)
	if !strings.Contains(out, "ubuntu-2404-x64@"+completedGeneration+" (tier pinned) boots") ||
		!strings.Contains(out, "it is pinned, so a generation completed with what this deployment "+
			"accepts reaches it only when the pin moves") {
		t.Errorf("a pinned tier was not told its pin has to move:\n%s", out)
	}
}

// A COMPONENT INSTALLED AND NO LONGER ACCEPTED IS NAMED, and nothing that was
// installed is reported as left out.
func TestCheckNamesAComponentNoLongerAccepted(t *testing.T) {
	store := fakeComponentsChecker{verified: completedGeneration, records: map[string]ceph.GenerationComponents{
		completedGeneration: {
			LeftOut:   []string{"google-chrome"},
			Installed: map[string]string{"google-chrome": "155.0.8059.84"},
		},
	}}

	out := checkComponents(t, store, "", "")
	if !strings.Contains(out, "which carries google-chrome, which this deployment no longer accepts") {
		t.Errorf("a withdrawn acceptance was not reported:\n%s", out)
	}
	if strings.Contains(out, "leaves out google-chrome") {
		t.Errorf("an installed component was reported as left out:\n%s", out)
	}
}

// TIERS THAT SHARE AN IMAGE ARE NAMED TOGETHER, once.
func TestCheckNamesEveryTierThatBootsAnImage(t *testing.T) {
	store := fakeComponentsChecker{verified: completedGeneration, records: map[string]ceph.GenerationComponents{
		completedGeneration: {LeftOut: []string{"codeql"}, Installed: map[string]string{}},
	}}

	large := `  - label: large
    provider: firecracker
    vcpu: 4
    memory: 8GiB
    image: ubuntu-2404-x64@verified
`
	out := checkComponents(t, store, "", large)
	if strings.Count(out, "leaves out codeql") != 1 || !strings.Contains(out, "(tiers small, large)") {
		t.Errorf("two tiers on one image were not reported once, together:\n%s", out)
	}
}

// A GENERATION WHOSE IMAGE LEFT NOTHING OUT SAYS NOTHING, and one that cannot be
// resolved says it could not tell.
func TestCheckIsQuietForAnImageThatLeftNothingOutAndSaysWhenItCannotTell(t *testing.T) {
	if out := checkComponents(t, fakeComponentsChecker{verified: completedGeneration}, "", ""); out != "" {
		t.Errorf("an image that left nothing out was reported:\n%s", out)
	}

	out := checkComponents(t, fakeComponentsChecker{resolveErr: errors.New("rbd timed out")}, "", "")
	if !strings.Contains(out, "could not tell which generation it boots: rbd timed out") {
		t.Errorf("an unresolvable tier image was not reported as unknown:\n%s", out)
	}
}

// A PIN TO A GENERATION THAT DOES NOT EXIST IS SAID SO, never read as an image that
// left nothing out, and a listing that fails is could-not-tell.
func TestCheckSaysWhenATierNamesAGenerationThatIsNotThere(t *testing.T) {
	pinned := `  - label: pinned
    provider: firecracker
    vcpu: 2
    memory: 4GiB
    image: ubuntu-2404-x64@g20260101000000
`
	store := fakeComponentsChecker{verified: completedGeneration, generations: []string{completedGeneration}}

	out := checkComponents(t, store, "", pinned)
	if !strings.Contains(out, "ubuntu-2404-x64@g20260101000000 (tier pinned) names "+
		"ubuntu-2404-x64@g20260101000000, which is not a generation of ubuntu-2404-x64, so a launch of it fails") {
		t.Errorf("a pin to a missing generation was not reported:\n%s", out)
	}

	store.generationsErr = errors.New("rbd timed out")
	out = checkComponents(t, store, "", "")
	if !strings.Contains(out, "could not tell which generations of ubuntu-2404-x64 exist: rbd timed out") {
		t.Errorf("a failed listing was not reported as unknown:\n%s", out)
	}
}

// A NEWER GENERATION THAT ALREADY MATCHES IS A VERIFICATION OUTSTANDING, NOT A PULL:
// the refresh compares against the newest generation and sees nothing to do.
func TestCheckSendsAnUnverifiedMatchingGenerationToVerification(t *testing.T) {
	const newer = "g20260903120000"
	store := fakeComponentsChecker{verified: completedGeneration, records: map[string]ceph.GenerationComponents{
		completedGeneration: {LeftOut: []string{"google-chrome"}, Installed: map[string]string{}},
		newer:               {LeftOut: []string{"google-chrome"}, Installed: map[string]string{"google-chrome": "155.0.8059.84"}},
	}}

	out := checkComponents(t, store, "images:\n  accept: [google-chrome]\n", "")
	if !strings.Contains(out, "lacks google-chrome, which this deployment accepts; the newer generation "+
		newer+" matches what this deployment accepts and @verified has not moved onto it") ||
		!strings.Contains(out, "`billet images verify ubuntu-2404-x64@"+newer+"` proves it") {
		t.Errorf("a matching unverified generation was not sent to verification:\n%s", out)
	}

	// A NEWER GENERATION UNDER ANOTHER GUEST CONTRACT is not this billet's to verify:
	// during a rolling upgrade it is a newer binary's.
	store.contracts = map[string]string{newer: "999"}
	out = checkComponents(t, store, "images:\n  accept: [google-chrome]\n", "")
	if !strings.Contains(out, "the newer generation "+newer+" matches what this deployment accepts and "+
		"speaks guest contract 999, which this billet does not") || strings.Contains(out, "billet images verify") {
		t.Errorf("a newer generation under another contract was sent to this billet's verification:\n%s", out)
	}

	// NO RECORDED CONTRACT, OR NONE THAT COULD BE READ, IS NOT A VERIFICATION TO RUN.
	store.contracts = map[string]string{newer: ""}
	out = checkComponents(t, store, "images:\n  accept: [google-chrome]\n", "")
	if !strings.Contains(out, "records no guest contract, so no billet resolves @verified to it") ||
		strings.Contains(out, "billet images verify") {
		t.Errorf("a newer generation with no contract was sent to verification:\n%s", out)
	}
	store.contracts = nil

	store.contractErr = errors.New("rbd timed out")
	out = checkComponents(t, store, "images:\n  accept: [google-chrome]\n", "")
	if !strings.Contains(out, "could not tell which guest contract the newer generation "+newer) ||
		!strings.Contains(out, "rbd timed out") || strings.Contains(out, "billet images verify") {
		t.Errorf("an unreadable contract was not reported as unknown:\n%s", out)
	}
	store.contractErr = nil

	// A NEWER GENERATION THAT ALSO LACKS IT leaves the refresh to complete one.
	store.records[newer] = ceph.GenerationComponents{LeftOut: []string{"google-chrome"}, Installed: map[string]string{}}
	out = checkComponents(t, store, "images:\n  accept: [google-chrome]\n", "")
	if !strings.Contains(out, "the refresh timer's next `billet images refresh` completes a new generation") {
		t.Errorf("a newer generation lacking the component was treated as complete:\n%s", out)
	}
}

// checkChannel runs ReportChannelImage against a fetch that answers manifest or
// err, and returns what it printed.
func checkChannel(t *testing.T, extra string, manifest *imagesource.Manifest, fetchErr error) string {
	t.Helper()

	restore := fetchImageManifest
	t.Cleanup(func() { fetchImageManifest = restore })
	fetchImageManifest = func(_ context.Context, env cli.Env, _ *config.Config) (*imagesource.Manifest, error) {
		env.Stdout.Write([]byte("fetching the channel\n")) //nolint:errcheck // a buffer or io.Discard

		return manifest, fetchErr
	}

	cfg, err := config.Load(firecrackerRefreshConfig(t, extra))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var out bytes.Buffer
	ReportChannelImage(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, cfg)

	return out.String()
}

// AN IMAGE THAT NEEDS A NEWER BILLET IS SAID SO, and a deployment that does not
// update itself is told that nothing changes until somebody upgrades billet.
func TestCheckSaysWhetherThisBilletCanTakeUpTheChannelsImage(t *testing.T) {
	usable := refreshManifest(channelBuilt)
	out := checkChannel(t, "", usable, nil)
	if !strings.Contains(out, "the channel's image, built 2026-09-01 04:17 UTC, is one this billet can import") {
		t.Errorf("a usable image was not reported as usable:\n%s", out)
	}
	if strings.Contains(out, "fetching") {
		t.Errorf("the fetch's narration reached the check:\n%s", out)
	}

	needsNewer := refreshManifest(channelBuilt)
	leavingOut(needsNewer)
	underAnotherContract(needsNewer)

	out = checkChannel(t, "", needsNewer, nil)
	if !strings.Contains(out, "needs a newer billet, which this deployment installs by itself") {
		t.Errorf("an image needing a newer billet was not reported:\n%s", out)
	}

	out = checkChannel(t, "release:\n  automatic: false\n", needsNewer, nil)
	if !strings.Contains(out, "needs a newer billet, and release.automatic is false, so no newer "+
		"guest image is taken up until an operator upgrades billet") {
		t.Errorf("a deployment that does not update itself was not told:\n%s", out)
	}

	// A PINNED RELEASE FOLLOWS NO CHANNEL, so nothing installs a newer billet by itself.
	out = checkChannel(t, "release:\n  version: v0.13.0\n", needsNewer, nil)
	if !strings.Contains(out, "release.version pins this deployment at v0.13.0, so no newer guest "+
		"image is taken up until the pin moves") || strings.Contains(out, "installs by itself") {
		t.Errorf("a pinned release was promised an automatic upgrade:\n%s", out)
	}

	out = checkChannel(t, "", nil, errors.New("dial tcp: i/o timeout"))
	if !strings.Contains(out, "could not tell whether this billet can import the channel's image: "+
		"dial tcp: i/o timeout") || strings.Contains(out, "newer billet") {
		t.Errorf("a failed fetch was not reported as unknown:\n%s", out)
	}
}
