package nodeplane

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// ec2CacheTier is an EC2 tier with the caches the node serves to its guest, which an
// older EC2 node refuses a launch for.
func ec2CacheTier() config.Tier {
	on := true
	tier := testTier()
	tier.Provider = config.ProviderEC2
	tier.Cache = &config.TierCache{
		Git: &config.CacheToggle{Enabled: &on},
		Go:  &config.GoCache{Enabled: &on},
	}

	return tier
}

func ec2Lease() *alloc.Lease {
	lease := testLease()
	lease.Providers = []config.ProviderKind{config.ProviderEC2}

	return lease
}

// launchOn dispatches a launch to a node registered at wire and returns what it took.
func launchOn(t *testing.T, tier config.Tier, wire int) nodeapi.Command {
	t.Helper()
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute, WithTierCatalog([]config.Tier{tier}),
		WithRegistrar(&recordingRegistrar{}), WithCommandTimeout(5*time.Second))
	node := releasedNode()
	node.Provider = config.ProviderEC2
	node.Version, node.MinVersion = wire, nodeapi.MinVersion
	if _, err := p.Register(t.Context(), node); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(ctx, ec2Lease(), dispatch.Job{RequestID: 7}) }()
	cmd, took, err := p.Poll(t.Context(), node.Node, node.Incarnation)
	if err != nil || !took || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
	}
	if err := p.Result(node.Node, node.Incarnation, nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}
	<-launched

	return cmd
}

// AN OLDER EC2 NODE REFUSES A LAUNCH THAT NAMES A GUEST CACHE, which would fail every
// job on a tier that merely has them by default. So it is sent the tier with them off
// and the job runs as it did before: doing less, never failing.
func TestAnOlderEC2NodeIsSentATierWithoutItsGuestCaches(t *testing.T) {
	t.Parallel()
	cmd := launchOn(t, ec2CacheTier(), nodeapi.VersionEC2GuestCaches-1)
	spec := cmd.Tier.Cache
	if spec == nil {
		t.Fatal("the launch carries no cache block")
	}
	if spec.Git.Enabled || spec.Go.Enabled || spec.Bazel.Enabled || spec.Actions.Enabled || spec.Npm {
		t.Fatalf("an older EC2 node was sent guest caches: %+v", spec)
	}
	if !spec.Docker.Enabled || !spec.StickyDisks.Enabled {
		t.Fatalf("the caches an older EC2 node does serve were turned off too: %+v", spec)
	}
	if cmd.Tier.Intercept {
		t.Fatal("an older EC2 node was told to intercept")
	}
}

func TestACurrentEC2NodeIsSentItsGuestCaches(t *testing.T) {
	t.Parallel()
	cmd := launchOn(t, ec2CacheTier(), nodeapi.Version)
	if spec := cmd.Tier.Cache; spec == nil || !spec.Git.Enabled || !spec.Go.Enabled {
		t.Fatalf("a current EC2 node was not sent its guest caches: %+v", cmd.Tier.Cache)
	}
}

// ANOTHER BACKEND'S OLDER NODE ALREADY SERVES THEM, and loses nothing.
func TestAnOlderFirecrackerNodeKeepsItsGuestCaches(t *testing.T) {
	t.Parallel()
	tier := ec2CacheTier()
	tier.Provider = config.ProviderFirecracker
	lease := testLease()
	lease.Providers = []config.ProviderKind{config.ProviderFirecracker}
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute, WithTierCatalog([]config.Tier{tier}),
		WithRegistrar(&recordingRegistrar{}), WithCommandTimeout(5*time.Second))
	node := releasedNode()
	node.Provider = config.ProviderFirecracker
	node.Version, node.MinVersion = nodeapi.VersionEC2GuestCaches-1, nodeapi.MinVersion
	if _, err := p.Register(t.Context(), node); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(ctx, lease, dispatch.Job{RequestID: 7}) }()
	cmd, took, err := p.Poll(t.Context(), node.Node, node.Incarnation)
	if err != nil || !took {
		t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
	}
	if err := p.Result(node.Node, node.Incarnation, nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}
	<-launched
	if !cmd.Tier.Cache.Git.Enabled || !cmd.Tier.Cache.Go.Enabled {
		t.Fatalf("an older firecracker node lost its guest caches: %+v", cmd.Tier.Cache)
	}
}

// A QUEUED COMMAND OUTLIVES THE REGISTRATION IT WAS CHECKED AGAINST. A process that
// re-registers on an older wire would refuse this launch, so it does not take it.
func TestAnOlderProcessCannotTakeAnEC2LaunchNamingGuestCaches(t *testing.T) {
	t.Parallel()
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{ec2CacheTier()}), WithRegistrar(&recordingRegistrar{}),
		WithCommandTimeout(2*time.Second))
	current := releasedNode()
	current.Provider = config.ProviderEC2
	current.Version, current.MinVersion = nodeapi.Version, nodeapi.MinVersion
	current.Incarnation = "new"
	if _, err := p.Register(t.Context(), current); err != nil {
		t.Fatalf("register the current process: %v", err)
	}
	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), ec2Lease(), dispatch.Job{RequestID: 7}) }()
	waitForQueued(t, p, "n1")

	older := releasedNode()
	older.Provider = config.ProviderEC2
	older.Version, older.MinVersion = nodeapi.VersionEC2GuestCaches-1, nodeapi.MinVersion
	older.Incarnation = "old"
	if _, err := p.Register(t.Context(), older); err != nil {
		t.Fatalf("register the older process: %v", err)
	}
	if cmd, took, err := p.Poll(t.Context(), "n1", "old"); err == nil && took {
		t.Fatalf("the older process took %s", cmd.Kind)
	}
	if err := <-launched; err == nil {
		t.Fatal("the launch reported success though nothing took it")
	}
}
