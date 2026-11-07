package config

import "testing"

// WHAT A TIER'S CONFIGURATION FIXES IS READ, AND WHAT IT DOES NOT IS UNKNOWN: an
// architecture a reader cannot tell must not become one it guesses, since the tier
// it chooses from this runs somebody's job.
func TestATiersPlatformIsOnlyWhatItsConfigurationFixes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		tier    Tier
		arch    Arch
		version string
	}{
		{"the image names both", Tier{Provider: ProviderFirecracker, Image: "ubuntu-2404-x64@verified"}, ArchX64, "24.04"},
		{"dotted, under a registry", Tier{Provider: ProviderDocker, Image: "ghcr.io/acme/ubuntu:24.04-amd64"}, ArchX64, "24.04"},
		{"arm on Firecracker", Tier{Provider: ProviderFirecracker, Image: "ubuntu-2204-arm64@g1"}, ArchARM64, "22.04"},
		{"aarch64 is arm64", Tier{Provider: ProviderDocker, Image: "acme/runner:aarch64"}, ArchARM64, ""},
		{"x86_64 is x64", Tier{Provider: ProviderDocker, Image: "acme/runner:x86_64"}, ArchX64, ""},
		{"a word inside another is not a name", Tier{Provider: ProviderDocker, Image: "acme/linux-x640:latest"}, "", ""},
		{"a floating image names nothing", Tier{Provider: ProviderDocker, Image: "ghcr.io/actions/actions-runner:latest"}, "", ""},
		{"an AMI names nothing", Tier{Provider: ProviderEC2, Image: "ami-0123456789"}, "", ""},
		{"an image naming both is unknown", Tier{Provider: ProviderDocker, Image: "acme/x64-to-arm64:1"}, "", ""},
		{"the tier's own word first", Tier{Arch: ArchARM64, Provider: ProviderDocker, Image: "acme/runner-x64:1"}, ArchARM64, ""},
		{"tart alone is Apple silicon", Tier{Provider: ProviderTart, Image: "ghcr.io/cirruslabs/ubuntu-runner:latest"}, ArchARM64, ""},
		{"macOS is Apple silicon", Tier{GuestOS: GuestMacOS, Provider: ProviderCodeBuild, Image: "aws/codebuild/macos-arm-base:15"}, ArchARM64, ""},
		{"tart's macOS by its number", Tier{GuestOS: GuestMacOS, Provider: ProviderTart, Image: "ghcr.io/cirruslabs/macos-15:latest"}, ArchARM64, "15"},
		{"tart's macOS by its name", Tier{GuestOS: GuestMacOS, Provider: ProviderTart, Image: "ghcr.io/cirruslabs/macos-tahoe-xcode:26.5"}, ArchARM64, "26"},
		{"another name", Tier{GuestOS: GuestMacOS, Provider: ProviderTart, Image: "ghcr.io/cirruslabs/macos-golden-gate-xcode:27"}, ArchARM64, "27"},
		{"two names is no version", Tier{GuestOS: GuestMacOS, Provider: ProviderTart, Image: "acme/macos-sonoma-to-sequoia:1"}, ArchARM64, ""},
		{"an Ubuntu word on macOS is no version", Tier{GuestOS: GuestMacOS, Provider: ProviderTart, Image: "acme/ubuntu-2404:1"}, ArchARM64, ""},
		{"every image of a fallback agrees", Tier{Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
			Launch: map[ProviderKind]TierLaunch{ProviderFirecracker: {Image: "ubuntu-2404-x64@g1"},
				ProviderEC2: {Image: "billet-ubuntu-2404-x64-1"}}}, ArchX64, "24.04"},
		{"a fallback whose images disagree cannot promise", Tier{Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
			Launch: map[ProviderKind]TierLaunch{ProviderFirecracker: {Image: "ubuntu-2404-x64@g1"},
				ProviderEC2: {Image: "billet-ubuntu-2204-arm64-1"}}}, "", ""},
		{"a fallback with one silent image cannot promise", Tier{Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
			Launch: map[ProviderKind]TierLaunch{ProviderFirecracker: {Image: "ubuntu-2404-x64@g1"},
				ProviderEC2: {Image: "ami-0123456789"}}}, "", ""},
		{"tart beside another backend is not Apple silicon alone", Tier{Providers: []ProviderKind{ProviderTart, ProviderDocker},
			Image: "acme/runner:latest"}, "", ""},
		{"no provider at all", Tier{Image: "acme/runner:latest"}, "", ""},
	} {
		got := tc.tier.Platform()
		if got.Arch != tc.arch || got.OSVersion != tc.version {
			t.Errorf("%s: arch %q, version %q; want %q, %q", tc.name, got.Arch, got.OSVersion, tc.arch, tc.version)
		}
	}
}
