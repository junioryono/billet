package config

import (
	"slices"
	"strings"
	"testing"
)

// archTiers names an architecture on validConfig's two tiers.
func archTiers(first, second string) string {
	cfg := validConfig
	if first != "" {
		cfg = strings.Replace(cfg, "    image: ubuntu-2404-x64\n  - label: billet-8vcpu",
			"    image: ubuntu-2404-x64\n    arch: "+first+"\n  - label: billet-8vcpu", 1)
	}

	if second != "" {
		cfg = strings.Replace(cfg, "    shm: 1GiB\n", "    shm: 1GiB\n    arch: "+second+"\n", 1)
	}

	return cfg
}

// ARCH IS NAMED ON EVERY TIER OR ON NONE, so the first `arch` is a migration of the
// whole catalogue rather than a default an existing tier silently takes.
func TestATierArchIsNamedOnEveryTierOrOnNone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		first, second string
		wantErr       string
	}{
		{name: "none", first: "", second: ""},
		{name: "every tier", first: "x64", second: "arm64"},
		{name: "one of two", first: "x64", second: "",
			wantErr: "arch is named on 1 of 2 tiers; once one tier names an architecture every tier must"},
		{name: "a spelling billet does not use", first: "amd64", second: "x64",
			wantErr: `arch "amd64" is not one of [x64 arm64]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(writeConfig(t, archTiers(tc.first, tc.second)))

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("answered %v, want an error saying %q", err, tc.wantErr)
			case tc.wantErr == "":
				tier, _ := cfg.TierByLabel("billet-4vcpu-ubuntu-2404")
				if string(tier.Arch) != tc.first {
					t.Errorf("arch = %q, want %q", tier.Arch, tc.first)
				}
			}
		})
	}
}

// A macOS TIER IS arm64: every macOS guest billet launches runs on Apple silicon.
func TestAMacOSTierIsArm64(t *testing.T) {
	t.Parallel()

	for arch, wantErr := range map[Arch]bool{ArchARM64: false, ArchX64: true} {
		errs := TierArchErrors([]Tier{{Label: "mac", GuestOS: GuestMacOS, Arch: arch}})
		if (len(errs) > 0) != wantErr {
			t.Errorf("a macOS tier of arch %s answered %v", arch, errs)
		}
	}
}

// AN ARCHITECTURE IS AN EC2 SHAPE'S: a CodeBuild compute type's environment type
// decides its own, and a spelling billet does not use is refused on either.
func TestOnlyAnEC2ShapeNamesAnArchitecture(t *testing.T) {
	t.Parallel()

	shape := func(arch Arch) []RemoteShape {
		return []RemoteShape{{Type: "c7g.4xlarge", VCPU: 16, Memory: 32 * GiB, Arch: arch}}
	}

	for _, tc := range []struct {
		provider ProviderKind
		arch     Arch
		wantErr  string
	}{
		{provider: ProviderEC2, arch: ""},
		{provider: ProviderEC2, arch: ArchARM64},
		{provider: ProviderEC2, arch: "aarch64", wantErr: `arch "aarch64" is not one of`},
		{provider: ProviderCodeBuild, arch: ""},
		{provider: ProviderCodeBuild, arch: ArchARM64, wantErr: "arch is an EC2 shape's"},
	} {
		errs := CheckRemoteShapes(tc.provider, shape(tc.arch))

		got := ""
		if len(errs) > 0 {
			got = errs[0].Error()
		}

		if (tc.wantErr == "") != (got == "") || !strings.Contains(got, tc.wantErr) {
			t.Errorf("%s shape of arch %q answered %v, want %q", tc.provider, tc.arch, errs, tc.wantErr)
		}
	}
}

// THE TRANSLATIONS ARE THE ONE PLACE EACH SPELLING BECOMES BILLET'S.
func TestArchitecturesAreTranslatedFromEachSpelling(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]Arch{"amd64": ArchX64, "arm64": ArchARM64, "386": "", "riscv64": ""} {
		if got, ok := HostArch(in); got != want || ok != (want != "") {
			t.Errorf("HostArch(%q) = %q, %v", in, got, ok)
		}
	}

	for in, want := range map[string]Arch{"x86_64": ArchX64, "arm64": ArchARM64, "i386": "", "arm64_mac": ""} {
		if got, ok := ArchFromEC2(in); got != want || ok != (want != "") {
			t.Errorf("ArchFromEC2(%q) = %q, %v", in, got, ok)
		}
	}

	for env, want := range map[CodeBuildEnvironment]Arch{
		CodeBuildLinuxContainer: ArchX64, CodeBuildARMContainer: ArchARM64, CodeBuildLinuxEC2: ArchX64,
		CodeBuildARMEC2: ArchARM64, CodeBuildMacARM: ArchARM64, CodeBuildLinuxGPUContainer: ArchX64,
	} {
		if got := env.Arch(); got != want {
			t.Errorf("%s.Arch() = %q, want %q", env, got, want)
		}
	}
}

// THE LEDGER'S LIST IS ONE SPELLING OF A SET, and a name nothing wrote is refused
// rather than dropped.
func TestTheArchitectureListIsOneSpellingOfASet(t *testing.T) {
	t.Parallel()

	if got := JoinArchs([]Arch{ArchARM64, ArchX64, ArchARM64}); got != "arm64,x64" {
		t.Errorf("JoinArchs = %q, want arm64,x64", got)
	}

	got, err := SplitArchs("x64,arm64,x64")
	if err != nil || !slices.Equal(got, []Arch{ArchARM64, ArchX64}) {
		t.Errorf("SplitArchs = %v, %v", got, err)
	}

	if got, err := SplitArchs(""); err != nil || got != nil {
		t.Errorf("SplitArchs of nothing = %v, %v", got, err)
	}

	if _, err := SplitArchs("x64,sparc"); err == nil {
		t.Error("SplitArchs accepted an architecture nothing wrote")
	}
}
