package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/ops/images"

	"github.com/junioryono/billet/internal/config"
)

func TestCompatibilitySelectionUsesTheCertificateDerivedNodeName(t *testing.T) {
	t.Parallel()

	serverCfg := writeCAConfig(t, t.TempDir())
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if err := cmdCAIssue(t.Context(), processEnv(), []string{"epyc-1", "--config", serverCfg, "--out", bundleDir}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	cfg := nodeConfigWithoutName(t, t.TempDir(), bundleDir)
	cfg.Node.Provider = config.ProviderFirecracker
	cfg.Node.Site = "home"
	cfg.Tiers = []config.Tier{
		{Provider: config.ProviderFirecracker, Image: "shared@verified"},
		{Provider: config.ProviderFirecracker, Image: "this-node@verified", Node: "epyc-1"},
		{Provider: config.ProviderFirecracker, Image: "other-node@verified", Node: "epyc-2"},
	}

	want := []string{"shared@verified", "this-node@verified"}
	got, err := images.FirecrackerTierImages(cfg)
	if err != nil {
		t.Fatalf("select certificate-scoped firecracker images: %v", err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("certificate-scoped firecracker images = %v, want %v", got, want)
	}
	if cfg.Node.Name != "epyc-1" {
		t.Fatalf("resolved node name = %q, want certificate identity epyc-1", cfg.Node.Name)
	}
}
