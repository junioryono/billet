package e2e

import (
	"context"
	"errors"
	"net"
	"slices"
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

// runScheduler runs a controller's scheduler until the function it answers is called,
// which joins it, and the test's end calls it if nothing has.
func runScheduler(t *testing.T, c *assembled) func() {
	t.Helper()

	scheduler := c.schedule(t, app.ScheduleOptions{Hurry: make(chan struct{})})
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))

	var running sync.WaitGroup

	running.Go(func() {
		if err := scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the scheduler ended with %v", err)
		}
	})

	stop := sync.OnceFunc(func() {
		cancel()
		running.Wait()
	})
	t.Cleanup(stop)

	return stop
}

// A SUPERSEDED PROCESS KEEPS WHAT IT HOLDS WHEN THE CONTROLLER IS REPLACED (#685). A
// process holds a running job and another registers under its name. The controller goes,
// and the one that replaces it, knowing nothing, fills the ownership gap for the process
// that registered: the ledger's launched set says the lease is this node's. The
// superseded process must not register, so what restores its right to renew and release
// is its claim, on whichever controller answers.
//
// THEN GITHUB SAYS THE JOB IS DONE, through the new controller's own listener. A destroy
// that reached the current process would be answered truthfully that it has nothing to
// remove, and the lease released under a guest still running. It stays held, until the
// guest has gone and the process that holds it says so.
//
// THE SAME WHETHER THE CONTROLLER MOVED OR ONLY RESTARTED, because a restart at one
// address loses the record exactly as a failover does. On a shared machine the current
// process holds the guest too, and either process's proof that it is gone settles it.
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
				Owner: harnessOwner,
				Server: []server.ControlPlaneOption{
					server.WithDrainTimeout(200 * time.Millisecond),
					server.WithCleanupRetry(200*time.Millisecond, time.Second),
				},
			}

			first := openAssembled(t, harnessConfig(dir, 8, 16*config.GiB, tiers), targets, steering)
			deployment := first.ctl.Deployment()

			// A SHORT POLL WINDOW, because a registration does not wake a poll already
			// parked: the superseded process hears it at its poll's next answer, which
			// the default window puts up to fifty seconds away.
			first.wire.Plane().SetPollWindowForTest(time.Second)

			// THE ADDRESS THE NEXT CONTROLLER SERVES AT, known to the node before it
			// exists when it is another, as a standby's is.
			next := first.wire.Addr

			var controllers []string

			if tc.moved {
				ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("reserve the second address: %v", err)
				}

				next = ln.Addr().String()
				if err := ln.Close(); err != nil {
					t.Fatalf("release the second address: %v", err)
				}

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

			stopFirst := runScheduler(t, first)

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
			stopFirst()
			first.close()

			cfg := harnessConfig(dir, 8, 16*config.GiB, tiers)
			cfg.Server.Listen = next

			replaced := openAssembled(t, cfg, targets, steering)
			plane := replaced.wire.Plane()
			plane.SetPollWindowForTest(time.Second)
			ledger := replaced.ctl.Allocator()

			waitUntil(t, "the current process to register with the new controller", func() bool {
				return plane.CurrentIncarnationForTest(host) == current.incarnation
			})

			// IT IS THE FIRST PROCESS'S ON THE NEW CONTROLLER, with the request, so a
			// completion's destroy cannot go to a process that has nothing to remove.
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

			runScheduler(t, replaced)

			// GITHUB SAYS IT IS DONE, and billet has handled that once it acknowledges.
			done := fakeactions.JobJSON("JobCompleted", requestID, "push", testTier)
			done["result"] = "Succeeded"
			p.queue(fakeactions.StatisticsJSON(0, 0), done)

			waitUntil(t, "billet to handle the completion", func() bool {
				return slices.Contains(p.ackedIDs(), 3)
			})

			// ON A SECOND MACHINE NOTHING THAT CAN REACH THE GUEST WAS ASKED, so it runs and
			// its lease stays held until it ends. ON THE SAME ONE the current process holds
			// the guest too, and may have removed it, which is the job finishing.
			if !tc.sameMachine {
				if _, found, err := holderProvider.Find(t.Context(), inst.Name); err != nil || !found {
					t.Fatalf("the completion removed an instance only the superseded process has: found=%v err=%v", found, err)
				}

				if _, err := ledger.Lease(t.Context(), leaseID); err != nil {
					t.Fatalf("the lease was released under a running job: %v", err)
				}

				if _, err := holderProvider.Destroy(t.Context(), inst.ID); err != nil {
					t.Fatalf("end the job: %v", err)
				}
			}

			waitUntil(t, "the holder to settle the lease", func() bool {
				_, err := ledger.Lease(t.Context(), leaseID)

				return errors.Is(err, alloc.ErrLeaseNotFound)
			})

			select {
			case <-holder.done:
			case <-time.After(30 * time.Second):
				t.Fatal("the superseded process did not stop once it held nothing")
			}
		})
	}
}
