package e2e

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/codebuild"
)

// THE HOLDER-GONE REPRODUCTION AGAINST REAL CODEBUILD AND REAL PARAMETER STORE.
//
// The fake AWS in holdergone_test.go models this repository's UNDERSTANDING of
// the API — a stop that never confirms, a build that ends on its own, a
// registration that stays in Parameter Store — and three of the nine things
// measured while writing the backend contradicted that understanding (see
// docs/aws-acceptance.md). So the same scenario runs here over the real
// service: the real listener, plane, wire, node loop and provider, with only
// GitHub faked. The build ends on its own because the runner inside refuses
// the fake GitHub's registration and the buildspec's guard fails the build.
//
// IT SKIPS UNLESS ASKED, exactly as realcodebuild_test.go does: an acceptance
// test that silently starts billable compute in whatever account a contributor
// happens to be signed into is worse than one that skips. It costs a few cents.
//
// The reaper tick is slowed to two seconds because every tick is an inventory
// walk of the project — ListBuildsForProject and BatchGetBuilds — and a walk
// every 200ms is a throttled account rather than a faster test.
func liveCodeBuildProviders(
	t *testing.T,
) func(t *testing.T, shapes []config.RemoteShape, deployment string) *codebuild.Provider {
	t.Helper()

	project := os.Getenv("BILLET_TEST_CODEBUILD_PROJECT")
	if project == "" {
		t.Skip("set BILLET_TEST_CODEBUILD_PROJECT to a NO_SOURCE CodeBuild project this " +
			"test may start billable builds in, with AWS credentials in the environment")
	}

	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Skip("no AWS credentials in the environment; billet reads env-var or IMDSv2 only")
	}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-west-2"
	}

	return func(t *testing.T, shapes []config.RemoteShape, deployment string) *codebuild.Provider {
		t.Helper()

		p, err := codebuild.New(deployment, config.CodeBuildConfig{
			Region:                     region,
			Project:                    project,
			EnvironmentType:            config.CodeBuildLinuxContainer,
			PrivilegedMode:             true,
			AcceptExternalBuildCeiling: true,
			// THE PATH THE LIVE PROVIDER TEST USES, so one build-role grant serves
			// both.
			JITParameterPath: "/billet/realtest/jit",
			// THE TIGHTEST CEILINGS THE SERVICE ALLOWS, because they size the
			// inventory walk the node makes on every sweep.
			BuildTimeoutMinutes:  config.CodeBuildBuildFloorMinutes,
			QueuedTimeoutMinutes: config.CodeBuildQueuedFloorMinutes,
			ComputeTypes:         shapes,
		}, codebuild.WithCredentials(liveCredentials{}))
		if err != nil {
			t.Fatalf("codebuild.New: %v", err)
		}

		return p
	}
}

// liveCredentials reads the ordinary AWS environment variables.
type liveCredentials struct{}

func (liveCredentials) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}, nil
}

// reapLiveBuild guarantees that nothing this test started outlives it.
//
// REGISTERED BEFORE THE LAUNCH, and it does not depend on the test having
// observed anything: every build billet starts is named for a lease in this
// stack's own ledger, so the ledger is the list of names to account for, and
// the provider's listing of live builds under this deployment's identity is
// the second. ON A DETACHED CONTEXT, because t.Context() is cancelled just
// before cleanups run, the one moment billable compute is most likely to be
// left behind.
//
// IT PROVES BEFORE IT DELETES. A remote inventory is eventually consistent,
// so one empty listing is not proof a build is over: every name is looked up
// by itself, and what is accepted is an explicit TERMINAL record, or an absence
// sustained across a window longer than a create takes to appear — a lease
// that never reached StartBuild has no build to find. A transient inventory
// error is retried rather than read as anything. Only then is a staged
// registration removed: deleting it while a build is still starting is the
// "runner that never registers" failure the provider's own contract is
// arranged against. The scenario itself leaves the registration behind on
// purpose — the process that would have reaped it was killed — which is the
// residual ADR-007 records.
func (w *wiredCodeBuild) reapLiveBuild(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
		defer cancel()

		// A sustained absence is twelve consecutive misses five seconds apart —
		// a minute, longer than a create takes to become visible. An error is
		// not a miss: it restarts the run, because "could not look" is not
		// "looked and saw nothing".
		const absencesForGone = 12

		names := map[string]bool{}
		absences := map[string]int{}
		ledgerComplete := false
		deadline := time.Now().Add(8 * time.Minute)

		for {
			// EVERY LEASE THE LEDGER EVER HAD IS A NAME A BUILD MAY CARRY. Read
			// until it succeeds: a transient ledger error must not disable the
			// cleanup, and the provider's own listing below runs regardless.
			if !ledgerComplete {
				ids, err := w.ledgerLeaseIDs(ctx)
				if err != nil {
					t.Logf("cleanup: could not list this stack's leases yet, retrying: %v", err)
				} else {
					for _, id := range ids {
						names[provider.InstanceName(id)] = true
					}
					if w.liveName != "" {
						names[w.liveName] = true
					}
					ledgerComplete = true
				}
			}

			if live, err := w.cb.List(ctx); err != nil {
				t.Logf("cleanup: could not list live builds, retrying: %v", err)
			} else {
				for _, inst := range live {
					names[inst.Name] = true
					absences[inst.Name] = 0

					if _, err := w.cb.Destroy(ctx, inst.ID); err != nil {
						t.Logf("cleanup: could not stop %s yet: %v", inst.Name, err)
					}
				}
			}

			settled := ledgerComplete
			for name := range names {
				inst, found, err := w.cb.Find(ctx, name)
				if err != nil {
					t.Logf("cleanup: could not look up %s, retrying: %v", name, err)
				}

				proved := err == nil && found && inst.Terminal
				absences[name] = absenceRun(absences[name], err, found, proved)
				if !proved && absences[name] < absencesForGone {
					settled = false
				}
			}

			if settled {
				break
			}

			if time.Now().After(deadline) {
				t.Errorf("cleanup: could not prove every build of this stack is over; check the project by hand")

				return
			}

			time.Sleep(5 * time.Second)
		}

		for name := range names {
			if err := w.cb.ReapStagedCredential(ctx, name); err != nil {
				t.Errorf("cleanup: staged registration for %s may remain: %v", name, err)
			}
		}
	})
}

// ledgerLeaseIDs is every lease this stack's ledger has ever held, open or
// archived.
func (w *wiredCodeBuild) ledgerLeaseIDs(ctx context.Context) ([]string, error) {
	rows, err := w.db.Reader().QueryContext(ctx,
		`SELECT id FROM leases UNION SELECT lease_id FROM job_history`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// absenceRun advances one name's run of consecutive absences: a miss extends
// it, a sighting or a terminal record ends it, and an ERROR ends it too — a
// lookup that failed observed nothing, and a run it merely paused would let
// eleven misses, an outage and one more miss pass for twelve.
func absenceRun(prev int, lookupErr error, found, terminal bool) int {
	switch {
	case lookupErr != nil, found, terminal:
		return 0
	default:
		return prev + 1
	}
}

// THE STATE THE ACCEPTANCE RECORD DESCRIBES, END TO END, ON REAL CODEBUILD.
//
// The same scenario as the fake-AWS test of this name: the process that
// launched the build dies, the build ends on its own, a replacement registers
// under the same name, and GitHub's completion arrives bound to the dead
// process. What it proves that the fake cannot: that a real build the
// replacement's real inventory no longer lists is settled by that inventory
// after the grace, and that the completion's retry corrects the verdict to
// GitHub's — against the service's own consistency, its own listing, and its
// own idea of when a build is terminal.
func TestACompletionBoundToAKilledIncarnationIsSettledOnRealCodeBuild(t *testing.T) {
	providers := liveCodeBuildProviders(t)

	w := newWiredCodeBuild(t, withRealProviders(providers), withWiredReapInterval(2*time.Second))
	leaseID := w.runJobToCompletionOnADeadHolder(t)

	waitUntilWithin(t, 2*time.Minute, "the orphaned completion's lease to be quarantined", func() bool {
		phase, held := heldPhase(t, w.alloc, leaseID)

		return held && phase == alloc.PhaseQuarantine
	})

	if _, err := w.alloc.Lease(t.Context(), leaseID); err != nil {
		t.Fatalf("quarantine released the lease before any proof: %v", err)
	}

	// The replacement's inventory is the real project's: the build is terminal
	// and the walk no longer lists it. The grace is what turns that absence
	// into settlement, and the clock is the allocator's.
	w.clock.advance(pastGrace)

	waitUntilWithin(t, 2*time.Minute, "the replacement's inventory to settle the quarantined lease", func() bool {
		_, err := w.alloc.Lease(t.Context(), leaseID)

		return errors.Is(err, alloc.ErrLeaseNotFound)
	})

	waitUntilWithin(t, 2*time.Minute, "the history to carry the completion's outcome", func() bool {
		outcome, err := w.alloc.HistoryOutcome(t.Context(), leaseID)

		return err == nil && outcome == string(alloc.PhaseDone)
	})

	if phase, held := heldPhase(t, w.alloc, leaseID); held {
		t.Fatalf("the settled lease is still reported held as %s", phase)
	}

	// THE COMPLETION'S RETRY FINDS THE LEASE OVER AND THE RECORD GOES WITH IT.
	// Waited for rather than asserted, because the inventory settles the lease
	// as done on its own — GitHub's result was recorded before the destroy — and
	// the retry that drops the record runs on its own pacing after that.
	waitUntilWithin(t, 2*time.Minute, "the completion's retry to end the owner record", func() bool {
		_, present := w.wire.OwnerOfLease(leaseID)

		return !present
	})

	t.Logf("settled on real CodeBuild: lease %s, build %s", leaseID, provider.InstanceName(leaseID))
}
