package main

import (
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// tartTierImages answers "what does THIS host need", and every clause below is
// one way that differs from "every image in the deployment".
//
// It matters because the answer drives a multi-gigabyte download and a `billet
// check` line that tells an operator a tier cannot run. Fetching another Mac's
// images wastes an hour and a disk; missing this one's means every job on that
// tier fails to launch.
func TestTartTierImagesAreTheOnesThisHostWouldLaunch(t *testing.T) {
	cfg := &config.Config{
		Node: &config.NodeConfig{
			Name:     "mac-mini-1",
			Site:     "office",
			Provider: config.ProviderTart,
		},
		Tiers: []config.Tier{
			{Label: "a", Provider: config.ProviderTart, Image: "macos-xcode"},
			// Pinned to another Mac: its image is that Mac's to fetch.
			{Label: "b", Provider: config.ProviderTart, Node: "mac-mini-2", Image: "macos-other"},
			// Another site, same reasoning.
			{Label: "c", Provider: config.ProviderTart, Site: "home", Image: "macos-home"},
			// A second tier on the same image must not fetch it twice.
			{Label: "d", Provider: config.ProviderTart, Node: "mac-mini-1", Image: "macos-xcode"},
			// A backend this node does not run.
			{Label: "e", Provider: config.ProviderDocker, Image: "nginx:alpine"},
		},
	}

	got, err := tartTierImages(cfg)
	if err != nil {
		t.Fatalf("tartTierImages: %v", err)
	}

	if len(got) != 1 || got[0] != "macos-xcode" {
		t.Errorf("tartTierImages = %v, want just this host's own image once", got)
	}
}

// AND NOTHING AT ALL ON ANOTHER BACKEND, so `billet images pull` keeps its
// firecracker meaning on a firecracker node rather than acquiring a second one.
func TestTartTierImagesAreEmptyOnAnotherProvider(t *testing.T) {
	cfg := &config.Config{
		Node:  &config.NodeConfig{Name: "epyc-1", Provider: config.ProviderFirecracker},
		Tiers: []config.Tier{{Label: "a", Provider: config.ProviderTart, Image: "macos-xcode"}},
	}

	got, err := tartTierImages(cfg)
	if err != nil {
		t.Fatalf("tartTierImages: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("tartTierImages = %v on a firecracker node, want none", got)
	}
}

// IDENTITY IS RESOLVED BEFORE TIERS ARE SELECTED, and this is the case that
// makes it matter.
//
// A TLS node is encouraged to omit node.name: the certificate carries it, and
// nodeBundle fills it in. Selecting tier images against an UNRESOLVED name
// silently skips every tier pinned to this machine — so `billet images pull`
// fetches nothing, `billet check` reports nothing missing, and the failure
// arrives later as jobs that cannot launch on a host somebody just prepared.
//
// The bundle here does not exist, so resolution FAILS. That is the assertion:
// the function must go through identity resolution rather than reading an empty
// name and quietly returning the unpinned subset.
func TestTartTierImagesResolvesIdentityBeforeSelecting(t *testing.T) {
	cfg := &config.Config{
		Node: &config.NodeConfig{
			// No Name: the certificate is meant to supply it.
			Provider: config.ProviderTart,
			TLS: &config.NodeTLS{
				CertPath: filepath.Join(t.TempDir(), "node.crt"),
				KeyPath:  filepath.Join(t.TempDir(), "node.key"),
				CAPath:   filepath.Join(t.TempDir(), "ca.crt"),
			},
		},
		Tiers: []config.Tier{
			{Label: "a", Provider: config.ProviderTart, Node: "mac-mini-1", Image: "macos-xcode"},
		},
	}

	images, err := tartTierImages(cfg)
	if err == nil {
		t.Fatalf("tartTierImages = %v with no readable certificate; it cannot know which "+
			"tiers are pinned to this node, and returning a subset silently is how an "+
			"image goes unfetched", images)
	}

	if len(images) != 0 {
		t.Errorf("tartTierImages returned %v alongside its error", images)
	}
}
