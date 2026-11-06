package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

const (
	onTwo     = "billet-2vcpu-ubuntu-2404"
	onFour    = "billet-4vcpu-ubuntu-2404"
	onSixteen = "billet-16vcpu-ubuntu-2404"
)

// migrateTiers are a deployment's tiers as the reference deployment declares them.
func migrateTiers() []config.Tier {
	linux := func(label, runsOn string, vcpu int) config.Tier {
		return config.Tier{Label: label, Target: "default", RunsOn: runsOn, Provider: config.ProviderFirecracker,
			GuestOS: config.GuestLinux, VCPU: vcpu, Memory: config.ByteSize(vcpu) * 4 * config.GiB,
			Image: "ubuntu-2404-x64@verified"}
	}

	return []config.Tier{
		linux("platform-16vcpu", onSixteen, 16),
		linux("platform-2vcpu", onTwo, 2),
		linux("platform-4vcpu", onFour, 4),
		{Label: "billet-4vcpu-ubuntu-2404-arm", Target: "default", Provider: config.ProviderTart,
			GuestOS: config.GuestLinux, VCPU: 4, Memory: 12 * config.GiB, Image: "ghcr.io/acme/ubuntu-24.04-runner-arm64:1"},
		{Label: "billet-6vcpu-macos-26", Target: "default", Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
			VCPU: 6, Memory: 24 * config.GiB, Image: "ghcr.io/cirruslabs/macos-tahoe-xcode:26.5"},
		// Another target's, smaller than anything above: never this repository's.
		{Label: "theirs-1vcpu", Target: "theirs", RunsOn: "theirs-1vcpu", Provider: config.ProviderFirecracker,
			GuestOS: config.GuestLinux, VCPU: 1, Memory: 4 * config.GiB, Image: "ubuntu-2404-x64@verified"},
	}
}

func migrateConfig(tiers []config.Tier) *config.Config {
	return &config.Config{GitHub: &config.GitHubConfig{Org: "acme"},
		Targets: []config.GitHubConfig{{Name: "theirs", Org: "other"}},
		Tiers:   tiers}
}

func chooserOf(t *testing.T, tiers []config.Tier, mapped labelMaps, public bool) *tierChooser {
	t.Helper()

	c, err := newTierChooser(migrateConfig(tiers), "acme/api", mapped, public)
	if err != nil {
		t.Fatalf("newTierChooser: %v", err)
	}

	return c
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "migrate", name))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}

	return raw
}

func kindsOf(findings []finding) map[string]int {
	out := map[string]int{}
	for i := range findings {
		out[findings[i].Kind]++
	}

	return out
}

// EACH KIND OF `runs-on` THE ISSUE NAMES IS DECIDED AS IT SAYS, and the whole file
// beyond the edited values is byte for byte what it was: rewriting is by span, so
// comments, quoting, spacing and order cannot change.
func TestEachKindOfRunsOnIsDecidedAndNothingElseMoves(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		fixture string
		kinds   map[string]int
		// replace is applied to the fixture's own bytes to get what it must become.
		replace [][2]string
		says    []string
	}{
		{"plain.yml", map[string]int{findRewritten: 3}, [][2]string{
			{"runs-on: ubuntu-latest   # why this runner", "runs-on: " + onTwo + "   # why this runner"},
			{`runs-on: "ubuntu-24.04"`, `runs-on: "` + onTwo + `"`},
			{"runs-on:    'ubuntu-latest'", "runs-on:    '" + onTwo + "'"},
		}, []string{`job "bare": "ubuntu-latest": to ` + onTwo + ", tier platform-2vcpu (2 vCPU, 8.0 GiB, x64)"}},
		{"list.yml", map[string]int{findRewritten: 2}, [][2]string{
			{"runs-on: [ubuntu-latest]", "runs-on: [" + onTwo + "]"},
			{"      - ubuntu-24.04", "      - " + onTwo},
		}, nil},
		{"matrix.yml", map[string]int{findExpression: 1, findMatrix: 4}, nil, []string{
			`"ubuntu-latest": would be ` + onTwo, `"macos-latest": would be billet-6vcpu-macos-26`,
			`"windows-latest": no tier for it`, `"ubuntu-24.04-arm": would be billet-4vcpu-ubuntu-2404-arm`,
		}},
		{"expression.yml", map[string]int{findExpression: 2}, nil, []string{"only GitHub can evaluate it"}},
		{"group.yml", map[string]int{findGroup: 2}, nil, []string{"the organization's to decide"}},
		{"mixed.yml", map[string]int{findList: 2, findUnknown: 1}, nil, []string{
			`"[self-hosted, linux, x64]"`, `"[ubuntu-latest, gpu]"`, `"our-own-runners": it is neither a label GitHub hosts`,
		}},
		{"reusable.yml", map[string]int{findCall: 1, findCaller: 1}, nil, []string{
			`"./.github/workflows/called.yml"`, `"ubuntu-latest": input "os"`,
		}},
		{"called.yml", map[string]int{findRewritten: 1, findExpression: 1},
			[][2]string{{"    runs-on: ubuntu-latest\n", "    runs-on: " + onTwo + "\n"}}, nil},
		{"onbillet.yml", map[string]int{findOnBillet: 2}, nil, nil},
	} {
		before := fixture(t, tc.fixture)
		got := rewriteWorkflow(".github/workflows/"+tc.fixture, before, chooserOf(t, migrateTiers(), labelMaps{}, false), nil)

		if kinds := kindsOf(got.findings); !maps.Equal(kinds, tc.kinds) {
			t.Errorf("%s: findings %v, want %v: %+v", tc.fixture, kinds, tc.kinds, got.findings)
		}

		want := string(before)
		for _, r := range tc.replace {
			if !strings.Contains(want, r[0]) {
				t.Fatalf("%s: the fixture does not hold %q", tc.fixture, r[0])
			}
			want = strings.Replace(want, r[0], r[1], 1)
		}
		if string(got.after) != want {
			t.Errorf("%s: became\n%s\nwant\n%s", tc.fixture, got.after, want)
		}

		report := (&migrationPlan{findings: got.findings}).reportText()
		for _, says := range tc.says {
			if !strings.Contains(report, says) {
				t.Errorf("%s: the report does not say %q:\n%s", tc.fixture, says, report)
			}
		}
	}
}

// AN EDIT IS MADE ONLY WHERE THE FILE'S BYTES ARE THE LABEL: a parser's position is a
// claim, and what it cannot back is left alone and said. Everything here is a
// shape in which a span computed from the parser alone would corrupt the file.
func TestAnEditIsMadeOnlyWhereTheBytesAreTheLabel(t *testing.T) {
	t.Parallel()

	head := "on: [push]\njobs:\n"
	for _, tc := range []struct {
		name, job string
		// want is the job as it must end up; empty leaves it as it was.
		want string
		kind string
	}{
		{"a non-ASCII byte before the label on its line, where a column in bytes is short",
			`  "ünï": {name: "wörld", runs-on: ubuntu-latest, steps: [{run: echo}]}` + "\n",
			`  "ünï": {name: "wörld", runs-on: ` + onTwo + `, steps: [{run: echo}]}` + "\n", findRewritten},
		{"the label's own spelling, since GitHub compares labels without case",
			"  a:\n    runs-on: Ubuntu-Latest\n", "  a:\n    runs-on: " + onTwo + "\n", findRewritten},
		{"CRLF line ends", "  a:\r\n    runs-on: ubuntu-latest\r\n    steps: []\r\n",
			"  a:\r\n    runs-on: " + onTwo + "\r\n    steps: []\r\n", findRewritten},
		{"no newline at the end of the file", "  a:\n    runs-on: ubuntu-latest", "  a:\n    runs-on: " + onTwo, findRewritten},
		{"a folded block scalar", "  a:\n    runs-on: >-\n      ubuntu-latest\n", "", "a block scalar"},
		{"a literal block scalar", "  a:\n    runs-on: |-\n      ubuntu-latest\n", "", "a block scalar"},
		{"an escape inside quotes", "  a:\n    runs-on: \"ubuntu\\x2dlatest\"\n", "", "written with an escape"},
		{"an anchor other jobs may read", "  a:\n    runs-on: &r ubuntu-latest\n  b:\n    runs-on: *r\n", "", "a YAML anchor"},
		{"an anchored list", "  a:\n    runs-on: &r [ubuntu-latest]\n", "", "a YAML anchor"},
	} {
		before := head + tc.job
		got := rewriteWorkflow("w.yml", []byte(before), chooserOf(t, migrateTiers(), labelMaps{}, false), nil)
		want := before
		if tc.want != "" {
			want = head + tc.want
		}
		if string(got.after) != want {
			t.Errorf("%s: became %q, want %q", tc.name, got.after, want)
		}
		report := (&migrationPlan{findings: got.findings}).reportText()
		if kindsOf(got.findings)[tc.kind] == 0 && !strings.Contains(report, tc.kind) {
			t.Errorf("%s: findings %+v, want one %q", tc.name, got.findings, tc.kind)
		}
	}

	for _, data := range []string{"jobs: [unterminated\n", "jobs:\n  a:\n\t\tbad: tab\n"} {
		got := rewriteWorkflow("w.yml", []byte(data), chooserOf(t, migrateTiers(), labelMaps{}, false), nil)
		if string(got.after) != data || kindsOf(got.findings)[findUnreadable] != 1 {
			t.Errorf("%q: became %q with %+v; want it left alone and said", data, got.after, got.findings)
		}
	}
	for _, data := range []string{"", "on: [push]\n", "jobs: nothing\n", "jobs:\n  a: nothing\n", "jobs:\n  a:\n    steps: []\n"} {
		got := rewriteWorkflow("w.yml", []byte(data), chooserOf(t, migrateTiers(), labelMaps{}, false), nil)
		if string(got.after) != data || len(got.findings) != 0 {
			t.Errorf("%q: became %q with %+v; want nothing said about nothing", data, got.after, got.findings)
		}
	}
}

// GITHUB'S LABELS ARE READ BY THEIR FORM, as its reference gave them on 2026-11-03.
func TestAHostedLabelIsReadForWhatItGives(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label  string
		public bool
		want   hostedLabel
	}{
		{"ubuntu-latest", false, hostedLabel{OS: config.GuestLinux, Arch: config.ArchX64, VCPU: 2, MemoryGB: 8}},
		{"ubuntu-latest", true, hostedLabel{OS: config.GuestLinux, Arch: config.ArchX64, VCPU: 4, MemoryGB: 16}},
		{"UBUNTU-LATEST", false, hostedLabel{OS: config.GuestLinux, Arch: config.ArchX64, VCPU: 2, MemoryGB: 8}},
		{"ubuntu-24.04", false, hostedLabel{OS: config.GuestLinux, Arch: config.ArchX64, Version: "24.04", VCPU: 2, MemoryGB: 8}},
		{"ubuntu-26.04", true, hostedLabel{OS: config.GuestLinux, Arch: config.ArchX64, Version: "26.04", VCPU: 4, MemoryGB: 16}},
		{"ubuntu-22.04-arm", false, hostedLabel{OS: config.GuestLinux, Arch: config.ArchARM64, Version: "22.04", VCPU: 2, MemoryGB: 8}},
		{"ubuntu-24.04-arm", true, hostedLabel{OS: config.GuestLinux, Arch: config.ArchARM64, Version: "24.04", VCPU: 4, MemoryGB: 16}},
		{"ubuntu-slim", true, hostedLabel{OS: config.GuestLinux, Arch: config.ArchX64, VCPU: 1, MemoryGB: 5}},
		{"macos-latest", true, hostedLabel{OS: config.GuestMacOS, Arch: config.ArchARM64, VCPU: 3, MemoryGB: 7}},
		{"macos-15", false, hostedLabel{OS: config.GuestMacOS, Arch: config.ArchARM64, Version: "15", VCPU: 3, MemoryGB: 7}},
		{"macos-15-intel", false, hostedLabel{OS: config.GuestMacOS, Arch: config.ArchX64, Version: "15", VCPU: 4, MemoryGB: 14}},
		{"macos-26-large", false, hostedLabel{OS: config.GuestMacOS, Arch: config.ArchX64, Version: "26", VCPU: 12, MemoryGB: 30}},
		{"macos-26-xlarge", false, hostedLabel{OS: config.GuestMacOS, Arch: config.ArchARM64, Version: "26", VCPU: 5, MemoryGB: 14}},
		{"macos-latest-large", false, hostedLabel{OS: config.GuestMacOS, Arch: config.ArchX64, VCPU: 12, MemoryGB: 30}},
		{"macos-latest-xlarge", false, hostedLabel{OS: config.GuestMacOS, Arch: config.ArchARM64, VCPU: 5, MemoryGB: 14}},
	} {
		got, ok := lookupHostedLabel(tc.label, tc.public)
		if !ok || got != tc.want {
			t.Errorf("%s (public %t) = %+v, %t; want %+v", tc.label, tc.public, got, ok, tc.want)
		}
	}

	for _, label := range []string{"windows-latest", "windows-2022", "windows-11-arm", "xcode-27", "macos-13", "macos-12"} {
		if got, ok := lookupHostedLabel(label, false); !ok || got.NoMapping == "" {
			t.Errorf("%s = %+v, %t; want a label GitHub hosts that is never chosen for", label, got, ok)
		}
	}
	for _, label := range []string{"", "self-hosted", "linux", "ubuntu", "ubuntu-24", "ubuntu-24.04-x64", "macos", "macos-15-mini",
		"xcode", "xcode-latest", "ubuntu-latest-16-cores"} {
		if got, ok := lookupHostedLabel(label, false); ok {
			t.Errorf("%q = %+v; want a label this does not know", label, got)
		}
	}
}

// A TIER IS CHOSEN ONLY WHERE ITS PLATFORM IS THE LABEL'S AND ITS SHAPE HOLDS WHAT
// THE LABEL GIVES, the smallest of those; otherwise nothing is written and the reason
// says what would change it.
func TestATierIsChosenByPlatformAndSize(t *testing.T) {
	t.Parallel()

	replaced := func(label string, change func(*config.Tier)) []config.Tier {
		tiers := migrateTiers()
		i := slices.IndexFunc(tiers, func(t config.Tier) bool { return t.Label == label })
		change(&tiers[i])

		return tiers
	}
	const file = ".github/workflows/ci.yml"

	for _, tc := range []struct {
		name, label string
		tiers       []config.Tier
		public      bool
		mapped      labelMaps
		want, says  string
	}{
		{name: "the smallest that holds it", label: "ubuntu-latest", want: onTwo},
		{name: "a public repository's larger shape", label: "ubuntu-latest", public: true, want: onFour},
		{name: "never another target's tier", label: "ubuntu-slim", want: onTwo},
		{name: "a matching version", label: "ubuntu-24.04", want: onTwo},
		{name: "another version is not moved to", label: "ubuntu-22.04", says: "on another version of the OS"},
		{name: "arm goes to arm", label: "ubuntu-24.04-arm", want: "billet-4vcpu-ubuntu-2404-arm"},
		{name: "public arm needs more memory than it has", label: "ubuntu-24.04-arm", public: true,
			says: "smaller than 4 vCPU and 16 GB"},
		{name: "macOS by version", label: "macos-26", want: "billet-6vcpu-macos-26"},
		{name: "macOS latest on the family", label: "macos-latest", want: "billet-6vcpu-macos-26"},
		{name: "a macOS this deployment does not run", label: "macos-15", says: "on another version of the OS"},
		{name: "Intel macOS has no tier", label: "macos-26-intel", says: "no tier of guest OS macos runs on x64"},
		{name: "a larger shape than any tier", label: "macos-26-xlarge", tiers: replaced("billet-6vcpu-macos-26",
			func(t *config.Tier) { t.VCPU = 4 }), says: "smaller than 5 vCPU and 14 GB"},
		{name: "Windows", label: "windows-latest", says: "billet has no Windows runner"},
		{name: "Xcode", label: "xcode-27", says: "pass --map"},
		{name: "a name of somebody's own", label: "our-gpus", says: "pass --map for it"},
		{name: "--map for a name of somebody's own", label: "our-gpus", mapped: labelMaps{"our-gpus": "platform-16vcpu"}, want: onSixteen},
		{name: "--map over a choice", label: "ubuntu-latest", mapped: labelMaps{"ubuntu-latest": "platform-4vcpu"}, want: onFour},
		{name: "--map is not case-sensitive either", label: "ubuntu-latest", mapped: func() labelMaps {
			m := labelMaps{}
			if err := m.Set("Ubuntu-Latest=platform-4vcpu"); err != nil {
				t.Fatal(err)
			}
			return m
		}(), want: onFour},
		{name: "no architecture named is not guessed, even for a label it could serve", label: "ubuntu-latest",
			tiers: []config.Tier{{Label: "docker", Target: "default", Provider: config.ProviderDocker,
				GuestOS: config.GuestLinux, VCPU: 2, Memory: 8 * config.GiB, Image: "ghcr.io/actions/actions-runner:latest"}},
			says: "name `arch` on them or pass --map"},
		{name: "the tier's own arch is enough", label: "ubuntu-latest",
			tiers: []config.Tier{{Label: "docker", Target: "default", Arch: config.ArchX64, Provider: config.ProviderDocker,
				GuestOS: config.GuestLinux, VCPU: 2, Memory: 8 * config.GiB, Image: "ghcr.io/actions/actions-runner:latest"}},
			want: "docker"},
		{name: "a trusted tier not listing the workflow", label: "ubuntu-latest",
			tiers: []config.Tier{{Label: "trusted", Target: "default", Trust: config.WorkloadTrusted,
				Workflows: []string{"acme/api/.github/workflows/release.yml@refs/heads/main"}, Provider: config.ProviderFirecracker,
				GuestOS: config.GuestLinux, VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64@verified"}},
			says: "do not list this workflow"},
		{name: "a trusted tier listing it at any ref", label: "ubuntu-latest",
			tiers: []config.Tier{{Label: "trusted", Target: "default", Trust: config.WorkloadTrusted,
				Workflows: []string{"acme/api/.github/workflows/ci.yml@refs/heads/main"}, Provider: config.ProviderFirecracker,
				GuestOS: config.GuestLinux, VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64@verified"}},
			want: "trusted"},
		{name: "no tier of the OS at all", label: "macos-latest", tiers: []config.Tier{migrateTiers()[0]},
			says: "no tier serving this repository has guest OS macos"},
		{name: "one memory short is not enough", label: "ubuntu-latest", tiers: []config.Tier{{Label: "tiny", Target: "default",
			Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux, VCPU: 2, Memory: 7 * config.GiB,
			Image: "ubuntu-2404-x64@verified"}}, says: "smaller than 2 vCPU and 8 GB"},
	} {
		tiers := tc.tiers
		if tiers == nil {
			tiers = migrateTiers()
		}
		mapped := tc.mapped
		if mapped == nil {
			mapped = labelMaps{}
		}
		to, detail, _ := chooserOf(t, tiers, mapped, tc.public).choose(tc.label, file)
		if to != tc.want {
			t.Errorf("%s: chose %q (%s), want %q", tc.name, to, detail, tc.want)
		}
		if tc.says != "" && !strings.Contains(detail, tc.says) {
			t.Errorf("%s: the reason %q does not say %q", tc.name, detail, tc.says)
		}
	}

	for _, tc := range []struct {
		repository string
		mapped     labelMaps
		says       string
	}{
		{"acme/api", labelMaps{"ubuntu-latest": "no-such-tier"}, "names a tier this configuration does not give"},
		{"acme/api", labelMaps{"ubuntu-latest": "theirs-1vcpu"}, "names a tier this configuration does not give"},
		{"elsewhere/api", labelMaps{}, `no target of this configuration serves "elsewhere/api"`},
	} {
		_, err := newTierChooser(migrateConfig(migrateTiers()), tc.repository, tc.mapped, false)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s with %v = %v, want it refused saying %q", tc.repository, tc.mapped, err, tc.says)
		}
	}

	set := labelMaps{}
	for _, bad := range []string{"", "label", "=tier", "label=", " =tier"} {
		if err := set.Set(bad); err == nil {
			t.Errorf("--map %q was taken", bad)
		}
	}
}

// A REPOSITORY TARGET'S TIERS ARE THAT REPOSITORY'S, and GitHub's names have no case.
func TestATargetServesTheRepositoryItNames(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Targets: []config.GitHubConfig{
		{Name: "repo", Repository: "acme/api"},
		{Name: "org", Org: "other"},
	}, Tiers: []config.Tier{
		{Label: "repo-tier", Target: "repo"}, {Label: "org-tier", Target: "org"},
	}}

	c, err := newTierChooser(cfg, "ACME/api", labelMaps{}, false)
	if err != nil {
		t.Fatalf("newTierChooser: %v", err)
	}
	if !c.onBillet("repo-tier") || c.onBillet("org-tier") {
		t.Errorf("tiers = %v; want the repository target's alone", c.names)
	}

	if _, err := newTierChooser(cfg, "acme/web", labelMaps{}, false); err == nil {
		t.Error("a repository target served a sibling repository")
	}
	if c, err := newTierChooser(cfg, "OTHER/web", labelMaps{}, false); err != nil || !c.onBillet("org-tier") {
		t.Errorf("an organization target did not serve its repository: %v, %v", c, err)
	}
}

// writeRightsize writes jobs as `billet jobs rightsize --json` writes them.
func writeRightsize(t *testing.T, schema any, jobs ...rightsizeEntry) string {
	t.Helper()

	body, err := json.Marshal(map[string]any{"schema": schema, "jobs": jobs})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "rightsize.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	return path
}

// A RECOMMENDATION MOVES A JOB ONLY WHERE ITS NAME PROVES WHICH JOB IT MEANS AND THE
// JOB IS ON THE TIER IT JUDGED, to a runs-on this configuration gives the repository;
// anything else stays and is said. The report is a file, and a file is not trusted to
// put text into a workflow.
func TestARecommendationMovesOnlyTheJobItProvablyMeans(t *testing.T) {
	t.Parallel()

	const file = ".github/workflows/ci.yml"
	before := `on: [push]
jobs:
  by-key:
    runs-on: ` + onFour + ` # stays commented
  by-name:
    name: Shown name
    runs-on: "` + onFour + `"
  key-of-a-named-job:
    name: Another name
    runs-on: ` + onFour + `
  legs:
    strategy:
      matrix:
        n: [1, 2]
    runs-on: ` + onFour + `
  computed:
    name: ${{ github.ref }}
    runs-on: ` + onFour + `
  twin-a:
    name: Twin
    runs-on: ` + onFour + `
  twin-b:
    name: Twin
    runs-on: ` + onFour + `
  moved-since:
    runs-on: ` + onSixteen + `
  forged:
    runs-on: ` + onFour + `
  still-hosted:
    runs-on: ubuntu-latest
  web-only:
    runs-on: ` + onFour + `
`
	entry := func(job string) rightsizeEntry {
		return rightsizeEntry{Repository: "acme/api", Workflow: "acme/api/" + file, Job: job, CurrentRunsOn: onFour,
			Recommendation: "smaller", Reason: "idle", Confidence: "high", RecommendedTier: "platform-2vcpu",
			RecommendedRunsOn: onTwo}.judgedAt(4, 2)
	}
	forged := entry("forged")
	forged.RecommendedRunsOn = onTwo + "\n    evil: true"
	otherRepository := entry("by-key")
	otherRepository.Repository, otherRepository.Workflow = "acme/web", "acme/web/"+file
	otherOwnersWorkflow := entry("by-key")
	otherOwnersWorkflow.Workflow = "acme/shared/" + file
	noTier := entry("by-key")
	noTier.RecommendedRunsOn = ""
	// Another repository's entry that names this repository's workflow is still
	// another repository's.
	elsewhere := entry("web-only")
	elsewhere.Repository = "acme/web"
	// A second entry for a job the first moved cannot move it again.
	again := entry("by-key")

	path := writeRightsize(t, 1, entry("by-key"), entry("Shown name"), entry("key-of-a-named-job"), entry("legs"),
		entry("computed"), entry("${{ github.ref }}"), entry("Twin"), entry("moved-since"), forged, entry("still-hosted"),
		entry("no-such-job"), otherRepository, otherOwnersWorkflow, noTier, elsewhere, again)
	entries, err := readRightsize(path, "acme/api")
	if err != nil {
		t.Fatalf("readRightsize: %v", err)
	}

	got := rewriteWorkflow(file, []byte(before), chooserOf(t, migrateTiers(), labelMaps{}, false), entries)

	want := strings.Replace(before, "by-key:\n    runs-on: "+onFour, "by-key:\n    runs-on: "+onTwo, 1)
	want = strings.Replace(want, `runs-on: "`+onFour+`"`, `runs-on: "`+onTwo+`"`, 1)
	// The one hosted label is moved by the mapping, not by the recommendation.
	want = strings.Replace(want, "runs-on: ubuntu-latest", "runs-on: "+onTwo, 1)
	if string(got.after) != want {
		t.Errorf("became\n%s\nwant\n%s", got.after, want)
	}

	report := (&migrationPlan{findings: got.findings}).reportText()
	for _, says := range []string{
		`"key-of-a-named-job": no job here on one of this deployment's tiers is named that`,
		`"legs": the job has a matrix`,
		`"${{ github.ref }}": no job here`,
		`job "Twin"`, "2 jobs here are named that",
		`"moved-since": it runs on "` + onSixteen + `" here, and the recommendation was for "` + onFour + `"`,
		`"forged": it recommends "billet-2vcpu-ubuntu-2404\n    evil: true", which is no one tier`,
		`"still-hosted": no job here`,
		`"no-such-job": no job here`,
		`"idle"; confidence "high"`,
	} {
		says = strings.ReplaceAll(says, "\n", `\n`)
		if !strings.Contains(report, says) {
			t.Errorf("the report does not say %q:\n%s", says, report)
		}
	}
	for _, f := range got.findings {
		if f.Job == "web-only" && (f.Kind == findRightsize || f.Kind == findRewritten) {
			t.Errorf("another repository's entry was read: %+v", f)
		}
	}
	if !strings.Contains(report, `"by-key": another recommendation in the report already moved it`) {
		t.Errorf("a second entry for a moved job was not refused:\n%s", report)
	}
	if strings.Contains(string(got.after), "evil") {
		t.Errorf("a forged recommendation reached the workflow:\n%s", got.after)
	}

	for _, tc := range []struct {
		schema any
		says   string
	}{{2, "is not schema 1"}, {nil, "is not schema 1"}} {
		if _, err := readRightsize(writeRightsize(t, tc.schema), "acme/api"); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("schema %v = %v, want refused saying %q", tc.schema, err, tc.says)
		}
	}
	bad := filepath.Join(t.TempDir(), "not.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRightsize(bad, "acme/api"); err == nil || !strings.Contains(err.Error(), "is not the output of") {
		t.Errorf("a file that is not JSON = %v", err)
	}
	if _, err := readRightsize(filepath.Join(t.TempDir(), "absent"), "acme/api"); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file = %v, want not found", err)
	}
}

// A REMOTE IS READ IN EVERY FORM GIT TAKES, and never carries its userinfo into what
// the command says.
func TestARemoteIsReadForItsRepository(t *testing.T) {
	t.Parallel()

	for remote, want := range map[string]string{
		"git@github.com:acme/api.git":                   "acme/api",
		"git@github.com:acme/api":                       "acme/api",
		"https://github.com/acme/api.git":               "acme/api",
		"https://github.com/acme/api":                   "acme/api",
		"https://x-access-token:s3cret@github.com/a/b/": "a/b",
		"ssh://git@github.com/acme/api.git":             "acme/api",
		"ssh://git@github.com:22/acme/api.git":          "acme/api",
	} {
		if got, ok := repositoryOfRemote(remote); !ok || got != want {
			t.Errorf("%s = %q, %t; want %q", remote, got, ok, want)
		}
	}
	// A path on this machine names no owner, a colon in it included.
	for _, remote := range []string{"", "/srv/git/api.git", "https://github.com/acme", "https://github.com/a/b/c", "github.com",
		"../mirrors:alice/api.git", "/srv/mirrors:alice/api.git", "file:///srv/alice/api.git",
		"file://localhost/srv/alice/api.git"} {
		if got, ok := repositoryOfRemote(remote); ok {
			t.Errorf("%q = %q; want no answer", remote, got)
		}
	}
}

// runIn runs git in dir and fails the test on an error.
func runIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}

	return string(out)
}

// checkout is a git repository holding workflows, committed, with a bare origin.
func checkout(t *testing.T, workflows map[string]string) string {
	t.Helper()

	origin := t.TempDir()
	runIn(t, origin, "init", "-q", "--bare", "-b", "main")

	dir := t.TempDir()
	runIn(t, dir, "init", "-q", "-b", "main")
	runIn(t, dir, "config", "user.email", "test@example.invalid")
	runIn(t, dir, "config", "user.name", "test")
	runIn(t, dir, "config", "commit.gpgsign", "false")
	// ORIGIN READS AS THE REPOSITORY ON GITHUB AND PUSHES TO THE BARE ONE BESIDE IT, so
	// the command reads owner/name from it as from a real checkout.
	runIn(t, dir, "remote", "add", "origin", "https://github.com/acme/api.git")
	runIn(t, dir, "config", "url."+origin+".pushInsteadOf", "https://github.com/acme/api.git")

	workflowDir := filepath.Join(dir, filepath.FromSlash(workflowsDir))
	if err := os.MkdirAll(workflowDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range workflows {
		if err := os.WriteFile(filepath.Join(workflowDir, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	runIn(t, dir, "add", ".")
	runIn(t, dir, "commit", "-q", "-m", "init")

	return dir
}

// migrateConfigFile writes a configuration a command loads.
func migrateConfigFile(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	key := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(key, []byte("not read by this command"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "billet.yaml")
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + t.TempDir() + `
  max_vcpu: 64
  max_memory: 256GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + key + `
tiers:
  - label: billet-2vcpu
    arch: x64
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
  - label: billet-4vcpu
    arch: x64
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ubuntu:24.04
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// migrateRun runs the command and returns what it wrote to each stream.
func migrateRun(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	env := cli.Env{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	err := Migrate(t.Context(), env, append([]string{"workflows"}, args...))

	return stdout.String(), stderr.String(), err
}

const migrateWorkflow = "on: [push]\n\njobs:\n  test:\n    runs-on: ubuntu-latest # keep\n    steps:\n      - run: a\n      - run: b\n      - run: c\n      - run: d\n"

// WITHOUT --write NOTHING IS WRITTEN, THE DIFF IS WHAT `git apply` TAKES on stdout and
// the report is apart on stderr; with --write the file holds exactly what the diff
// said, with its mode, and git sees only that change.
func TestMigrateWorkflowsPrintsADiffAndWritesOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	noNewline := "on: [push]\njobs:\n  z:\n    runs-on: ubuntu-latest"
	// A change on a newline-ended file's last line, where a hunk runs to the end.
	atTheEnd := "on: [push]\njobs:\n  z:\n    steps: []\n    runs-on: ubuntu-latest\n"
	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow, "last.yaml": noNewline, "a b.yml": migrateWorkflow,
		"end.yml": atTheEnd, "notes.txt": "runs-on: ubuntu-latest\n"})
	cfg := migrateConfigFile(t)
	target := filepath.Join(dir, ".github", "workflows", "ci.yml")

	stdout, stderr, err := migrateRun(t, "--config", cfg, "--path", dir, "--repo", "acme/api")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, stderr)
	}
	if got := readFile(t, target); got != migrateWorkflow {
		t.Errorf("without --write the file became %q", got)
	}
	if !strings.Contains(stderr, `"ubuntu-latest": to billet-2vcpu`) || strings.Contains(stdout, "rewritten") {
		t.Errorf("the report is not apart from the diff:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if strings.Contains(stdout, "notes.txt") {
		t.Errorf("a file that is no workflow was read:\n%s", stdout)
	}

	// The diff applies, by the tool a person would use it with.
	patch := filepath.Join(t.TempDir(), "migrate.patch")
	if err := os.WriteFile(patch, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, dir, "apply", patch)
	applied, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(migrateWorkflow, "ubuntu-latest", "billet-2vcpu", 1)
	if string(applied) != want {
		t.Errorf("the diff applied to %q, want %q", applied, want)
	}
	if got := readFile(t, filepath.Join(dir, ".github", "workflows", "last.yaml")); got !=
		strings.Replace(noNewline, "ubuntu-latest", "billet-2vcpu", 1) {
		t.Errorf("the diff of a file with no final newline applied to %q", got)
	}
	if got := readFile(t, filepath.Join(dir, ".github", "workflows", "end.yml")); got !=
		strings.Replace(atTheEnd, "ubuntu-latest", "billet-2vcpu", 1) {
		t.Errorf("the diff of a change on the last line applied to %q", got)
	}
	runIn(t, dir, "checkout", "-q", ".")
	// git keeps no mode but the executable bit, so what it just restored is the umask's.
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, stderr, err := migrateRun(t, "--config", cfg, "--path", dir, "--repo", "acme/api", "--write"); err != nil {
		t.Fatalf("migrate --write: %v\n%s", err, stderr)
	}
	written, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != want {
		t.Errorf("--write wrote %q, want %q", written, want)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("--write left mode %o, want the file's 640", info.Mode().Perm())
	}

	// Once moved, a second run has nothing to do and says it is already on billet.
	if err := os.Remove(filepath.Join(dir, ".github", "workflows", "notes.txt")); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = migrateRun(t, "--config", cfg, "--path", dir, "--repo", "acme/api", "--write")
	if err != nil || stdout != "" || !strings.Contains(stderr, "already on billet (4)") || !strings.Contains(stderr, "nothing to change") {
		t.Errorf("a second run: %v, stdout %q, stderr %q", err, stdout, stderr)
	}
}

// THE REPOSITORY IS READ FROM THE CHECKOUT'S ORIGIN, and a remote holding a credential
// is never quoted: what the command prints lands in logs and pull requests.
func TestMigrateWorkflowsReadsTheRepositoryFromOrigin(t *testing.T) {
	t.Parallel()

	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	cfg := migrateConfigFile(t)

	runIn(t, dir, "remote", "set-url", "origin", "https://x-access-token:s3cret@github.com/acme/api.git")
	stdout, stderr, err := migrateRun(t, "--config", cfg, "--path", dir)
	if err != nil || !strings.Contains(stdout, "billet-2vcpu") {
		t.Errorf("reading acme/api from origin: %v\n%s\n%s", err, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, "s3cret") {
		t.Errorf("the credential was printed:\n%s\n%s", stdout, stderr)
	}

	runIn(t, dir, "remote", "set-url", "origin", "https://x-access-token:s3cret@github.com/elsewhere/api.git")
	if _, _, err := migrateRun(t, "--config", cfg, "--path", dir); err == nil || !strings.Contains(err.Error(), "elsewhere/api") ||
		strings.Contains(err.Error(), "s3cret") {
		t.Errorf("an origin no target serves = %v, want it named without the credential", err)
	}

	runIn(t, dir, "remote", "set-url", "origin", "/srv/s3cret/api.git")
	if _, _, err := migrateRun(t, "--config", cfg, "--path", dir); err == nil || !strings.Contains(err.Error(), "pass --repo") ||
		strings.Contains(err.Error(), "s3cret") {
		t.Errorf("an origin naming no repository = %v, want it refused without quoting it", err)
	}

	runIn(t, dir, "remote", "remove", "origin")
	if _, _, err := migrateRun(t, "--config", cfg, "--path", dir); err == nil || !strings.Contains(err.Error(), "pass --repo") {
		t.Errorf("no origin = %v, want it refused", err)
	}

	if _, _, err := migrateRun(t, "--config", cfg, "--path", dir, "--repo", "api"); err == nil ||
		!strings.Contains(err.Error(), `--repo takes owner/name, not "api"`) {
		t.Errorf("--repo api = %v", err)
	}
	if _, _, err := migrateRun(t, "--config", cfg, "--path", t.TempDir(), "--repo", "acme/api"); err == nil ||
		!errors.Is(err, os.ErrNotExist) {
		t.Errorf("a checkout with no workflows = %v, want not found rather than nothing to do", err)
	}
}

// stubGH puts a gh on PATH that records its arguments, and returns where.
//
// NOT PARALLEL-SAFE: PATH is the process's.
func stubGH(t *testing.T, exit string) string {
	t.Helper()

	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + record + "'; done\n" +
		"cat > '" + record + ".body'\necho https://github.com/acme/api/pull/7\n" + exit + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return record
}

// --open-pr DOES IT WITH THE OPERATOR'S OWN git AND gh, on a new branch, and commits
// the workflows it changed and nothing else; the branch reaches the remote, and the
// pull request's address is what stdout ends with.
func TestMigrateWorkflowsOpensThePullRequestWithTheOperatorsTools(t *testing.T) {
	record := stubGH(t, "exit 0")
	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow, "unmoved.yml": "on: [push]\njobs:\n  a:\n    runs-on: our-own\n"})
	cfg := migrateConfigFile(t)

	// Origin fetches from acme and pushes to alice's fork: the head is alice's.
	runIn(t, dir, "config", "remote.origin.pushurl", "https://github.com/alice/api.git")
	// pushInsteadOf IS NOT APPLIED TO AN EXPLICIT PUSH URL, insteadOf is.
	runIn(t, dir, "config", "url."+originOf(t, dir)+".insteadOf", "https://github.com/alice/api.git")
	// A NEW BRANCH THAT TRACKS main, pushed by a bare name under push.default=upstream,
	// would land on origin's main; the push names its destination.
	runIn(t, dir, "config", "branch.autoSetupMerge", "always")
	runIn(t, dir, "config", "push.default", "upstream")
	bare := originOf(t, dir)
	runIn(t, dir, "push", "-q", "origin", "refs/heads/main:refs/heads/main")
	mainBefore := runIn(t, bare, "rev-parse", "refs/heads/main")
	if mainBefore == "" {
		t.Fatal("origin has no main to compare with")
	}
	// NOTHING BUT THE BRANCH IS PUSHED, whatever push.followTags says, and a push the
	// operator's push.recurseSubmodules=only would make a success that sent nothing
	// still sends the branch.
	runIn(t, dir, "tag", "-a", "-m", "unreleased", "v1.0.0")
	runIn(t, dir, "config", "push.followTags", "true")
	runIn(t, dir, "config", "push.recurseSubmodules", "only")

	// The repository named in another case than origin's: the pull request goes to it
	// as named, from the branch on the fork origin pushes to.
	stdout, stderr, err := migrateRun(t, "--config", cfg, "--path", dir, "--repo", "Acme/API", "--open-pr")
	if err != nil {
		t.Fatalf("--open-pr: %v\n%s", err, stderr)
	}
	if !strings.HasSuffix(stdout, "https://github.com/acme/api/pull/7\n") {
		t.Errorf("stdout does not end with the pull request:\n%s", stdout)
	}

	if mainAfter := runIn(t, bare, "rev-parse", "refs/heads/main"); mainAfter != mainBefore {
		t.Errorf("origin's main moved from %q to %q", mainBefore, mainAfter)
	}
	if tags := runIn(t, bare, "tag", "--list"); tags != "" {
		t.Errorf("origin was sent the operator's tags: %q", tags)
	}

	branch := strings.TrimSpace(runIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	if !strings.HasPrefix(branch, "billet/migrate-workflows-") {
		t.Errorf("the checkout is on %q, want a new branch", branch)
	}
	files := strings.Fields(runIn(t, dir, "show", "--name-only", "--format=", "HEAD"))
	if !slices.Equal(files, []string{".github/workflows/ci.yml"}) {
		t.Errorf("the commit holds %v, want the changed workflow alone", files)
	}
	if status := runIn(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("the checkout is left dirty: %q", status)
	}
	remote := strings.TrimSpace(runIn(t, dir, "rev-parse", "origin/"+branch))
	if local := strings.TrimSpace(runIn(t, dir, "rev-parse", "HEAD")); remote != local {
		t.Errorf("origin holds %s and HEAD is %s", remote, local)
	}

	args, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("gh was never run: %v", err)
	}
	for _, want := range []string{"pr\ncreate\n", "--repo=Acme/API\n", "--base=main\n", "--head=alice:" + branch + "\n",
		"--body-file=-\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("gh's arguments do not hold %q:\n%s", want, args)
		}
	}
	body := readFile(t, record+".body")
	for _, want := range []string{`"our-own"`, "billet migrate workflows"} {
		if !strings.Contains(body, want) {
			t.Errorf("the pull request's body, read on stdin, does not hold %q:\n%s", want, body)
		}
	}
}

// --open-pr REFUSES BEFORE IT TOUCHES ANYTHING, and a failure past that point says
// where it stopped and leaves what was done, since it is the operator's to keep.
func TestMigrateWorkflowsOpenPRRefusesAndReportsWhereItStopped(t *testing.T) {
	stubGH(t, "exit 0")
	cfg := migrateConfigFile(t)
	head := func(dir string) string {
		return strings.TrimSpace(runIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	}

	dirty := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	if err := os.WriteFile(filepath.Join(dirty, "untracked"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrateRun(t, "--config", cfg, "--path", dirty, "--repo", "acme/api", "--open-pr"); err == nil ||
		!strings.Contains(err.Error(), "needs a clean checkout") {
		t.Errorf("a dirty checkout = %v", err)
	}
	if got := head(dirty); got != "main" {
		t.Errorf("a refusal left the checkout on %q", got)
	}

	// A DRAFT GIT WAS TOLD NOT TO SHOW is still uncommitted, and the plan reads it.
	hidden := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	runIn(t, hidden, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(hidden, ".github", "workflows", "draft.yml"), []byte(migrateWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrateRun(t, "--config", cfg, "--path", hidden, "--repo", "acme/api", "--open-pr"); err == nil ||
		!strings.Contains(err.Error(), "needs a clean checkout") {
		t.Errorf("an untracked draft git does not show = %v", err)
	}

	// A WORKFLOW GIT IGNORES would be written and then refused by git add.
	ignored := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	if err := os.WriteFile(filepath.Join(ignored, ".gitignore"), []byte(".github/workflows/local.yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, ignored, "add", ".gitignore")
	runIn(t, ignored, "commit", "-q", "-m", "ignore")
	if err := os.WriteFile(filepath.Join(ignored, ".github", "workflows", "local.yml"), []byte(migrateWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrateRun(t, "--config", cfg, "--path", ignored, "--repo", "acme/api", "--open-pr"); err == nil ||
		!strings.Contains(err.Error(), "git does not track every one of them; nothing was changed") {
		t.Errorf("an ignored workflow = %v", err)
	}
	if got := head(ignored); got != "main" {
		t.Errorf("a refusal over an ignored workflow left the checkout on %q", got)
	}
	if got := readFile(t, filepath.Join(ignored, ".github", "workflows", "ci.yml")); got != migrateWorkflow {
		t.Errorf("a refusal over an ignored workflow still changed ci.yml to %q", got)
	}

	// CONFIGURATION ONLY ON THE NEW BRANCH that pushes somewhere else is seen there,
	// before anything is written.
	onBranch := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	include := filepath.Join(t.TempDir(), "fork.gitconfig")
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n\tpushurl = https://github.com/bob/api.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, onBranch, "config", "includeIf.onbranch:billet/migrate-workflows-*.path", include)
	_, _, err := migrateRun(t, "--config", cfg, "--path", onBranch, "--repo", "acme/api", "--open-pr")
	if err == nil || !strings.Contains(err.Error(), "origin pushes to bob on this branch and to acme on main") {
		t.Errorf("a push URL only on the new branch = %v", err)
	}
	if got := readFile(t, filepath.Join(onBranch, ".github", "workflows", "ci.yml")); got != migrateWorkflow {
		t.Errorf("a push URL only on the new branch still changed ci.yml to %q", got)
	}

	// An origin pushing to two places has no one fork a head can name.
	twice := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	runIn(t, twice, "config", "--add", "remote.origin.pushurl", "https://github.com/alice/api.git")
	runIn(t, twice, "config", "--add", "remote.origin.pushurl", "https://github.com/bob/api.git")
	if _, _, err := migrateRun(t, "--config", cfg, "--path", twice, "--repo", "acme/api", "--open-pr"); err == nil ||
		!strings.Contains(err.Error(), "no one URL to push to; nothing was changed") {
		t.Errorf("two push URLs = %v, want refused before anything changed", err)
	}
	if got := head(twice); got != "main" {
		t.Errorf("a refusal over push URLs left the checkout on %q", got)
	}

	detached := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	runIn(t, detached, "checkout", "-q", "--detach")
	if _, _, err := migrateRun(t, "--config", cfg, "--path", detached, "--repo", "acme/api", "--open-pr"); err == nil ||
		!strings.Contains(err.Error(), "HEAD is detached") {
		t.Errorf("a detached HEAD = %v", err)
	}

	nothing := checkout(t, map[string]string{"ci.yml": "on: [push]\njobs:\n  a:\n    runs-on: billet-2vcpu\n"})
	if stdout, stderr, err := migrateRun(t, "--config", cfg, "--path", nothing, "--repo", "acme/api", "--open-pr"); err != nil ||
		stdout != "" || !strings.Contains(stderr, "nothing to change") {
		t.Errorf("nothing to change: %v, %q, %q", err, stdout, stderr)
	}
	if got := head(nothing); got != "main" {
		t.Errorf("nothing to change still made a branch: %q", got)
	}

	// The push fails: there is no such remote any more. The commit stands, and the
	// error says so.
	unpushed := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	runIn(t, unpushed, "config", "remote.origin.pushurl", "https://github.com/bob/api.git")
	runIn(t, unpushed, "config", "url."+filepath.Join(t.TempDir(), "gone")+".insteadOf", "https://github.com/bob/api.git")
	_, _, err = migrateRun(t, "--config", cfg, "--path", unpushed, "--repo", "acme/api", "--open-pr")
	if err == nil || !strings.Contains(err.Error(), "stopped on branch billet/migrate-workflows-") ||
		!strings.Contains(err.Error(), "after committing") {
		t.Errorf("a push that fails = %v", err)
	}
	if got := runIn(t, unpushed, "log", "--format=%s", "-1"); !strings.Contains(got, "Run workflows on billet's tiers") {
		t.Errorf("the commit is gone: %q", got)
	}
}

// --open-pr's gh IS THE OPERATOR'S, and without one nothing is changed.
func TestMigrateWorkflowsOpenPRNeedsGH(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	onlyGit := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(onlyGit, "git")); err != nil {
		t.Fatal(err)
	}
	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	cfg := migrateConfigFile(t)
	t.Setenv("PATH", onlyGit)

	_, _, err = migrateRun(t, "--config", cfg, "--path", dir, "--repo", "acme/api", "--open-pr")
	if err == nil || !strings.Contains(err.Error(), "none is on PATH; nothing was changed") {
		t.Errorf("no gh = %v", err)
	}
	if got := readFile(t, filepath.Join(dir, ".github", "workflows", "ci.yml")); got != migrateWorkflow {
		t.Errorf("no gh still changed the file to %q", got)
	}
}

// A PUSH THAT SUCCEEDS HAVING SENT NOTHING IS NO PUSH, as git's own settings can make
// one: the pull request is not opened for a branch the remote does not have.
func TestMigrateWorkflowsOpenPRWantsTheBranchSent(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	record := stubGH(t, "exit 0")
	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	cfg := migrateConfigFile(t)

	stub := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = push ]; then printf 'To elsewhere\\nDone\\n'; exit 0; fi\nexec '" + gitPath + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(stub, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, _, err = migrateRun(t, "--config", cfg, "--path", dir, "--repo", "acme/api", "--open-pr")
	if err == nil || !strings.Contains(err.Error(), "does not say it updated refs/heads/billet/migrate-workflows-") ||
		!strings.Contains(err.Error(), "after committing") {
		t.Errorf("a push that sent nothing = %v", err)
	}
	if _, err := os.Stat(record); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("gh was run for a branch never sent: %v", err)
	}
}

// EVERY STRING A WORKFLOW CHOSE IS QUOTED IN THE REPORT, A FILE'S NAME TOO, and a file
// that is no regular file is said rather than read: a FIFO named like a workflow
// would otherwise hang the command.
func TestTheMigrateReportQuotesWhatAWorkflowChoseAndSkipsWhatIsNoFile(t *testing.T) {
	t.Parallel()

	dir := checkout(t, map[string]string{"ci.yml": "on: [push]\njobs:\n  \"x\\nhosted (9):\":\n    runs-on: \"our\\nrewritten (1):\"\n"})
	workflows := filepath.Join(dir, ".github", "workflows")
	// A link to a workflow the command would move, so following it is seen.
	outside := filepath.Join(t.TempDir(), "elsewhere.yml")
	if err := os.WriteFile(outside, []byte(migrateWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workflows, "link.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workflows, "dir.yml"), 0o750); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := migrateRun(t, "--config", migrateConfigFile(t), "--path", dir, "--repo", "acme/api")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if stdout != "" {
		t.Errorf("a diff of nothing: %q", stdout)
	}
	if strings.Contains(stderr, "\nhosted (9):") || strings.Contains(stderr, "\nrewritten (1):") {
		t.Errorf("a job's name forged a line of the report:\n%s", stderr)
	}
	for _, want := range []string{`job "x\nhosted (9):"`, `"our\nrewritten (1):"`, `"` + workflowsDir + `/link.yml"`, `"` + workflowsDir + `/dir.yml"`,
		"a file that could not be read, left alone (2)", "link.yml\": it could not be read as a regular file"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the report does not say %q:\n%s", want, stderr)
		}
	}
}

// A PATH THAT WOULD END A DIFF'S HEADER IS QUOTED, as git quotes one, and one that
// would not is as it is.
func TestADiffQuotesAPathThatWouldBreakItsHeader(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]string{
		"a/.github/workflows/ci.yml": "a/.github/workflows/ci.yml",
		"a/x y.yml":                  `"a/x y.yml"`,
		"a/x\ny.yml":                 `"a/x\ny.yml"`,
		`a/x"y.yml`:                  `"a/x\"y.yml"`,
	} {
		if got := diffPath(path); got != want {
			t.Errorf("diffPath(%q) = %s, want %s", path, got, want)
		}
	}
}

// THE COMMAND IS ASKED FOR BY NAME, and --map takes what it says it does.
func TestMigrateAsksForASubcommand(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	for _, args := range [][]string{nil, {"everything"}} {
		if err := Migrate(t.Context(), env, args); err == nil || !strings.Contains(err.Error(), "usage: billet migrate workflows") {
			t.Errorf("billet migrate %v = %v, want its usage", args, err)
		}
	}
	if err := Migrate(t.Context(), env, []string{"workflows", "-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h = %v, want a request for help", err)
	}
}

// A PARSER'S POSITION IS CHECKED AGAINST THE BYTES BEFORE ANYTHING IS EDITED, for a
// plain value as for a quoted one: a position that lands on other bytes, or on a
// prefix of a longer word, is refused.
func TestAPlacementIsCheckedAgainstTheBytes(t *testing.T) {
	t.Parallel()

	lines := newLineIndex([]byte("a: ubuntu-latest\nb: ubuntu-latest-x\nc: ubuntu-lAtest\n"))
	for _, tc := range []struct {
		node yaml.Node
		says string
	}{
		{yaml.Node{Kind: yaml.ScalarNode, Value: "ubuntu-latest", Line: 1, Column: 4}, ""},
		{yaml.Node{Kind: yaml.ScalarNode, Value: "ubuntu-latest", Line: 1, Column: 3}, "does not hold the label"},
		{yaml.Node{Kind: yaml.ScalarNode, Value: "ubuntu-latest", Line: 2, Column: 4}, "does not hold the label"},
		{yaml.Node{Kind: yaml.ScalarNode, Value: "ubuntu-latest", Line: 3, Column: 4}, "does not hold the label"},
		{yaml.Node{Kind: yaml.ScalarNode, Value: "ubuntu-latest", Line: 5, Column: 1}, "not in the file"},
		{yaml.Node{Kind: yaml.ScalarNode, Value: "ubuntu-latest", Line: 1, Column: 40}, "not in the file"},
		{yaml.Node{Kind: yaml.ScalarNode, Value: "ubuntu-latest", Line: 1, Column: 4, Style: yaml.DoubleQuotedStyle},
			"does not hold the label"},
	} {
		got, reason := lines.span(&tc.node)
		if tc.says == "" && (reason != "" || got.start != 3 || got.end != 16) {
			t.Errorf("%+v placed at %+v (%s), want 3 to 16", tc.node, got, reason)
		}
		if tc.says != "" && !strings.Contains(reason, tc.says) {
			t.Errorf("%+v: %q, want it refused saying %q", tc.node, reason, tc.says)
		}
	}
}

// readFile is a file's contents, failing the test when it cannot be read.
func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}

// A MOVE A REPORT ASKS FOR IS CHECKED AGAINST THIS CONFIGURATION, and what is written
// is the configuration's own name for the tier: a stale or edited report cannot move
// a job to another OS, another architecture, a tier that will not take its workflow,
// a tier whose architecture cannot be told, or a look-alike of a real name.
func TestARecommendationIsCheckedAgainstTheConfiguration(t *testing.T) {
	t.Parallel()

	const file = ".github/workflows/ci.yml"
	before := "on: [push]\njobs:\n  build:\n    runs-on: " + onFour + "\n"
	tiers := func(change func(*config.Tier)) []config.Tier {
		out := migrateTiers()
		for i := range out {
			if out[i].Label == "platform-2vcpu" {
				change(&out[i])
			}
		}
		return out
	}
	entry := rightsizeEntry{Repository: "acme/api", Workflow: "acme/api/" + file, Job: "build", CurrentRunsOn: onFour,
		Recommendation: "smaller", Reason: "idle", Confidence: "high", RecommendedRunsOn: onTwo}.judgedAt(4, 2)

	for _, tc := range []struct {
		name   string
		tiers  []config.Tier
		entry  func(*rightsizeEntry)
		writes string
		says   string
	}{
		{name: "a compatible move writes the configuration's name", entry: func(e *rightsizeEntry) {
			e.RecommendedRunsOn = "BILLET-2VCPU-UBUNTU-2404"
		}, writes: onTwo},
		{name: "a lowercase k names the tier, and a Kelvin sign, which folds to it, does not",
			tiers: tiers(func(t *config.Tier) { t.RunsOn = "kube-2vcpu" }),
			entry: func(e *rightsizeEntry) { e.RecommendedRunsOn = "\u212aube-2vcpu" }, says: "which is no one tier"},
		{name: "the same tier by its ASCII name", tiers: tiers(func(t *config.Tier) { t.RunsOn = "kube-2vcpu" }),
			entry: func(e *rightsizeEntry) { e.RecommendedRunsOn = "KUBE-2vcpu" }, writes: "kube-2vcpu"},
		{name: "another guest OS on the same architecture", tiers: tiers(func(t *config.Tier) { t.GuestOS = config.GuestWindows }),
			says: "is not"},
		{name: "another architecture", tiers: tiers(func(t *config.Tier) { t.Image = "ubuntu-2404-arm64@verified" }),
			says: "is not"},
		{name: "the same image naming no architecture, which may run on either", tiers: func() []config.Tier {
			out := tiers(func(t *config.Tier) { t.Image = "ubuntu-2404@verified" })
			for i := range out {
				if out[i].Label == "platform-4vcpu" {
					out[i].Image = "ubuntu-2404@verified"
				}
			}
			return out
		}(), says: "the architecture of either cannot be told"},
		{name: "a recommended tier given fewer vCPUs since the report", tiers: tiers(func(t *config.Tier) { t.VCPU = 1 }),
			says: `tier "platform-2vcpu" is 1 vCPU and 8.0 GiB now, and the report judged it at 2 vCPU and 8.0 GiB`},
		{name: "a recommended tier given less memory since the report",
			tiers: tiers(func(t *config.Tier) { t.Memory = 4 * config.GiB }),
			says:  `tier "platform-2vcpu" is 2 vCPU and 4.0 GiB now, and the report judged it at 2 vCPU and 8.0 GiB`},
		// A CURRENT TIER GROWN PAST THE ONE RECOMMENDED turns the move into a move down.
		{name: "a current tier grown since the report", tiers: func() []config.Tier {
			out := migrateTiers()
			for i := range out {
				if out[i].Label == "platform-4vcpu" {
					out[i].VCPU, out[i].Memory = 16, 64*config.GiB
				}
			}
			return out
		}(), says: `tier "platform-4vcpu" is 16 vCPU and 64.0 GiB now, and the report judged it at 4 vCPU and 16.0 GiB`},
		{name: "a current tier given more memory since the report", tiers: func() []config.Tier {
			out := migrateTiers()
			for i := range out {
				if out[i].Label == "platform-4vcpu" {
					out[i].Memory = 64 * config.GiB
				}
			}
			return out
		}(), says: `tier "platform-4vcpu" is 4 vCPU and 64.0 GiB now`},
		{name: "a report without the recommended tier's shape", entry: func(e *rightsizeEntry) { e.RecommendedMemory = nil },
			says: `the report does not record the shape it judged tier "platform-2vcpu" at`},
		{name: "a report without the current tier's shape", entry: func(e *rightsizeEntry) { e.CurrentTierVCPU = nil },
			says: `the report does not record the shape it judged tier "platform-4vcpu" at`},
		{name: "a recommended tier whose image changed since the report", tiers: tiers(func(t *config.Tier) {
			t.Image = "ubuntu-2404-x64@g2"
		}), says: "is not"},
		{name: "a recommended tier with less disk since the report", tiers: func() []config.Tier {
			out := migrateTiers()
			for i := range out {
				switch out[i].Label {
				case "platform-4vcpu":
					out[i].Disk = 40 * config.GiB
				case "platform-2vcpu":
					out[i].Disk = 20 * config.GiB
				}
			}
			return out
		}(), says: "less disk"},
		{name: "a trusted tier listing the workflow under another case", tiers: func() []config.Tier {
			out := migrateTiers()
			for i := range out {
				switch out[i].Label {
				case "platform-4vcpu":
					out[i].Trust, out[i].Workflows = config.WorkloadTrusted, []string{"acme/api/" + file + "@refs/heads/main"}
				case "platform-2vcpu":
					out[i].Trust, out[i].Workflows = config.WorkloadTrusted, []string{"acme/api/.github/workflows/CI.yml@refs/heads/main"}
				}
			}
			return out
		}(), says: "is not"},
		{name: "a trusted tier listing it, the repository in another case", tiers: func() []config.Tier {
			out := migrateTiers()
			for i := range out {
				if out[i].Label == "platform-4vcpu" || out[i].Label == "platform-2vcpu" {
					out[i].Trust, out[i].Workflows = config.WorkloadTrusted, []string{"ACME/Api/" + file + "@refs/heads/main"}
				}
			}
			return out
		}(), writes: onTwo},
		{name: "the report names no job", entry: func(e *rightsizeEntry) { e.Job = "" }, says: "the report names no job"},
		{name: "judged on a tier that is not here", entry: func(e *rightsizeEntry) { e.CurrentRunsOn = "gone" },
			says: `it was judged on "gone"`},
	} {
		tierSet := tc.tiers
		if tierSet == nil {
			tierSet = migrateTiers()
		}
		e := entry
		if tc.entry != nil {
			tc.entry(&e)
		}
		entries := &rightsizeEntries{byFile: map[string][]rightsizeEntry{file: {e}}}
		got := rewriteWorkflow(file, []byte(before), chooserOf(t, tierSet, labelMaps{}, false), entries)
		want := before
		if tc.writes != "" {
			want = strings.Replace(before, onFour, tc.writes, 1)
		}
		if string(got.after) != want {
			t.Errorf("%s: became %q, want %q", tc.name, got.after, want)
		}
		if report := (&migrationPlan{findings: got.findings}).reportText(); tc.says != "" && !strings.Contains(report, tc.says) {
			t.Errorf("%s: the report does not say %q:\n%s", tc.name, tc.says, report)
		}
	}

	// A job whose name only GitHub can work out is matched by no entry, empty or not.
	computed := "on: [push]\njobs:\n  build:\n    name: ${{ github.ref }}\n    runs-on: " + onFour + "\n"
	e := entry
	e.Job = ""
	got := rewriteWorkflow(file, []byte(computed), chooserOf(t, migrateTiers(), labelMaps{}, false),
		&rightsizeEntries{byFile: map[string][]rightsizeEntry{file: {e}}})
	if string(got.after) != computed {
		t.Errorf("an empty entry moved a job named by an expression:\n%s", got.after)
	}
}

// A JOB SHARED THROUGH AN ANCHOR, AN ALIAS OF ONE, AND ONE TAKING ITS KEYS WITH <<
// ARE LEFT ALONE AND SAID: an edit to one would move the others.
func TestASharedJobIsLeftAlone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, data, says string }{
		{"an anchored job", "jobs:\n  a: &shared\n    runs-on: ubuntu-latest\n  b: *shared\n", "the job is a YAML anchor"},
		{"a merged job", "x: &defaults\n  runs-on: ubuntu-latest\njobs:\n  a:\n    <<: *defaults\n    steps: []\n",
			"takes keys from another mapping"},
		{"anchored jobs", "jobs: &all\n  a:\n    runs-on: ubuntu-latest\n", "its jobs are a YAML anchor"},
	} {
		got := rewriteWorkflow("w.yml", []byte(tc.data), chooserOf(t, migrateTiers(), labelMaps{}, false), nil)
		report := (&migrationPlan{findings: got.findings}).reportText()
		if string(got.after) != tc.data || !strings.Contains(report, tc.says) {
			t.Errorf("%s: became %q, saying\n%s\nwant it left alone saying %q", tc.name, got.after, report, tc.says)
		}
	}

	aliased := "jobs:\n  a: &shared\n    runs-on: ubuntu-latest\n  b: *shared\n"
	got := rewriteWorkflow("w.yml", []byte(aliased), chooserOf(t, migrateTiers(), labelMaps{}, false), nil)
	if report := (&migrationPlan{findings: got.findings}).reportText(); !strings.Contains(report, `job "b": the job is a YAML alias`) {
		t.Errorf("the alias job was not said:\n%s", report)
	}
}

// A TIER WHOSE NAME WOULD READ BACK AS ANOTHER TYPE IS WRITTEN QUOTED, and one already
// quoted keeps its quotes: runs-on must still be the string after the edit.
func TestAReplacementReadsBackAsTheLabel(t *testing.T) {
	t.Parallel()

	tiers := migrateTiers()
	for i := range tiers {
		if tiers[i].Label == "platform-2vcpu" {
			tiers[i].RunsOn = "null"
		}
	}
	for _, tc := range []struct{ data, want string }{
		{"jobs:\n  a:\n    runs-on: ubuntu-latest\n", "jobs:\n  a:\n    runs-on: \"null\"\n"},
		{"jobs:\n  a:\n    runs-on: [ubuntu-latest]\n", "jobs:\n  a:\n    runs-on: [\"null\"]\n"},
		{"jobs:\n  a:\n    runs-on: 'ubuntu-latest'\n", "jobs:\n  a:\n    runs-on: 'null'\n"},
	} {
		got := rewriteWorkflow("w.yml", []byte(tc.data), chooserOf(t, tiers, labelMaps{}, false), nil)
		if string(got.after) != tc.want {
			t.Errorf("%q became %q, want %q", tc.data, got.after, tc.want)
		}
	}
	for value, bare := range map[string]bool{"billet-2vcpu": true, "null": false, "true": false, "1": false, "~": false,
		"a: b": false, "#x": false, "[x]": false, "yes": true} {
		if got := readsBackAs(value); got != bare {
			t.Errorf("readsBackAs(%q) = %t, want %t", value, got, bare)
		}
	}
}

// LINES ARE COUNTED AS THE PARSER COUNTS THEM: a lone CR, a NEL, an LS or a PS above
// the label moves every later line, and an edit placed by a count of LFs alone would
// land on another line. Whatever the count, the result is read back before it is
// kept, and a file that would not read back as itself with only the label changed is
// left alone.
func TestLinesAreCountedAsTheParserCountsThem(t *testing.T) {
	t.Parallel()

	for name, sep := range map[string]string{"CR": "\r", "NEL": "\u0085", "LS": "\u2028", "PS": "\u2029"} {
		data := "# a" + sep + "# b\njobs:\n  a:\n    runs-on: ubuntu-latest\n  b:\n    runs-on: ubuntu-latest\n"
		got := rewriteWorkflow("w.yml", []byte(data), chooserOf(t, migrateTiers(), labelMaps{}, false), nil)
		want := strings.ReplaceAll(data, "ubuntu-latest", onTwo)
		if string(got.after) != want {
			t.Errorf("%s above the jobs: became %q, want %q", name, got.after, want)
		}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("jobs:\n  a:\n    runs-on: x\n    name: y\n"), &doc); err != nil {
		t.Fatal(err)
	}
	runsOn := mappingValue(mappingValue(mappingValue(rootOf(&doc), "jobs"), "a"), "runs-on")
	for _, tc := range []struct {
		after, says string
	}{
		{"jobs:\n  a:\n    runs-on: z\n    name: y\n", ""},
		{"jobs:\n  a:\n    runs-on: z\n    name: w\n", "a value that was not meant to change did"},
		{"jobs:\n  a:\n    runs-on: w\n    name: y\n", "a value did not become the label meant"},
		{"jobs:\n  a:\n    runs-on: [z]\n    name: y\n", "its structure changed"},
		{"jobs: [\n", "it no longer parses"},
	} {
		if got := sameWorkflowBut(&doc, []byte(tc.after), []edit{{node: runsOn, value: "z"}}); got != tc.says {
			t.Errorf("%q: %q, want %q", tc.after, got, tc.says)
		}
	}
}

// NEITHER WORKFLOW DIRECTORY MAY BE A LINK, and a file changed after it was read is
// not written over: either would have the command change what it did not read.
func TestMigrateWorkflowsWritesOnlyWhatItRead(t *testing.T) {
	t.Parallel()

	cfg := migrateConfigFile(t)
	elsewhere := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	for _, link := range []string{".github", ".github/workflows"} {
		dir := t.TempDir()
		target := filepath.Join(elsewhere, filepath.FromSlash(link))
		at := filepath.Join(dir, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(at), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
		_, _, err := migrateRun(t, "--config", cfg, "--path", dir, "--repo", "acme/api", "--write")
		if err == nil || !strings.Contains(err.Error(), "is a link or a file, not a directory") {
			t.Errorf("a linked %s = %v, want refused", link, err)
		}
	}
	if got := readFile(t, filepath.Join(elsewhere, ".github", "workflows", "ci.yml")); got != migrateWorkflow {
		t.Errorf("a link wrote another checkout's workflow: %q", got)
	}

	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	plan, err := planMigration(dir, "acme/api", chooserOf(t, []config.Tier{{Label: "billet-2vcpu", Target: "default",
		Arch: config.ArchX64, Provider: config.ProviderDocker, GuestOS: config.GuestLinux, VCPU: 2, Memory: 8 * config.GiB,
		Image: "ubuntu:24.04"}}, labelMaps{}, false), nil)
	if err != nil || !plan.changes() {
		t.Fatalf("plan = %+v, %v; want a change", plan, err)
	}
	target := filepath.Join(dir, ".github", "workflows", "ci.yml")
	if err := os.WriteFile(target, []byte(migrateWorkflow+"# edited meanwhile\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := plan.install(dir); err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Errorf("installing over an edit = %v, want refused", err)
	}
	if got := readFile(t, target); !strings.HasSuffix(got, "# edited meanwhile\n") {
		t.Errorf("the edit was written over: %q", got)
	}
}

// A TOOL'S MESSAGE LOSES EVERY URL'S CREDENTIALS before it reaches an error.
func TestAToolsMessageLosesItsCredentials(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"fatal: unable to access 'https://x-access-token:s3cret@github.com/a/b.git/': 403": "fatal: unable to access 'https://github.com/a/b.git/': 403",
		"push to ssh://git:pw@host/a/b failed":                                             "push to ssh://host/a/b failed",
		"nothing to hide at https://github.com/a/b":                                        "nothing to hide at https://github.com/a/b",
	} {
		if got := withoutCredentials(in); got != want {
			t.Errorf("withoutCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}

// A JOB WHOSE STRATEGY CANNOT BE READ MAY HAVE A MATRIX, so a recommendation for it
// is never applied to the whole job.
func TestARecommendationLeavesAJobWhoseStrategyCannotBeRead(t *testing.T) {
	t.Parallel()

	const file = ".github/workflows/ci.yml"
	data := "x: &legs\n  matrix:\n    n: [1, 2]\njobs:\n  build:\n    strategy: *legs\n    runs-on: " + onFour + "\n"
	entry := rightsizeEntry{Repository: "acme/api", Workflow: "acme/api/" + file, Job: "build", CurrentRunsOn: onFour,
		Recommendation: "smaller", RecommendedRunsOn: onTwo}.judgedAt(4, 2)
	got := rewriteWorkflow(file, []byte(data), chooserOf(t, migrateTiers(), labelMaps{}, false),
		&rightsizeEntries{byFile: map[string][]rightsizeEntry{file: {entry}}})
	report := (&migrationPlan{findings: got.findings}).reportText()
	if string(got.after) != data || !strings.Contains(report, "the job has a matrix") {
		t.Errorf("became %q, saying\n%s\nwant it left alone as a job with a matrix", got.after, report)
	}
}

// AN EDIT THAT DOES NOT READ BACK AS MEANT IS NOT KEPT: the file is left as it was,
// and each rewrite in it is said not to have happened.
func TestAnEditThatDoesNotReadBackIsNotKept(t *testing.T) {
	t.Parallel()

	data := []byte("jobs:\n  a:\n    runs-on: ubuntu-latest\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	runsOn := mappingValue(mappingValue(mappingValue(rootOf(&doc), "jobs"), "a"), "runs-on")
	span, reason := newLineIndex(data).span(runsOn)
	if reason != "" {
		t.Fatal(reason)
	}
	for _, tc := range []struct {
		text string
		kept bool
	}{{"meant", true}, {"something-else", false}} {
		r := workflowRewrite{before: data, edited: map[*yaml.Node]bool{},
			edits:    []edit{{start: span.start, end: span.end, text: tc.text, node: runsOn, value: "meant"}},
			findings: []finding{{Kind: findRewritten, File: "w.yml", Job: "a"}}}
		r.after = applyEdits(data, r.edits)
		r.keepOnlyIfSame(&doc)
		if kept := !bytes.Equal(r.after, data); kept != tc.kept {
			t.Errorf("writing %q: kept %t, want %t", tc.text, kept, tc.kept)
		}
		if !tc.kept && (r.findings[0].Kind != findCouldNot || !strings.Contains(r.findings[0].Detail, "did not read back")) {
			t.Errorf("writing %q: the rewrite is still said to have happened: %+v", tc.text, r.findings[0])
		}
	}
}

// THE READ-BACK IS ON THE PATH EVERY EDIT TAKES, after the edits are applied: no input
// reaches it through a correct placement, so the call is held here, where a test of
// the read-back alone cannot see it.
func TestEveryRewriteIsReadBackBeforeItIsKept(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "migrateworkflow.go", nil, 0)
	if err != nil {
		t.Fatalf("parse migrateworkflow.go: %v", err)
	}
	var applied, kept token.Pos
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "rewriteWorkflow" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				if f.Name == "applyEdits" {
					applied = call.Pos()
				}
			case *ast.SelectorExpr:
				if f.Sel.Name == "keepOnlyIfSame" {
					kept = call.Pos()
				}
			}
			return true
		})
	}
	if !applied.IsValid() || !kept.IsValid() || kept < applied {
		t.Errorf("rewriteWorkflow applies its edits at %s and reads them back at %s; want a read-back after",
			fset.Position(applied), fset.Position(kept))
	}
}

// originOf is the bare repository a checkout() pushes to, from the rule it wrote.
func originOf(t *testing.T, dir string) string {
	t.Helper()

	out := runIn(t, dir, "config", "--get-regexp", `^url\..*\.pushinsteadof$`)
	key, _, _ := strings.Cut(strings.TrimSpace(out), " ")

	return strings.TrimSuffix(strings.TrimPrefix(key, "url."), ".pushinsteadof")
}

// A WORKFLOWS DIRECTORY REPLACED BETWEEN PLANNING AND WRITING IS NOT WRITTEN THROUGH,
// even to files whose bytes are the ones planned.
func TestMigrateWorkflowsDoesNotWriteThroughAReplacedDirectory(t *testing.T) {
	t.Parallel()

	tier := []config.Tier{{Label: "billet-2vcpu", Target: "default", Arch: config.ArchX64, Provider: config.ProviderDocker,
		GuestOS: config.GuestLinux, VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04"}}
	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	elsewhere := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	plan, err := planMigration(dir, "acme/api", chooserOf(t, tier, labelMaps{}, false), nil)
	if err != nil || !plan.changes() {
		t.Fatalf("plan = %+v, %v; want a change", plan, err)
	}

	workflows := filepath.Join(dir, ".github", "workflows")
	if err := os.Rename(workflows, workflows+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, ".github", "workflows"), workflows); err != nil {
		t.Fatal(err)
	}
	if err := plan.install(dir); err == nil || !strings.Contains(err.Error(), "is a link or a file") {
		t.Errorf("installing through a replaced directory = %v, want refused", err)
	}
	if got := readFile(t, filepath.Join(elsewhere, ".github", "workflows", "ci.yml")); got != migrateWorkflow {
		t.Errorf("another checkout's workflow was written: %q", got)
	}

	// A directory replaced by another directory is refused too.
	if err := os.Remove(workflows); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workflows, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflows, "ci.yml"), []byte(migrateWorkflow), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := plan.install(dir); err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Errorf("installing into another directory = %v, want refused", err)
	}
}

// THE HEAD IS THE OWNER GIT WILL PUSH TO, after every rewrite rule: a rule that names
// another repository is followed, a rule to a path on this machine falls back to the
// URL configured, and a destination naming no owner is refused.
func TestThePushOwnerIsWhereGitPushes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(dir string)
		want  string
		fails bool
	}{
		{"the configured URL", func(string) {}, "acme", false},
		{"a push URL", func(dir string) {
			runIn(t, dir, "config", "remote.origin.pushurl", "git@github.com:carol/api.git")
		}, "carol", false},
		{"a rewrite naming another repository", func(dir string) {
			runIn(t, dir, "config", "--unset-all", "url."+originOf(t, dir)+".pushInsteadOf")
			runIn(t, dir, "config", "url.https://github.com/dave/.pushInsteadOf", "https://github.com/acme/")
		}, "dave", false},
		{"a rewrite to an alias naming no owner", func(dir string) {
			runIn(t, dir, "config", "--unset-all", "url."+originOf(t, dir)+".pushInsteadOf")
			runIn(t, dir, "config", "url.gh-alias:.pushInsteadOf", "https://github.com/acme/")
		}, "", true},
	} {
		dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
		tc.setup(dir)
		got, err := pushOwner(t.Context(), dir)
		if (err != nil) != tc.fails || got != tc.want {
			t.Errorf("%s: %q, %v; want %q (fails %t)", tc.name, got, err, tc.want, tc.fails)
		}
	}
}

// judgedAt is the entry with the shapes a report records for its current and
// recommended tiers, each of this file's test tiers being 4 GiB to the vCPU.
func (e rightsizeEntry) judgedAt(currentVCPU, recommendedVCPU int64) rightsizeEntry {
	shape := func(v int64) *int64 { return &v }
	e.CurrentTierVCPU, e.CurrentTierMemory = shape(currentVCPU), shape(currentVCPU*int64(4*config.GiB))
	e.RecommendedVCPU, e.RecommendedMemory = shape(recommendedVCPU), shape(recommendedVCPU*int64(4*config.GiB))

	return e
}

// GIT'S OUTPUT IS ONE VALUE TO A LINE, so a path holding a space is one destination.
func TestAPushToAPathWithASpaceFallsBackToTheConfiguredURL(t *testing.T) {
	t.Parallel()

	dir := checkout(t, map[string]string{"ci.yml": migrateWorkflow})
	runIn(t, dir, "config", "--unset-all", "url."+originOf(t, dir)+".pushInsteadOf")
	runIn(t, dir, "config", "url./srv/Git Mirrors/.pushInsteadOf", "https://github.com/acme/")
	if got, err := pushOwner(t.Context(), dir); err != nil || got != "acme" {
		t.Errorf("a push to a path with a space: %q, %v; want acme", got, err)
	}
	runIn(t, dir, "config", "--unset-all", "url./srv/Git Mirrors/.pushInsteadOf")
	runIn(t, dir, "config", "url.Git Mirrors/.pushInsteadOf", "https://github.com/acme/")
	if got, err := pushOwner(t.Context(), dir); err != nil || got != "acme" {
		t.Errorf("a push to a relative path: %q, %v; want acme", got, err)
	}

	for remote, want := range map[string]bool{
		"/srv/api.git": true, "Git Mirrors/api.git": true, "api": true, "./a:b/api.git": true, "Mirrors/odd://api.git": true,
		"file:///srv/api.git": true, "https://github.com/acme/api.git": false, "ssh://git@github.com/acme/api.git": false,
		"git@github.com:acme/api.git": false, "gh-alias:api.git": false,
	} {
		if got := localPath(remote); got != want {
			t.Errorf("localPath(%q) = %t, want %t", remote, got, want)
		}
	}
	if got := gitLines("/srv/Git Mirrors/api.git\r\n\nhttps://github.com/acme/api.git"); len(got) != 2 ||
		got[0] != "/srv/Git Mirrors/api.git" {
		t.Errorf("gitLines: %q", got)
	}
}

// A PUSH IS PROVED BY WHAT GIT SAYS IT SENT: the refspec on a line of its own with a
// flag that sent it, not a rejection, another ref or the summary.
func TestAPushIsProvedByWhatGitSaysItSent(t *testing.T) {
	t.Parallel()

	const spec = "refs/heads/b:refs/heads/b"
	for porcelain, want := range map[string]bool{
		"To /srv/api.git\n*\t" + spec + "\t[new branch]\nDone\n":            true,
		"To /srv/api.git\n \t" + spec + "\tabc..def\nDone\n":                true,
		"To /srv/api.git\n+\t" + spec + "\tabc...def (forced update)\n":     true,
		"=\t" + spec + "\t[up to date]\n":                                   true,
		"To /srv/api.git\n!\t" + spec + "\t[rejected] (non-fast-forward)\n": false,
		"*\trefs/heads/main:refs/heads/main\t[new branch]\n":                false,
		"*\trefs/heads/b:refs/heads/main\t[new branch]\n":                   false,
		"*\trefs/heads/b:refs/heads/bb\t[new branch]\n":                     false,
		"!*\t" + spec + "\t[new branch]\n":                                  false,
		"To /srv/api.git\nDone\n":                                           false,
		"":                                                                  false,
	} {
		if got := pushedRef(porcelain, spec); got != want {
			t.Errorf("pushedRef(%q) = %t, want %t", porcelain, got, want)
		}
	}
}
