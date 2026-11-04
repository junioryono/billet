package e2e

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/simulated"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// A NODE THAT NAMES EVERY CONTROLLER FOLLOWS THE DEPLOYMENT TO THE ONE SERVING.
// A control plane serves the node wire at one address and a node, naming that
// address and a second, registers there. The control plane stops; another,
// over the same ledger, serves at the second address, as a promoted standby
// does; and the same node process registers with it, its incarnation unchanged,
// without any address moving. The order is the assembly's own: the second
// serves the wire only once it is the controller.
func TestANodeFollowsTheDeploymentToTheControllerServingIt(t *testing.T) {
	dir := t.TempDir()
	ledgertest.Seed(t, dir)

	p := newPlane(t)

	client, err := scaleset.New(scaleset.Config{
		Target:         billetgithub.OrganizationTarget("acme"),
		GitHubURL:      p.URL,
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     p.PrivateKeyPEM(),
		AppID:          12345,
		APIURL:         p.URL + "/api/v3",
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	targets := []app.Target{{Config: config.GitHubTarget{Name: config.DefaultTargetName, Org: "acme"}, Client: client}}
	tiers := []config.Tier{{
		Label: testTier, Provider: config.ProviderSimulated, VCPU: 2, Memory: 2 * config.GiB,
		Disk: 10 * config.GiB, Image: "simulated", RunnerGroup: testGroup, GuestOS: config.GuestLinux,
		Trust: config.WorkloadTrusted, Workflows: []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},
		Command: simulated.RunFor(simulatedRunsFor),
	}}
	steering := &app.Steering{Owner: harnessOwner}

	// THE SECOND CONTROLLER'S ADDRESS, known to the node before that controller
	// exists, as a standby's is.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve the second address: %v", err)
	}

	second := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the second address: %v", err)
	}

	first := openAssembled(t, harnessConfig(dir, 8, 16*config.GiB, tiers), targets, steering)
	deployment := first.ctl.Deployment()

	prov, err := simulated.New(deployment, simulated.OnHost(simulated.NewHost()))
	if err != nil {
		t.Fatalf("simulated.New: %v", err)
	}

	const host = "e2e-roaming"

	node := startNodeLoop(t, testLogger(t), "", nodeProcess{
		host: host, deployment: deployment, provider: prov, kind: config.ProviderSimulated,
		hurry: make(chan struct{}), controllers: []string{first.wire.Addr, second},
	})

	first.awaitRegistered(t, 1)

	if got := first.wire.Plane().CurrentIncarnationForTest(host); got != node.incarnation {
		t.Fatalf("the first controller holds incarnation %q, the node is %q", got, node.incarnation)
	}

	// THE FIRST GOES, and nothing listens at its address after.
	first.close()

	cfg := harnessConfig(dir, 8, 16*config.GiB, tiers)
	cfg.Server.Listen = second

	promoted := openAssembled(t, cfg, targets, steering)
	if promoted.wire.Addr != second {
		t.Fatalf("the second controller serves at %s, want the address the node was given (%s)",
			promoted.wire.Addr, second)
	}

	deadline := time.Now().Add(30 * time.Second)
	for promoted.wire.Plane().CurrentIncarnationForTest(host) != node.incarnation {
		if time.Now().After(deadline) {
			t.Fatalf("the node never registered with the controller at its second address")
		}

		time.Sleep(20 * time.Millisecond)
	}

	// AND POLLS IT, which is where its commands now come from: a registration
	// alone says the node reached it once, not that the node dials it.
	for promoted.wire.Plane().WaitersForTest(host) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the node registered with the second controller and never polled it")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// A SUPERSEDED PROCESS KEEPS WHAT IT HOLDS WHEN THE CONTROLLER IS REPLACED (#685). A
// process holds a running job and another registers under its name on a machine that
// cannot see it. The controller goes, and the one that replaces it, knowing nothing,
// fills the ownership gap for the process that registered: the ledger's launched set
// says the lease is this node's. The superseded process must not register, so what
// restores its right to renew and release is its claim, on whichever controller
// answers. Then it finishes the job, as it would have on the controller it knew.
//
// THE SAME WHETHER THE CONTROLLER MOVED OR ONLY RESTARTED, because a restart at one
// address loses the record exactly as a failover does.
//
// AND WHERE THE PROCESS THAT REGISTERED SHARES THE MACHINE, it reports the compute, and
// the claim answers that it is that process's: the superseded one lets go without
// destroying anything, and stops.
func TestASupersededProcessFollowsTheControllerThatReplacedTheOneItKnew(t *testing.T) {
	for _, tc := range []struct {
		name string
		// moved is a controller at a second address, as a promoted standby is;
		// otherwise it comes back at the first.
		moved bool
		// sameMachine is a current process that can see what the superseded one runs.
		sameMachine bool
	}{
		{name: "a second controller, and a second machine", moved: true},
		{name: "the one controller restarting, and a second machine"},
		{name: "a second controller, and the same machine", moved: true, sameMachine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ledgertest.Seed(t, dir)

			p := newPlane(t)

			client, err := scaleset.New(scaleset.Config{
				Target:         billetgithub.OrganizationTarget("acme"),
				GitHubURL:      p.URL,
				ClientID:       "12345",
				InstallationID: 67890,
				PrivateKey:     p.PrivateKeyPEM(),
				AppID:          12345,
				APIURL:         p.URL + "/api/v3",
			}, nil)
			if err != nil {
				t.Fatalf("scaleset.New: %v", err)
			}

			targets := []app.Target{{Config: config.GitHubTarget{Name: config.DefaultTargetName, Org: "acme"}, Client: client}}
			tiers := []config.Tier{{
				Label: testTier, Provider: config.ProviderSimulated, VCPU: 2, Memory: 2 * config.GiB,
				Disk: 10 * config.GiB, Image: "simulated", RunnerGroup: testGroup, GuestOS: config.GuestLinux,
				Trust: config.WorkloadTrusted, Workflows: []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},
				Command: simulated.RunFor(simulatedRunsFor),
			}}
			steering := &app.Steering{
				Owner:  harnessOwner,
				Server: []server.ControlPlaneOption{server.WithDrainTimeout(200 * time.Millisecond)},
			}

			first := openAssembled(t, harnessConfig(dir, 8, 16*config.GiB, tiers), targets, steering)
			deployment := first.ctl.Deployment()

			// THE ADDRESS THE NEXT CONTROLLER SERVES AT, known to the node before it
			// exists when it is another, as a standby's is.
			next := first.wire.Addr

			if tc.moved {
				ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("reserve the second address: %v", err)
				}

				next = ln.Addr().String()
				if err := ln.Close(); err != nil {
					t.Fatalf("release the second address: %v", err)
				}
			}

			var controllers []string
			if tc.moved {
				controllers = []string{first.wire.Addr, next}
			}

			machine := simulated.NewHost()

			holderProvider, err := simulated.New(deployment, simulated.OnHost(machine))
			if err != nil {
				t.Fatalf("simulated.New: %v", err)
			}

			const (
				host      = "e2e-superseded"
				requestID = 8101
			)

			start := func(prov provider.Provider, supersedable bool) *nodeLoop {
				hurry := make(chan struct{})
				loop := startNodeLoop(t, testLogger(t), first.wire.Addr, nodeProcess{
					host: host, deployment: deployment, provider: prov, kind: config.ProviderSimulated,
					hurry: hurry, controllers: controllers, supersedable: supersedable,
				})

				t.Cleanup(func() {
					close(hurry)
					loop.stop()
				})

				return loop
			}

			holder := start(holderProvider, true)
			first.awaitRegistered(t, 1)

			schedulerCtx, stopScheduler := context.WithCancel(context.WithoutCancel(t.Context()))
			defer stopScheduler()

			scheduler := first.schedule(t, app.ScheduleOptions{Hurry: make(chan struct{})})

			var running sync.WaitGroup

			running.Go(func() {
				if err := scheduler.Run(schedulerCtx); err != nil && !errors.Is(err, context.Canceled) {
					t.Logf("the first controller's scheduler ended with %v", err)
				}
			})

			p.queue(fakeactions.StatisticsJSON(1, 0), fakeactions.JobJSON("JobAvailable", requestID, "push", testTier))
			waitUntil(t, "billet to bid for the available job", func() bool { return len(p.acquiredIDs()) > 0 })
			p.queue(fakeactions.StatisticsJSON(0, 1), fakeactions.JobJSON("JobAssigned", requestID, "push", testTier))

			var inst *provider.Instance

			waitUntil(t, "the job to run on the first process", func() bool {
				list, err := holderProvider.List(t.Context())
				if err != nil || len(list) == 0 {
					return false
				}

				inst = list[0]

				return true
			})

			leaseID, ours := provider.LeaseOf(inst.Name)
			if !ours {
				t.Fatalf("instance %q does not carry a billet lease name", inst.Name)
			}

			// ANOTHER PROCESS TAKES THE NAME, on its own machine or on this one, and
			// the first learns it on its next poll.
			currentMachine := simulated.NewHost()
			if tc.sameMachine {
				currentMachine = machine
			}

			currentProvider, err := simulated.New(deployment, simulated.OnHost(currentMachine))
			if err != nil {
				t.Fatalf("simulated.New: %v", err)
			}

			current := start(currentProvider, false)

			waitUntil(t, "the second process to hold the name", func() bool {
				return first.wire.Plane().CurrentIncarnationForTest(host) == current.incarnation
			})
			// ONLY ONCE IT HAS HEARD: a process that had not would register with the next
			// controller, which is a different race (and the claim's other case).
			waitUntil(t, "the first process to learn it was superseded", func() bool {
				return holder.runner.HoldsInCustodyForTest(leaseID)
			})

			// THE CONTROLLER GOES, and another comes up over the same ledger.
			stopScheduler()
			running.Wait()
			first.close()

			cfg := harnessConfig(dir, 8, 16*config.GiB, tiers)
			cfg.Server.Listen = next

			replaced := openAssembled(t, cfg, targets, steering)
			plane := replaced.wire.Plane()

			waitUntil(t, "the current process to register with the new controller", func() bool {
				return plane.CurrentIncarnationForTest(host) == current.incarnation
			})

			if tc.sameMachine {
				// THE CURRENT PROCESS HAS THE COMPUTE, so the first lets go and stops,
				// and nothing happens to the job.
				select {
				case <-holder.done:
				case <-time.After(30 * time.Second):
					t.Fatal("the superseded process did not let go of what the current one adopted")
				}

				if !plane.OwnsForTest(leaseID, host, current.incarnation) {
					t.Fatal("the lease left the process that has the compute")
				}

				if _, found, err := holderProvider.Find(t.Context(), inst.Name); err != nil || !found {
					t.Fatalf("letting go removed the instance: found=%v err=%v", found, err)
				}

				if _, err := replaced.ctl.Allocator().Lease(t.Context(), leaseID); err != nil {
					t.Fatalf("letting go ended the lease: %v", err)
				}

				return
			}

			// IT IS THE FIRST PROCESS'S ON THE NEW CONTROLLER, with the request, so a
			// completion's destroy cannot go to the process that has nothing to remove.
			waitUntil(t, "the superseded process's claim to land", func() bool {
				return plane.OwnsForTest(leaseID, host, holder.incarnation)
			})

			owner, known := plane.OwnerOfRequest(requestID)
			if !known || owner.Incarnation != holder.incarnation || owner.Current {
				t.Fatalf("the request's owner is %+v (known=%v), want the superseded process", owner, known)
			}

			if got := plane.CurrentIncarnationForTest(host); got != current.incarnation {
				t.Fatalf("the claim took the name: the node is %q, want %q", got, current.incarnation)
			}

			if _, err := replaced.ctl.Allocator().Lease(t.Context(), leaseID); err != nil {
				t.Fatalf("the lease of a running job ended: %v", err)
			}

			// THE JOB ENDS, and the process that held it settles it and stops.
			if _, err := holderProvider.Destroy(t.Context(), inst.ID); err != nil {
				t.Fatalf("end the job: %v", err)
			}

			waitUntil(t, "the holder to settle the lease", func() bool {
				_, err := replaced.ctl.Allocator().Lease(t.Context(), leaseID)

				return errors.Is(err, alloc.ErrLeaseNotFound)
			})

			if outcome, err := replaced.ctl.Allocator().HistoryOutcome(t.Context(), leaseID); err != nil ||
				outcome != string(alloc.PhaseDone) {
				t.Fatalf("history outcome = %q err=%v, want done", outcome, err)
			}

			select {
			case <-holder.done:
			case <-time.After(30 * time.Second):
				t.Fatal("the superseded process did not stop once it held nothing")
			}
		})
	}
}
