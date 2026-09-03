package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/store/ceph"
)

// fakeRefreshStore answers with one newest generation, or none.
type fakeRefreshStore struct {
	newest ceph.Generation
	found  bool
}

func (s fakeRefreshStore) NewestGeneration(context.Context, string) (ceph.Generation, bool, error) {
	return s.newest, s.found, nil
}

// refreshRun stages the seams a refresh runs through and records what it did.
type refreshRun struct {
	pulled  []string
	reaped  []string
	pullErr error
}

// firecrackerRefreshConfig writes a config for a firecracker node with one tier
// naming one image, and returns its path.
func firecrackerRefreshConfig(t *testing.T, automatic string) string {
	t.Helper()

	body := strings.Replace(firecrackerNodeConfig, "node:", automatic+"node:", 1)

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// firecrackerNodeConfig is the smallest firecracker node config the refresh
// accepts: a loopback control plane beside it, one tier naming @verified.
const firecrackerNodeConfig = `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/app-private-key.pem
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: /var/lib/billet/node
  firecracker:
    kernel_image: /var/lib/billet/kernels/vmlinux
    bridge: billet0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
tiers:
  - label: small
    provider: firecracker
    vcpu: 2
    memory: 4GiB
    image: ubuntu-2404-x64@verified
`

func stageRefresh(t *testing.T, store fakeRefreshStore, builtAt time.Time) *refreshRun {
	t.Helper()

	run := &refreshRun{}

	restoreStore, restoreFetch, restorePull, restoreReap :=
		openRefreshStore, fetchImageManifest, refreshPull, refreshReap

	t.Cleanup(func() {
		openRefreshStore, fetchImageManifest, refreshPull, refreshReap =
			restoreStore, restoreFetch, restorePull, restoreReap
	})

	openRefreshStore = func(*config.Config) (refreshStore, error) { return store, nil }
	fetchImageManifest = func(context.Context, *config.Config) (*imagesource.Manifest, error) {
		return &imagesource.Manifest{BuiltAt: builtAt}, nil
	}
	refreshPull = func(_ context.Context, _, image string) error {
		run.pulled = append(run.pulled, image)

		return run.pullErr
	}
	refreshReap = func(_ context.Context, _, image string, keep int) error {
		run.reaped = append(run.reaped, image)

		if keep != 3 {
			t.Errorf("reap asked to keep %d, want 3", keep)
		}

		return nil
	}

	return run
}

var (
	channelBuilt = time.Date(2026, 9, 1, 4, 17, 0, 0, time.UTC)
	older        = ceph.Generation{Name: "g20260825120000", Built: channelBuilt.Add(-7 * 24 * time.Hour)}
	newer        = ceph.Generation{Name: "g20260902120000", Built: channelBuilt.Add(32 * time.Hour)}
)

// A GENERATION OLDER THAN THE CHANNEL'S BUILD IS REFRESHED, THEN REAPED. A
// generation is named for the moment it was imported, which is after the build
// it came from, so the comparison is the whole decision.
func TestARefreshPullsWhenTheChannelIsNewerThanWhatIsImported(t *testing.T) {
	run := stageRefresh(t, fakeRefreshStore{newest: older, found: true}, channelBuilt)

	err := cmdImagesRefresh(t.Context(), []string{"--config", firecrackerRefreshConfig(t, "")})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(run.pulled) != 1 || run.pulled[0] != "ubuntu-2404-x64" {
		t.Errorf("pulled %v, want the bare image name once", run.pulled)
	}

	if len(run.reaped) != 1 || run.reaped[0] != "ubuntu-2404-x64" {
		t.Errorf("reaped %v, want the image once after its pull", run.reaped)
	}
}

// A GENERATION IMPORTED AFTER THE CHANNEL'S BUILD IS UP TO DATE, and nothing is
// pulled or reaped — the same image must never be imported twice.
func TestARefreshDoesNothingWhenTheImportedGenerationIsNewer(t *testing.T) {
	run := stageRefresh(t, fakeRefreshStore{newest: newer, found: true}, channelBuilt)

	err := cmdImagesRefresh(t.Context(), []string{"--config", firecrackerRefreshConfig(t, "")})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(run.pulled) != 0 || len(run.reaped) != 0 {
		t.Errorf("pulled %v and reaped %v for an image already newer than the channel",
			run.pulled, run.reaped)
	}
}

// NO GENERATION AT ALL IS A PULL. A fresh node has nothing to boot until one is
// imported, and the channel is where it comes from.
func TestARefreshPullsWhenNothingIsImported(t *testing.T) {
	run := stageRefresh(t, fakeRefreshStore{}, channelBuilt)

	err := cmdImagesRefresh(t.Context(), []string{"--config", firecrackerRefreshConfig(t, "")})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(run.pulled) != 1 {
		t.Errorf("pulled %v, want one pull for a node with no generation", run.pulled)
	}
}

// A FAILED PULL REAPS NOTHING, and is reported. Reaping after a pull that left
// nothing new would be a cleanup nobody asked for on a day something went wrong.
func TestAFailedPullIsReportedAndReapsNothing(t *testing.T) {
	run := stageRefresh(t, fakeRefreshStore{newest: older, found: true}, channelBuilt)
	run.pullErr = errors.New("the channel expired")

	err := cmdImagesRefresh(t.Context(), []string{"--config", firecrackerRefreshConfig(t, "")})
	if err == nil || !strings.Contains(err.Error(), "the channel expired") {
		t.Fatalf("a failed pull was not reported: %v", err)
	}

	if len(run.reaped) != 0 {
		t.Errorf("reaped %v after a failed pull", run.reaped)
	}
}

// `automatic: false` LEAVES IMAGES TO AN OPERATOR: nothing is fetched, nothing
// is pulled, and the exit is clean because a timer exiting red on every healthy
// opted-out host is noise.
func TestARefreshDoesNothingWhenAutomaticUpdatesAreOff(t *testing.T) {
	run := stageRefresh(t, fakeRefreshStore{newest: older, found: true}, channelBuilt)

	err := cmdImagesRefresh(t.Context(), []string{"--config",
		firecrackerRefreshConfig(t, "release:\n  automatic: false\n")})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(run.pulled) != 0 {
		t.Errorf("pulled %v with automatic updates off", run.pulled)
	}
}

// A DRY RUN DECIDES AND DOES NOTHING.
func TestARefreshDryRunPullsNothing(t *testing.T) {
	run := stageRefresh(t, fakeRefreshStore{newest: older, found: true}, channelBuilt)

	err := cmdImagesRefresh(t.Context(), []string{"--config", firecrackerRefreshConfig(t, ""),
		"--dry-run"})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(run.pulled) != 0 || len(run.reaped) != 0 {
		t.Errorf("a dry run pulled %v and reaped %v", run.pulled, run.reaped)
	}
}
