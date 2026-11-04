package host

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider/tart"
	"github.com/junioryono/billet/internal/state"
)

// A VERIFIED TART IMAGE IS WRITTEN ONLY ONCE EVERY HOST READS IT (ADR-021): a Mac's own
// image commands hand the reference to tart as a name.
func TestAVerifiedTartImageIsRefusedWhileARegisteredHostPredatesIt(t *testing.T) {
	t.Parallel()

	tier := func(image string, provider config.ProviderKind) []config.Tier {
		return []config.Tier{{Label: "a", Provider: provider, Image: image}}
	}

	named := &config.Config{Tiers: tier("ghcr.io/a/b:1@verified", config.ProviderTart)}
	unnamed := &config.Config{Tiers: tier("ghcr.io/a/b:1", config.ProviderTart)}
	firecracker := &config.Config{Tiers: tier("ubuntu-2404-x64@verified", config.ProviderFirecracker)}

	current := alloc.NodeWire{Name: "new-1", Max: nodeapi.VersionTartVerified}
	older := alloc.NodeWire{Name: "old-1", Release: "v0.29.0", Max: nodeapi.VersionTartVerified - 1}
	unsaid := alloc.NodeWire{Name: "unsaid-1"}
	gone := alloc.NodeWire{Name: "gone-1", Max: 1, Decommissioned: "2026-10-26T00:00:00Z"}

	for _, tc := range []struct {
		name  string
		cfg   *config.Config
		hosts []alloc.NodeWire
		want  string
	}{
		{name: "every host current", cfg: named, hosts: []alloc.NodeWire{current}},
		{name: "an older host", cfg: named, hosts: []alloc.NodeWire{current, older}, want: "old-1 (v0.29.0)"},
		{name: "a host that cannot say", cfg: named, hosts: []alloc.NodeWire{unsaid}, want: "unsaid-1"},
		{name: "an older host decommissioned", cfg: named, hosts: []alloc.NodeWire{current, gone}},
		{name: "no tier names it", cfg: unnamed, hosts: []alloc.NodeWire{older}},
		{name: "another backend's spelling", cfg: firecracker, hosts: []alloc.NodeWire{older}},
	} {
		err := tartVerifiedBeforeEveryHost(tc.cfg, tc.hosts)

		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: answered %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestACheckNamesEveryTartTierNobodyVerified(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	reportUnverifiedTartTiers(cli.Env{Stdout: &out}, &config.Config{Tiers: []config.Tier{
		{Label: "plain", Provider: config.ProviderTart, Image: "ghcr.io/a/b:1"},
		{Label: "checked", Provider: config.ProviderTart, Image: "ghcr.io/a/c:1@verified"},
		{Label: "elsewhere", Provider: config.ProviderFirecracker, Image: "ubuntu-2404-x64@g1"},
	}})

	if !strings.Contains(out.String(), "tier plain boots ghcr.io/a/b:1, which nobody verified") {
		t.Errorf("said %s", out.String())
	}

	for _, label := range []string{"checked", "elsewhere"} {
		if strings.Contains(out.String(), label) {
			t.Errorf("named %s: %s", label, out.String())
		}
	}

	out.Reset()

	// VALIDATION REFUSES A DIGEST WITH @verified, so that is not what to advise.
	reportUnverifiedTartTiers(cli.Env{Stdout: &out}, &config.Config{Tiers: []config.Tier{
		{Label: "pinned", Provider: config.ProviderTart, Image: "ghcr.io/a/b@sha256:0"},
	}})

	if !strings.Contains(out.String(), "pinned") || strings.Contains(out.String(), "sha256:0@verified") {
		t.Errorf("advised what validation refuses: %s", out.String())
	}
}

// A RECORD DOES NOT ANSWER FOR A PLAIN NAME: a base is not in the cache that name clones from, so
// a tier naming it plainly still fails, and the check says so.
func TestAPlainNameIsCheckedAsAPlainName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		pulled, recorded bool
		want, not        string
	}{
		{name: "neither", want: "every job on its tier will fail"},
		{name: "pulled", pulled: true, want: "nobody verified"},
		{name: "recorded and not pulled", recorded: true, want: "NOT pulled, so a job on a tier naming it plainly", not: "nobody verified"},
		{name: "both", pulled: true, recorded: true, want: "pulled", not: "nobody verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			got := reportPlainTartImage(cli.Env{Stdout: &out}, "ghcr.io/a/b:1", tc.pulled, tc.recorded)
			if got != tc.pulled {
				t.Errorf("answered %v", got)
			}

			if !strings.Contains(out.String(), tc.want) || (tc.not != "" && strings.Contains(out.String(), tc.not)) {
				t.Errorf("said %q, want %q and not %q", out.String(), tc.want, tc.not)
			}
		})
	}
}

// fakeReporter answers for the host.
type fakeReporter struct {
	images []tart.VerifiedImage
	slots  tart.SlotReport
	free   uint64
	size   int64
	// refreshed records that GitHub was asked.
	refreshed *bool
	orphans   []string
}

func (f fakeReporter) VerifiedImages(context.Context) ([]tart.VerifiedImage, error) {
	return f.images, nil
}

func (f fakeReporter) RefreshDeadlines(context.Context, tart.RunnerWindow) ([]string, error) {
	if f.refreshed != nil {
		*f.refreshed = true
	}

	return nil, nil
}
func (f fakeReporter) Slots(context.Context) (tart.SlotReport, error) { return f.slots, nil }

func (f fakeReporter) OrphanProbes(context.Context) ([]string, error) { return f.orphans, nil }
func (f fakeReporter) StoreSpace(tart.VerifiedRecord) (uint64, int64, error) {
	return f.free, f.size, nil
}

func TestACheckSaysWhatEachVerifiedImageHolds(t *testing.T) {
	t.Parallel()

	const key = "ghcr.io/a/b:1"

	record := tart.VerifiedRecord{Image: key, Base: "base-1", Runner: "2.337.0", GuestOS: "darwin", VerifiedAt: time.Now()}
	past := time.Now().Add(-time.Hour)

	for _, tc := range []struct {
		name  string
		image tart.VerifiedImage
		free  uint64
		want  []string
	}{
		{name: "no record", image: tart.VerifiedImage{}, want: []string{"NOT verified", "billet images verify " + key}},
		{
			name: "only an invalid record", want: []string{"NOT verified"},
			image: tart.VerifiedImage{Records: []tart.VerifiedRecord{func() tart.VerifiedRecord {
				bad := record
				bad.Invalid = "changed"

				return bad
			}()}},
		},
		{
			name: "only a withdrawn digest", want: []string{"NOT verified"},
			image: tart.VerifiedImage{Withdrawn: []string{"sha256:0"}, Records: []tart.VerifiedRecord{func() tart.VerifiedRecord {
				bad := record
				bad.Digest = "sha256:0"

				return bad
			}()}},
		},
		{
			name: "a good one", free: 1 << 40,
			image: tart.VerifiedImage{Records: []tart.VerifiedRecord{record}},
			want:  []string{"as base-1, runner 2.337.0", "two bases of this macOS image"},
		},
		{
			name: "past its window", free: 1 << 40,
			image: tart.VerifiedImage{
				Records: []tart.VerifiedRecord{record}, Deadlines: map[string]time.Time{"2.337.0": past},
			},
			want: []string{"went past GitHub's window"},
		},
		{
			name: "keeps yielding", free: 1 << 40,
			image: tart.VerifiedImage{Records: []tart.VerifiedRecord{record}, Yields: 3},
			want:  []string{"last 3 verification(s) gave way", "local down --node", "--no-yield"},
		},
		{
			name: "a base removed by hand", free: 1 << 40,
			image: tart.VerifiedImage{
				Records:      []tart.VerifiedRecord{record},
				BaseProblems: map[string]string{"base-1": "it is gone from tart's store"},
			},
			want: []string{"base-1: it is gone from tart's store", "next job on its tier fails"},
		},
		{
			name: "an older base and an invalid one", free: 1 << 40,
			image: tart.VerifiedImage{Records: []tart.VerifiedRecord{
				record,
				{Base: "base-older"},
				{Base: "base-bad", Invalid: "changed"},
			}},
			want: []string{"also holds base-older", "base base-bad is invalid"},
		},
		{
			name: "a base waiting and a digest that failed", free: 1 << 40,
			image: tart.VerifiedImage{
				Records: []tart.VerifiedRecord{record},
				Pending: &tart.VerifiedRecord{Base: "base-waiting", Digest: "sha256:1"},
				Failed:  &tart.FailedDigest{Digest: "sha256:2"},
			},
			want: []string{"holds base-waiting", "sha256:2 failed its verification"},
		},
		{
			name: "no room", free: 1 << 30,
			image: tart.VerifiedImage{Records: []tart.VerifiedRecord{record}},
			want:  []string{"only 1 GiB free and its base is 80 GiB"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			reportTartImage(cli.Env{Stdout: &out}, fakeReporter{free: tc.free, size: 80 << 30}, key, tc.image, nil)

			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("said %q, want %q", out.String(), want)
				}
			}
		})
	}
}

func TestACheckNamesAWindowItCouldNotAsk(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	reportTartImage(cli.Env{Stdout: &out}, fakeReporter{free: 1 << 40}, "k", tart.VerifiedImage{
		Records: []tart.VerifiedRecord{{Base: "b", Runner: "2.337.0"}},
	}, []string{"2.337.0"})

	if !strings.Contains(out.String(), "its window is UNKNOWN") {
		t.Fatalf("said %s", out.String())
	}
}

func TestACheckNamesASlotCountThatDiffersAndGuestsItCannotAccountFor(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	reportTartSlots(t.Context(), cli.Env{Stdout: &out}, fakeReporter{slots: tart.SlotReport{
		Recorded: 2, Held: 1, Limits: map[string]int{"abc": 3, "def": 2},
		Uncounted: []string{"pid 7, someone"},
	}})

	for _, want := range []string{"1 of 2 macOS guest slots held", "deployment abc names a limit of 3", "pid 7, someone"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("said %q, want %q", out.String(), want)
		}
	}

	if strings.Contains(out.String(), "deployment def") {
		t.Errorf("named a deployment that agrees: %s", out.String())
	}

	if !strings.Contains(out.String(), "no record of its macOS limit") {
		t.Errorf("did not say a launch is on Apple's default: %s", out.String())
	}
}

// A MAINTENANCE PROBE RUNS OFFLINE DURING AN UPGRADE, so it asks GitHub nothing and rewrites no
// record; an ordinary check asks again.
func TestAMaintenanceCheckAsksGitHubNothing(t *testing.T) {
	prev := openTartReport

	t.Cleanup(func() { openTartReport = prev })

	for _, tc := range []struct {
		skip bool
		want bool
	}{{skip: true, want: false}, {skip: false, want: true}} {
		var asked bool

		openTartReport = func(*config.Config) (tartReporter, error) {
			return fakeReporter{refreshed: &asked}, nil
		}

		cfg := &config.Config{Node: &config.NodeConfig{
			Name: "mac-1", Provider: config.ProviderTart, StateDir: t.TempDir(),
		}}

		deployment := filepath.Join(cfg.Node.StateDir, "deployment-id")
		if err := os.WriteFile(deployment, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer

		reportTartVerified(t.Context(), cli.Env{Stdout: &out}, cfg, nil, tc.skip)

		if asked != tc.want {
			t.Errorf("skip %v: asked GitHub %v, want %v; said %s", tc.skip, asked, tc.want, out.String())
		}
	}
}

// A RECORD THIS FILE'S TIERS DO NOT NAME IS STILL REPORTED: the tiers a launch uses are the control
// plane's, and a base nobody names still holds tens of gigabytes. An orphan probe is named too.
func TestACheckReportsEveryRecordWhateverThisFileNames(t *testing.T) {
	prev := openTartReport

	t.Cleanup(func() { openTartReport = prev })

	openTartReport = func(*config.Config) (tartReporter, error) {
		return fakeReporter{
			free: 1 << 40,
			images: []tart.VerifiedImage{{
				Key: "ghcr.io/a/recorded:1", Records: []tart.VerifiedRecord{{Base: "recorded-base"}},
			}},
			orphans: []string{"billetprobe-x"},
		}, nil
	}

	cfg := &config.Config{Node: &config.NodeConfig{
		Name: "mac-1", Provider: config.ProviderTart, StateDir: t.TempDir(),
	}}

	if err := os.WriteFile(filepath.Join(cfg.Node.StateDir, "deployment-id"),
		[]byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	reportTartVerified(t.Context(), cli.Env{Stdout: &out}, cfg, []string{"ghcr.io/a/named:1@verified"}, true)

	for _, want := range []string{"as recorded-base", "ghcr.io/a/named:1", "NOT verified", "billetprobe-x"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("said %q, want %q", out.String(), want)
		}
	}

	if keys := tartRecordedKeys(t.Context(), cfg); !keys["ghcr.io/a/recorded:1"] {
		t.Errorf("recorded keys = %v", keys)
	}
}

func TestACheckIsQuietAboutALimitItHasARecordOf(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	reportTartSlots(t.Context(), cli.Env{Stdout: &out}, fakeReporter{slots: tart.SlotReport{Limit: 2, LimitKnown: true}})

	if strings.Contains(out.String(), "no record") {
		t.Errorf("said %s", out.String())
	}
}

func TestADryRunOfTheNodeAloneDoesNotSayThereIsNoControlPlane(t *testing.T) {
	asLinux(t)

	cfgPath, _ := downConfig(t, true)
	stageDown(t, &fakeConverger{}, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), processEnv(), downOptions{configPath: cfgPath, nodeOnly: true, dryRun: true}); err != nil {
			t.Errorf("refused: %v", err)
		}
	})

	if strings.Contains(out, "runs no control plane") || !strings.Contains(out, "seal nothing (--node)") {
		t.Errorf("the plan says:\n%s", out)
	}
}

// --node STOPS THE NODE ALONE: no seal, the server left running, and the deployment
// keeps admitting work, which a plain `down` on a Mac running both roles does not.
func TestDownOfTheNodeAloneLeavesTheServerAndAdmission(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), processEnv(), downOptions{configPath: cfgPath, nodeOnly: true}); err != nil {
			t.Errorf("refused: %v", err)
		}
	})

	want := []string{"lock", "stop " + deploy.NodeUnitName, "disable " + deploy.NodeUnitName}
	if strings.Join(f.trace, " → ") != strings.Join(want, " → ") {
		t.Errorf("down did:\n  %s\nwant:\n  %s", strings.Join(f.trace, " → "), strings.Join(want, " → "))
	}

	if got := admissionNow(t, db); got.Mode == state.AdmissionSealed {
		t.Errorf("the deployment was sealed: %+v", got)
	}

	if !strings.Contains(out, "SKIPPED by --node") {
		t.Errorf("the report does not say what it left alone:\n%s", out)
	}

	// NOTHING WAS PROVED ABOUT THE LEDGER, and the report does not say it was.
	if strings.Contains(out, "records nothing outstanding") || !strings.Contains(out, "no ledger was read") {
		t.Errorf("the report claims more than it established:\n%s", out)
	}
}

func TestDownOfTheNodeAloneNeedsANode(t *testing.T) {
	asLinux(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	body := `server:
  listen: 127.0.0.1:7717
  state_dir: ` + filepath.Join(dir, "server-state") + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + filepath.Join(dir, "key.pem") + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runLocalDown(t.Context(), processEnv(), downOptions{configPath: cfgPath, nodeOnly: true})
	if err == nil || !strings.Contains(err.Error(), "declares no node") {
		t.Fatalf("answered %v", err)
	}
}
