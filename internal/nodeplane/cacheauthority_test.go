package nodeplane

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// waitForQueued waits for one command to be queued for a node, so a test that
// changes the node's registration does so after dispatch.
func waitForQueued(t *testing.T, p *Plane, name string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for p.QueuedForTest(name) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("nothing was queued for %s", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cacheConfiguredTier() config.Tier {
	tier := testTier()
	tier.Cache = &config.TierCache{Publish: config.CachePublishOff}

	return tier
}

// A TIER THAT CONFIGURES ITS CACHES IS NOT SENT TO A NODE THAT CANNOT HONOUR
// IT. An older node would ignore `publish: off` and publish a trusted pool's
// writes anyway; refused before dispatch, nothing started and the lease can go.
func TestACacheConfiguredTierIsNotLaunchedOnAnOlderNode(t *testing.T) {
	t.Parallel()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute, WithTierCatalog([]config.Tier{cacheConfiguredTier()}),
		WithRegistrar(&recordingRegistrar{}))
	if _, err := p.Register(t.Context(), releasedNode()); err != nil {
		t.Fatalf("register: %v", err)
	}

	// BOUNDED, so a launch that was dispatched instead of refused fails here in
	// seconds rather than waiting out the command timeout for a node that never
	// polls.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := p.NewRunner().Launch(ctx, testLease(), server.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) || !strings.Contains(err.Error(), "cannot honour") {
		t.Fatalf("launch = %v, want ErrNoNode naming the cache block", err)
	}
}

// A PROCESS THAT RE-REGISTERS ON AN OLDER WIRE AFTER DISPATCH DOES NOT TAKE
// THE LAUNCH: the check that chose the node is repeated when the command is
// delivered, because a queued command outlives the registration it was
// checked against.
func TestAnOlderProcessCannotTakeACacheConfiguredLaunchQueuedForANewerOne(t *testing.T) {
	t.Parallel()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute, WithTierCatalog([]config.Tier{cacheConfiguredTier()}),
		WithRegistrar(&recordingRegistrar{}), WithCommandTimeout(5*time.Second))
	current := releasedNode()
	current.Version, current.MinVersion = nodeapi.Version, nodeapi.MinVersion
	current.Incarnation = "new"
	if _, err := p.Register(t.Context(), current); err != nil {
		t.Fatalf("register the current process: %v", err)
	}

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()
	waitForQueued(t, p, "n1")

	older := releasedNode()
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

// AND A NODE THAT CAN IS GIVEN THE TIER'S EFFECTIVE CACHE CONFIGURATION with the
// launch, every default applied, so it needs no copy of the rules.
func TestACurrentNodeReceivesTheTiersCacheConfiguration(t *testing.T) {
	t.Parallel()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute, WithTierCatalog([]config.Tier{cacheConfiguredTier()}),
		WithCommandTimeout(5*time.Second))
	register(t, p, "n1", config.ProviderDocker)

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	cmd, took, err := p.Poll(t.Context(), "n1", "")
	if err != nil || !took || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
	}
	if cmd.Tier == nil || cmd.Tier.Cache == nil || cmd.Tier.Cache.Publish != config.CachePublishOff ||
		!cmd.Tier.Cache.Docker.Enabled {
		t.Fatalf("launched tier cache = %+v, want publish off with its defaults", cmd.Tier)
	}
	if err := p.Result("n1", "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
}

// A COMPLETION'S AUTHORITY RIDES ON ITS DESTROY, field for field, and a destroy
// with none carries none, so a node never mistakes a teardown for permission.
func TestACompletionsCacheAuthorityRidesOnItsDestroy(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(5*time.Second))
	register(t, p, "n1", config.ProviderDocker)

	authority := server.CacheAuthority{LeaseID: "l1", JobID: "job-7", RunID: 31, Owner: "acme",
		Repository: "api", Event: "push", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
		Proven: true, WriteOwnRef: true, PublishDefault: true}

	for _, tc := range []struct {
		authority server.CacheAuthority
		want      *nodeapi.CacheAuthority
	}{
		{authority: authority, want: WireCacheAuthority(authority)},
		{authority: server.CacheAuthority{}, want: nil},
	} {
		done := make(chan error, 1)
		go func() {
			done <- p.NewRunner().DestroyCompleted(t.Context(), 7, "succeeded", tc.authority)
		}()

		cmd, took, err := p.Poll(t.Context(), "n1", "")
		if err != nil || !took || cmd.Kind != nodeapi.CommandDestroy {
			t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
		}
		switch {
		case tc.want == nil && cmd.CacheAuthority != nil:
			t.Errorf("a destroy with no authority carried %+v", cmd.CacheAuthority)
		case tc.want != nil && (cmd.CacheAuthority == nil || *cmd.CacheAuthority != *tc.want):
			t.Errorf("authority on the wire = %+v, want %+v", cmd.CacheAuthority, tc.want)
		}
		if err := p.Result("n1", "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			t.Fatalf("result: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("destroy: %v", err)
		}
	}
}
