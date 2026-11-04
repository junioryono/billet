package images

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/provider/tart"
	"github.com/junioryono/billet/internal/runnerrelease"
)

// fakeVerifier answers for the provider, recording what each command asked.
type fakeVerifier struct {
	due       map[string]tart.VerifiedDue
	dueErr    error
	verifyErr map[string]error
	verified  []tart.VerifyOptions
	askedDue  []string
	refreshed int
	unknown   []string
	unpromote []string
}

func (f *fakeVerifier) Verify(_ context.Context, opts tart.VerifyOptions) (*tart.VerifiedRecord, error) {
	f.verified = append(f.verified, opts)
	if err := f.verifyErr[opts.Image]; err != nil {
		return nil, err
	}

	return &tart.VerifiedRecord{Image: opts.Image, Base: "base", Runner: "2.337.0"}, nil
}

func (f *fakeVerifier) Unpromote(_ context.Context, image, base string) ([]string, error) {
	f.unpromote = append(f.unpromote, image+" "+base)

	return []string{base}, nil
}

func (f *fakeVerifier) ImageDue(_ context.Context, image string) (tart.VerifiedDue, error) {
	f.askedDue = append(f.askedDue, image)

	return f.due[image], f.dueErr
}

func (f *fakeVerifier) RefreshDeadlines(context.Context, tart.RunnerWindow) ([]string, error) {
	f.refreshed++

	return f.unknown, nil
}

func stageVerifier(t *testing.T, f *fakeVerifier) {
	t.Helper()

	prev := OpenTartVerifier
	OpenTartVerifier = func(*config.Config) (tartVerifier, error) { return f, nil }

	t.Cleanup(func() { OpenTartVerifier = prev })
}

func tartConfig(tiers ...config.Tier) *config.Config {
	return &config.Config{
		Node:  &config.NodeConfig{Name: "mac-1", Provider: config.ProviderTart},
		Tiers: tiers,
	}
}

func tartTier(label, image string, trust config.WorkloadTrust) config.Tier {
	return config.Tier{Label: label, Provider: config.ProviderTart, Image: image, Trust: trust}
}

func capturedEnv() (cli.Env, *bytes.Buffer) {
	var out bytes.Buffer

	return cli.Env{Stdout: &out, Stderr: &out}, &out
}

// A VERIFICATION IS HANDED WHAT THE TIER BOOTS: its command says where the runner is, and
// a tier taking untrusted work boots its guests under isolation, so its probe does too.
func TestAVerificationTakesItsCommandAndTrustFromTheTier(t *testing.T) {
	const image = "ghcr.io/a/b:1@verified"

	custom := tartTier("t", image, config.WorkloadTrusted)
	custom.Command = []string{"./custom/run.sh"}

	untrusted := tartTier("u", image, config.WorkloadUntrusted)
	untrusted.Command = custom.Command

	for _, tc := range []struct {
		name      string
		tiers     []config.Tier
		command   string
		untrusted bool
	}{
		{name: "the tier's own command", tiers: []config.Tier{custom}, command: "./custom/run.sh"},
		{name: "another tier naming it untrusted", tiers: []config.Tier{custom, untrusted},
			command: "./custom/run.sh", untrusted: true},
		{name: "another spelling of one image", tiers: []config.Tier{tartTier("v", "ghcr.io/a/b:1", config.WorkloadUntrusted)},
			command: "./actions-runner/run.sh", untrusted: true},
		{name: "no tier names it", tiers: []config.Tier{tartTier("x", "ghcr.io/other/c:1", config.WorkloadUntrusted)},
			command: "./actions-runner/run.sh"},
		{name: "another node's tier says nothing about this one", tiers: []config.Tier{func() config.Tier {
			foreign := tartTier("f", image, config.WorkloadUntrusted)
			foreign.Node = "mac-2"
			foreign.Command = []string{"./foreign/run.sh"}

			return foreign
		}()}, command: "./actions-runner/run.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := tartVerifyOptions(tartConfig(tc.tiers...), image)
			if err != nil {
				t.Fatal(err)
			}

			if !slices.Equal(opts.Command, []string{tc.command}) || opts.GuestUntrusted != tc.untrusted {
				t.Fatalf("options %+v, want command %q, untrusted %v", opts, tc.command, tc.untrusted)
			}

			if opts.Window == nil {
				t.Fatal("nothing asks GitHub about the runner's window")
			}
		})
	}
}

// A PROBE ASKS WHERE ONE RUNNER IS, so two commands for one image cannot both be proved.
func TestTiersNamingOneImageWithDifferentCommandsRefuse(t *testing.T) {
	const image = "ghcr.io/a/b:1@verified"

	one := tartTier("one", image, config.WorkloadTrusted)
	one.Command = []string{"./one/run.sh"}

	two := tartTier("two", image, config.WorkloadTrusted)
	two.Command = []string{"./two/run.sh"}

	_, err := tartVerifyOptions(tartConfig(one, two), image)
	if err == nil || !strings.Contains(err.Error(), "different commands") {
		t.Fatalf("answered %v", err)
	}
}

// AN UNKNOWN IMAGE SET IS NOT AN EMPTY ONE: a refresh that cannot tell which images are this
// node's says so rather than answer nothing to do.
func TestARefreshThatCannotTellItsImagesFails(t *testing.T) {
	stageVerifier(t, &fakeVerifier{})

	cfg := tartConfig(tartTier("a", "ghcr.io/a/b:1@verified", config.WorkloadTrusted))
	cfg.Node.TLS = &config.NodeTLS{
		CAPath: "/nonexistent/ca", CertPath: "/nonexistent/cert", KeyPath: "/nonexistent/key",
	}

	env, _ := capturedEnv()

	if err := verifyDueTartImages(t.Context(), env, cfg, false); err == nil {
		t.Fatal("verifyDueTartImages answered nothing to do")
	}

	if err := refreshTartVerified(t.Context(), env, cfg); err == nil {
		t.Fatal("refreshTartVerified answered nothing to do")
	}
}

// --no-yield HOLDS A SLOT AGAINST EVERY JOB, so a node that may be running refuses it, and
// a node nobody can vouch for is one that may be.
func TestNoYieldRefusesUnlessTheNodeIsProvedStopped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active lifeops.Tristate
		err    error
		want   string
	}{
		{name: "running", active: lifeops.Yes, want: "local down --node"},
		{name: "could not tell", active: lifeops.Unknown, want: "local down --node"},
		{name: "the question failed", active: lifeops.Unknown, err: errors.New("launchctl"), want: "could not tell"},
		{name: "stopped", active: lifeops.No},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeVerifier{}
			stageVerifier(t, f)

			prev := tartNodeRunning
			tartNodeRunning = func(context.Context) (lifeops.Tristate, error) { return tc.active, tc.err }

			t.Cleanup(func() { tartNodeRunning = prev })

			env, _ := capturedEnv()
			err := cmdImagesVerifyTart(t.Context(), env, tartConfig(), "ghcr.io/a/b:1", "", true)

			if tc.want == "" {
				if err != nil || len(f.verified) != 1 || !f.verified[0].NoYield {
					t.Fatalf("Verify = %v, options %+v", err, f.verified)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("answered %v, want %q", err, tc.want)
			}

			if len(f.verified) != 0 {
				t.Fatal("verified anyway")
			}
		})
	}
}

// THE REFRESH VERIFIES WHAT MOVED, for `@verified` references alone. One that fails stops
// none of the others, and a yield is not a failure.
func TestTheRefreshVerifiesOnlyWhatIsDue(t *testing.T) {
	const (
		moved   = "ghcr.io/a/moved:1@verified"
		same    = "ghcr.io/a/same:1@verified"
		broken  = "ghcr.io/a/broken:1@verified"
		yields  = "ghcr.io/a/yields:1@verified"
		plain   = "ghcr.io/a/plain:1"
		movedOK = "ghcr.io/a/after:1@verified"
	)

	f := &fakeVerifier{
		due: map[string]tart.VerifiedDue{
			moved: {Verify: true}, same: {Why: "already verified"}, broken: {Verify: true},
			yields: {Verify: true}, movedOK: {Verify: true},
		},
		verifyErr: map[string]error{broken: errors.New("no boot"), yields: tart.ErrYielded},
	}
	stageVerifier(t, f)

	cfg := tartConfig(tartTier("a", moved, config.WorkloadTrusted), tartTier("b", same, config.WorkloadTrusted),
		tartTier("c", broken, config.WorkloadTrusted), tartTier("d", yields, config.WorkloadTrusted),
		tartTier("e", plain, config.WorkloadTrusted), tartTier("f", movedOK, config.WorkloadTrusted))

	env, _ := capturedEnv()
	err := verifyDueTartImages(t.Context(), env, cfg, false)

	if err == nil || !strings.Contains(err.Error(), "no boot") || strings.Contains(err.Error(), "yielded") {
		t.Fatalf("refresh = %v, want the failure alone reported", err)
	}

	var verified []string
	for _, opts := range f.verified {
		verified = append(verified, opts.Image)
	}

	if want := []string{moved, broken, yields, movedOK}; !slices.Equal(verified, want) {
		t.Fatalf("verified %v, want %v", verified, want)
	}

	if slices.Contains(f.askedDue, plain) {
		t.Fatal("a reference no tier names `@verified` was asked about")
	}
}

func TestADryRunVerifiesNothing(t *testing.T) {
	const image = "ghcr.io/a/moved:1@verified"

	f := &fakeVerifier{due: map[string]tart.VerifiedDue{image: {Verify: true}}}
	stageVerifier(t, f)

	env, _ := capturedEnv()
	if err := verifyDueTartImages(t.Context(), env, tartConfig(tartTier("a", image, config.WorkloadTrusted)), true); err != nil {
		t.Fatal(err)
	}

	if len(f.verified) != 0 {
		t.Fatal("a dry run verified an image")
	}
}

// A REGISTRY THAT CANNOT ANSWER IS AN ERROR THE REFRESH REPORTS, never nothing to do.
func TestARegistryThatCannotAnswerIsReported(t *testing.T) {
	const image = "ghcr.io/a/moved:1@verified"

	f := &fakeVerifier{dueErr: errors.New("registry down")}
	stageVerifier(t, f)

	env, _ := capturedEnv()

	err := verifyDueTartImages(t.Context(), env, tartConfig(tartTier("a", image, config.WorkloadTrusted)), false)
	if err == nil || !strings.Contains(err.Error(), "registry down") {
		t.Fatalf("refresh = %v", err)
	}
}

func TestUnpromoteSplitsItsBaseAtTheLastAt(t *testing.T) {
	f := &fakeVerifier{}
	stageVerifier(t, f)

	env, out := capturedEnv()

	if err := cmdImagesUnpromoteTart(t.Context(), env, tartConfig(), "ghcr.io/a/b:1@verified@billetbase-x-1"); err != nil {
		t.Fatal(err)
	}

	if want := "ghcr.io/a/b:1@verified billetbase-x-1"; !slices.Equal(f.unpromote, []string{want}) {
		t.Fatalf("asked %v, want %q", f.unpromote, want)
	}

	if !strings.Contains(out.String(), "removed  billetbase-x-1") {
		t.Fatalf("said %s", out.String())
	}

	if err := cmdImagesUnpromoteTart(t.Context(), env, tartConfig(), "no-base"); err == nil {
		t.Fatal("unpromoted a spec naming no base")
	}
}

// A VERIFIED IMAGE IS NEVER PULLED INTO THE SHARED STORE, where a pull could replace a
// digest another deployment pins.
func TestAPullSkipsAVerifiedImage(t *testing.T) {
	const image = "ghcr.io/a/b:1@verified"

	env, out := capturedEnv()

	err := PullTartImages(t.Context(), env, tartConfig(tartTier("a", image, config.WorkloadTrusted)), "")
	if err != nil {
		t.Fatalf("PullTartImages: %v", err)
	}

	if !strings.Contains(out.String(), "billet images verify") {
		t.Fatalf("said %s", out.String())
	}
}

// THE RUNNER'S WINDOW IN THE PROVIDER'S WORDS. Expiry is a proof whatever was read, and
// anything short of it on a partial history is could-not-tell, never a date.
func TestTheRunnerWindowAnswersThreeWays(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	published := now.Add(-10 * 24 * time.Hour)
	whole := runnerrelease.Freshness{Installed: "2.300.0", InstalledKnown: true, HistoryComplete: true}

	behind := whole
	behind.Latest, behind.FirstNewer, behind.FirstNewerPublished = "2.301.0", "2.301.0", published

	expired := behind
	expired.FirstNewerPublished = now.Add(-runnerrelease.Grace)

	partial := behind
	partial.HistoryComplete = false

	expiredPartial := expired
	expiredPartial.InstalledKnown, expiredPartial.HistoryComplete = false, false

	noWindow := whole
	noWindow.Latest = "2.301.0"

	current := whole

	for _, tc := range []struct {
		name     string
		fresh    runnerrelease.Freshness
		err      error
		wantErr  bool
		expired  bool
		deadline time.Time
	}{
		{name: "current", fresh: current},
		{name: "behind", fresh: behind, deadline: published.Add(runnerrelease.Grace)},
		{name: "expired", fresh: expired, expired: true, deadline: now},
		{name: "expired, on a partial history", fresh: expiredPartial, expired: true, deadline: now},
		{name: "a partial history", fresh: partial, wantErr: true},
		{name: "behind with no window", fresh: noWindow, wantErr: true},
		{name: "GitHub did not answer", err: errors.New("down"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := resolveRunnerFreshness
			resolveRunnerFreshness = func(context.Context, *http.Client, string) (runnerrelease.Freshness, error) {
				return tc.fresh, tc.err
			}

			t.Cleanup(func() { resolveRunnerFreshness = prev })

			answer, err := TartRunnerWindow(func() time.Time { return now })(t.Context(), "2.300.0")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}

			if answer.Expired != tc.expired || !answer.Deadline.Equal(tc.deadline) {
				t.Fatalf("answer %+v, want expired %v at %v", answer, tc.expired, tc.deadline)
			}
		})
	}
}
