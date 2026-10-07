package app

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/version"
)

// channelResolver is what the automatic starter asks for a target.
//
// THE SAME RESOLUTION AND THE SAME PREFLIGHT `billet rollout start` RUNS, through
// the same two functions, so the channel a person resolves and the channel the
// control plane resolves cannot drift: ResolveTarget fetches and verifies the
// signed statement and manifest, and releasesource.Compatibility refuses a
// candidate this deployment could not serve. A target comes back only when both
// agreed; a refusal is an error the starter logs and waits out.
type channelResolver struct {
	client  *releasesource.Client
	policy  releasesource.Policy
	channel string
	pin     string
	current releasesource.Current
}

func (r channelResolver) Resolve(ctx context.Context) (rollout.Target, error) {
	manifest, digest, err := ResolveTarget(ctx, r.client, r.policy, r.channel, r.pin)
	if err != nil {
		return rollout.Target{}, err
	}

	warnings, err := releasesource.Compatibility(manifest, r.current)
	if err != nil {
		return rollout.Target{}, err
	}

	target := rollout.Target{Version: manifest.Version, Digest: digest}
	for _, w := range warnings {
		target.Notes = append(target.Notes, w.Error())
	}

	return target, nil
}

// NewRolloutStarter assembles the automatic starter the way `rollout start`
// would resolve the same config by hand.
func NewRolloutStarter(cfg *config.Config, store *rollout.Store, fleet rollout.Fleet,
	current releasesource.Current,
) (*rollout.Starter, error) {
	policy, err := ReleasePolicyFor(cfg, false)
	if err != nil {
		return nil, fmt.Errorf("release signing policy: %w", err)
	}

	resolver := channelResolver{
		client:  &releasesource.Client{},
		policy:  policy,
		channel: cfg.Release.EffectiveChannel(),
		pin:     cfg.Release.PinnedVersion(),
		current: current,
	}

	return rollout.NewStarter(store, fleet, resolver, rollout.StartPolicy{
		Enabled: cfg.Release.AutomaticUpdates(),
		OpenAt:  cfg.Release.OpenAt,
		Channel: resolver.channel,
		Pin:     resolver.pin,
		Rollout: rollout.DefaultPolicy(),
	}, version.Version()), nil
}

// LedgerFleet is what the rollout coordinator knows about the hosts.
//
// THE LEDGER, NOT THE PLANE'S MEMORY. A host's release and negotiated wire are
// recorded at registration and survive a control-plane restart; the plane's
// in-memory view does not. A successor that had to wait for every host to
// reconnect before it could resume a rollout would stall for as long as the
// quietest node's poll interval, on exactly the restart the rollout caused.
type LedgerFleet struct{ Alloc *alloc.Allocator }

func (f LedgerFleet) Hosts(ctx context.Context) ([]rollout.Host, error) {
	fleet, err := f.Alloc.NodeWireVersions(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]rollout.Host, 0, len(fleet))
	for i := range fleet {
		out = append(out, rollout.Host{
			Name:    fleet[i].Name,
			Release: fleet[i].Release,
			Digest:  fleet[i].Digest,
			Wire:    fleet[i].Negotiated,
			Live:    fleet[i].Live,
			Epoch:   fleet[i].Epoch,
		})
	}

	return out, nil
}

// PlaneDispatcher tells one node to replace its own billet, over the node wire.
type PlaneDispatcher struct{ Runner *nodeplane.Runner }

func (d PlaneDispatcher) Upgrade(ctx context.Context, node, release, manifestSHA256,
	rolloutID string, generation int64,
) error {
	return d.Runner.Upgrade(ctx, node, nodeapi.UpgradeSpec{
		Version:        release,
		ManifestSHA256: manifestSHA256,
		RolloutID:      rolloutID,
		Generation:     generation,
	})
}

// ReleasePolicyFor decides what a release manifest must be signed by for this
// deployment.
//
// ONE ANSWER FOR THE COMMAND, THE UPDATER AND THE AUTOMATIC STARTER. A
// deployment mirroring billet's releases internally names its own signing
// identity and issuer in `release:`, and a path that consulted only the built-in
// default would refuse that mirror's every manifest — or, with the waiver, trust
// it unsigned. The waiver still wins, because skipping verification is an act
// somebody performs on the command line rather than something a config implies.
func ReleasePolicyFor(cfg *config.Config, skipVerify bool) (releasesource.Policy, error) {
	policy, err := releasesource.PolicyForRelease(skipVerify)
	if err != nil || skipVerify || cfg == nil || cfg.Release == nil {
		return policy, err
	}

	if cfg.Release.SigningIdentity != "" {
		if cfg.Release.SigningIdentity != policy.Identity ||
			cfg.Release.SigningIssuer != policy.Issuer {
			policy.SourceRepositoryURI = ""
		}
		policy.Identity = cfg.Release.SigningIdentity
		policy.Issuer = cfg.Release.SigningIssuer
	}

	return policy, nil
}

// ResolveTarget turns a channel or an exact pin into one immutable manifest.
func ResolveTarget(ctx context.Context, client *releasesource.Client,
	policy releasesource.Policy, channel, pin string,
) (*releasesource.Manifest, string, error) {
	// AN EXACT PIN NEVER MOVES, and it is resolved without touching a channel at
	// all. An operator who typed a version is asking for that version, and
	// consulting a pointer would let a channel's opinion reach a decision the
	// operator had already made.
	if pin != "" {
		return client.Manifest(ctx, pin, "", policy)
	}

	statement, err := client.Resolve(ctx, channel, policy)
	if err != nil {
		return nil, "", err
	}

	return client.Manifest(ctx, statement.Tag, statement.ManifestSHA256, policy)
}
