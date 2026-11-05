package setup

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// hostedLabel is what a GitHub-hosted label asks for. Version is the operating
// system's, empty for a label that follows the newest (`ubuntu-latest`).
type hostedLabel struct {
	OS      config.GuestOS
	Arch    config.Arch
	Version string
	// VCPU and MemoryGB are the shape a job on the label has, for a private
	// repository unless Public asks for what a public one gets.
	VCPU     int
	MemoryGB int64
	// NoMapping says why this label is never chosen for automatically, and names
	// what the operator can do: --map remains.
	NoMapping string
}

// The labels GitHub documents as read from its hosted-runner reference on
// 2026-11-03: https://docs.github.com/en/actions/reference/runners/github-hosted-runners
// A standard Linux runner has 2 vCPU and 8 GB in a private repository and 4 and 16 in
// a public one, and the rest are the same in both. Parsed by form rather than listed,
// because GitHub adds a version a release at a time and a label one version newer is
// the same shape; a label whose form this does not know is reported, never guessed.
var (
	hostedUbuntu = regexp.MustCompile(`^ubuntu-(\d{2}\.\d{2})(-arm)?$`)
	hostedMacOS  = regexp.MustCompile(`^macos-(\d{2})(-intel|-large|-xlarge)?$`)
)

// lookupHostedLabel reads a label GitHub hosts. GitHub compares labels without regard
// to case, so a workflow's spelling is not part of the answer.
func lookupHostedLabel(label string, public bool) (hostedLabel, bool) {
	lower := strings.ToLower(label)

	linuxVCPU, linuxMemory := 2, int64(8)
	if public {
		linuxVCPU, linuxMemory = 4, 16
	}
	linux := func(arch config.Arch, version string) hostedLabel {
		return hostedLabel{OS: config.GuestLinux, Arch: arch, Version: version, VCPU: linuxVCPU, MemoryGB: linuxMemory}
	}

	switch lower {
	case "ubuntu-latest":
		return linux(config.ArchX64, ""), true
	case "ubuntu-slim":
		return hostedLabel{OS: config.GuestLinux, Arch: config.ArchX64, VCPU: 1, MemoryGB: 5}, true
	case "macos-latest":
		return macOSLabel("", ""), true
	case "macos-latest-large":
		return macOSLabel("", "-large"), true
	case "macos-latest-xlarge":
		return macOSLabel("", "-xlarge"), true
	}

	if m := hostedUbuntu.FindStringSubmatch(lower); m != nil {
		arch := config.ArchX64
		if m[2] != "" {
			arch = config.ArchARM64
		}

		return linux(arch, m[1]), true
	}
	if m := hostedMacOS.FindStringSubmatch(lower); m != nil {
		// macOS 13 AND OLDER RAN ON INTEL AND ARE GONE FROM GITHUB'S REFERENCE, so what
		// a job there needed is not this command's to guess.
		if version, err := strconv.Atoi(m[1]); err == nil && version <= 13 && m[2] == "" {
			return hostedLabel{OS: config.GuestMacOS, NoMapping: "GitHub retired it, and it ran on Intel; pass --map"}, true
		}

		return macOSLabel(m[1], m[2]), true
	}

	switch {
	case strings.HasPrefix(lower, "windows-"):
		return hostedLabel{OS: config.GuestWindows, NoMapping: "billet has no Windows runner"}, true
	case isXcodeLabel(lower):
		return hostedLabel{OS: config.GuestMacOS, Arch: config.ArchARM64, NoMapping: "it names an Xcode rather " +
			"than a macOS version, and which macOS that is cannot be read from a tier; pass --map"}, true
	}

	return hostedLabel{}, false
}

func isXcodeLabel(lower string) bool {
	version, ok := strings.CutPrefix(lower, "xcode-")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(version)

	return err == nil
}

// macOSLabel is a macOS label's shape by its suffix: `-intel` is 4 vCPU and 14 GB on
// x64, `-large` 12 and 30 on x64, `-xlarge` 5 and 14 on arm64, and none 3 and 7 on
// arm64. The two larger shapes need a paid plan and come from GitHub's documentation;
// nothing here has measured them.
func macOSLabel(version, suffix string) hostedLabel {
	label := hostedLabel{OS: config.GuestMacOS, Version: version}
	switch suffix {
	case "-intel":
		label.Arch, label.VCPU, label.MemoryGB = config.ArchX64, 4, 14
	case "-large":
		label.Arch, label.VCPU, label.MemoryGB = config.ArchX64, 12, 30
	case "-xlarge":
		label.Arch, label.VCPU, label.MemoryGB = config.ArchARM64, 5, 14
	default:
		label.Arch, label.VCPU, label.MemoryGB = config.ArchARM64, 3, 7
	}

	return label
}
