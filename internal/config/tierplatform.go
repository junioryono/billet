package config

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// imageX64 and imageARM64 find an architecture named as a whole word of an image,
// so `ubuntu-2404-x64@verified` names one and `linux-x640` does not.
var (
	imageX64   = regexp.MustCompile(`(?i)(^|[^a-z0-9])(x64|amd64|x86[-_]64)([^a-z0-9]|$)`)
	imageARM64 = regexp.MustCompile(`(?i)(^|[^a-z0-9])(arm64|aarch64)([^a-z0-9]|$)`)
	// imageUbuntu finds `ubuntu-2404`, `ubuntu-24.04` and `ubuntu:24.04`.
	imageUbuntu = regexp.MustCompile(`(?i)(^|[^a-z0-9])ubuntu[-_:]?(\d{2})\.?(\d{2})(\D|$)`)
	// imageMacOS finds `macos-15` and `macos-26.5`.
	imageMacOS = regexp.MustCompile(`(?i)(^|[^a-z0-9])macos[-_:]?(\d{2})(\D|$)`)
)

// macOSCodenames is the version of each macOS whose images are named for it rather
// than numbered, as the images billet's tart tiers boot are.
var macOSCodenames = map[string]string{"sonoma": "14", "sequoia": "15", "tahoe": "26", "golden-gate": "27"}

// TierPlatform is what a tier's jobs run on, as far as its configuration fixes it:
// the architecture and the operating system's version, each empty where it does
// not. It is for a reader choosing a tier for somebody's workflow, which has to
// tell "not this" from "cannot tell", and it decides nothing about placement.
type TierPlatform struct {
	Arch      Arch
	OSVersion string
}

// Platform reads what this tier's configuration fixes.
//
// THE ARCHITECTURE IS THE TIER'S OWN WHERE IT NAMES ONE (ADR-022). Failing that it
// is arm64 where every way the tier can run is Apple silicon: a macOS guest, or tart
// alone. Failing that, it is what every image the tier can boot names; images that
// disagree, or one that names none, are unknown. A tier on a docker image with no
// such word runs on whatever the host is, and a config cannot say what that is.
func (t Tier) Platform() TierPlatform {
	images := t.images()

	return TierPlatform{Arch: t.platformArch(images), OSVersion: t.platformVersion(images)}
}

// images is every image this tier can boot, one per backend.
func (t Tier) images() []string {
	providers := t.AcceptableProviders()
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, t.ImageFor(p))
	}

	return out
}

func (t Tier) platformArch(images []string) Arch {
	switch {
	case t.Arch != "":
		return t.Arch
	case t.GuestOS == GuestMacOS:
		return ArchARM64
	}

	providers := t.AcceptableProviders()
	if len(providers) > 0 && !slices.ContainsFunc(providers, func(p ProviderKind) bool { return p != ProviderTart }) {
		return ArchARM64
	}

	return onePerImage(images, imageArch)
}

func imageArch(image string) string {
	x64, arm := imageX64.MatchString(image), imageARM64.MatchString(image)
	switch {
	case x64 && !arm:
		return string(ArchX64)
	case arm && !x64:
		return string(ArchARM64)
	default:
		return ""
	}
}

func (t Tier) platformVersion(images []string) string {
	switch t.GuestOS {
	case GuestMacOS:
		return string(onePerImage(images, imageMacOSVersion))
	case GuestWindows:
		return ""
	default:
		return string(onePerImage(images, imageUbuntuVersion))
	}
}

func imageUbuntuVersion(image string) string {
	m := imageUbuntu.FindStringSubmatch(image)
	if m == nil {
		return ""
	}

	return m[2] + "." + m[3]
}

func imageMacOSVersion(image string) string {
	if m := imageMacOS.FindStringSubmatch(image); m != nil {
		return strconv.Itoa(mustTwoDigits(m[2]))
	}

	lower := strings.ToLower(image)
	var found string
	for name, version := range macOSCodenames {
		if !strings.Contains(lower, name) {
			continue
		}
		if found != "" {
			return ""
		}
		found = version
	}

	return found
}

// mustTwoDigits reads what the pattern already proved is two ASCII digits.
func mustTwoDigits(s string) int { return int(s[0]-'0')*10 + int(s[1]-'0') }

// onePerImage is what every image says, empty when they disagree or any says
// nothing, since a tier that may boot either cannot promise one. A silent image
// disagrees with any that speaks, so disagreement is the one test.
func onePerImage(images []string, read func(string) string) Arch {
	var out string
	for i, image := range images {
		got := read(image)
		if i > 0 && got != out {
			return ""
		}
		out = got
	}

	return Arch(out)
}
