package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// stagedImagesFake stands in for the staged candidate binary: it records every
// invocation, answers `images compatible` with the status and result the test
// chose, and accepts every `images pull`.
func stagedImagesFake(t *testing.T, compatibleStatus int, names []string) (string, func() []string) {
	t.Helper()

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + record + "\n" +
		"if [ \"$2\" = compatible ]; then\n" +
		"  while [ $# -gt 0 ]; do if [ \"$1\" = --result-file ]; then shift; out=$1; fi; shift; done\n" +
		"  printf '" + strings.Join(names, "\\n") + "' > \"$out\"; [ -n \"$out\" ] && [ -s \"$out\" ] || true\n" +
		"  exit " + itoa(compatibleStatus) + "\n" +
		"fi\n" +
		"exit 0\n"
	staged := filepath.Join(dir, "billet.candidate")
	if err := os.WriteFile(staged, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return staged, func() []string {
		body, err := os.ReadFile(record)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read the fake's record: %v", err)
		}
		var lines []string
		for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func firecrackerHost(staged string) *systemdHost {
	return &systemdHost{
		ledgerHost: ledgerHost{
			cfg: &config.Config{Node: &config.NodeConfig{
				Provider: config.ProviderFirecracker,
				Ceph:     &config.CephConfig{},
			}},
			cfgPath: "/etc/billet/billet.yaml",
		},
		staged: staged,
	}
}

// THE CANDIDATE'S ANSWER IS ITS EXIT STATUS: 2 names the images that need a
// generation, and each one is pulled and verified through the staged binary.
func TestAnUpgradePullsEveryImageTheCandidateSaysNeedsAGeneration(t *testing.T) {
	staged, recorded := stagedImagesFake(t, 2, []string{"ubuntu-2404-x64", "ubuntu-2404-arm64"})

	if err := firecrackerHost(staged).PrepareImages(t.Context()); err != nil {
		t.Fatalf("PrepareImages: %v", err)
	}

	got := recorded()
	want := []string{
		"images compatible --config /etc/billet/billet.yaml --result-file " +
			filepath.Join(filepath.Dir(staged), "guest-images-to-refresh"),
		"images pull --verify --config /etc/billet/billet.yaml ubuntu-2404-x64",
		"images pull --verify --config /etc/billet/billet.yaml ubuntu-2404-arm64",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the candidate was run as:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Status 0 is nothing to do, and nothing is pulled.
func TestAnUpgradePullsNothingWhenEveryImageIsCompatible(t *testing.T) {
	staged, recorded := stagedImagesFake(t, 0, nil)

	if err := firecrackerHost(staged).PrepareImages(t.Context()); err != nil {
		t.Fatalf("PrepareImages: %v", err)
	}

	for _, line := range recorded() {
		if strings.HasPrefix(line, "images pull") {
			t.Fatalf("a compatible image was pulled anyway: %v", recorded())
		}
	}
}

// ANY OTHER STATUS IS "COULD NOT TELL", which refuses the upgrade rather than
// reading as nothing to do: a candidate that cannot reach the cluster is not a
// candidate that proved its images.
func TestAnUpgradeRefusesWhenTheCandidateCannotJudgeItsImages(t *testing.T) {
	staged, recorded := stagedImagesFake(t, 1, nil)

	err := firecrackerHost(staged).PrepareImages(t.Context())
	if err == nil || !strings.Contains(err.Error(), "could not say whether") {
		t.Fatalf("PrepareImages = %v, want a refusal naming the unanswered question", err)
	}

	for _, line := range recorded() {
		if strings.HasPrefix(line, "images pull") {
			t.Fatalf("an image was pulled on an unanswered question: %v", recorded())
		}
	}
}

// A host that boots no firecracker guests runs nothing at all.
func TestAnUpgradeOnANonFirecrackerHostTouchesNoImages(t *testing.T) {
	staged, recorded := stagedImagesFake(t, 2, []string{"x"})
	h := firecrackerHost(staged)
	h.cfg.Node.Provider = config.ProviderDocker
	h.cfg.Node.Ceph = nil

	if err := h.PrepareImages(t.Context()); err != nil {
		t.Fatalf("PrepareImages: %v", err)
	}
	if len(recorded()) != 0 {
		t.Fatalf("the candidate was run on a host with no guest images: %v", recorded())
	}
}
