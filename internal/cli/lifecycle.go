package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"

	"github.com/junioryono/billet/internal/lifeops/launchd"
)

// Lifecycle is the two ways an operator can hurry a shutdown along.
//
// THREE LEVELS, BECAUSE A DRAIN CAN LAST AS LONG AS A JOB. Before the drain
// existed two were enough — the first signal tore everything down, the second
// gave up on that. Now the first may wait hours for work already running, so
// there has to be a way to say "stop waiting" that is not also "abandon the
// containers you are holding". Otherwise the only lever an impatient operator
// has is the one that strands compute.
//
//	1st  drain: stop taking new work, wait for what is running
//	2nd  stop waiting; the work is LEFT RUNNING and the teardown owed still runs
//	3rd  give up where you are
//
// Carried in a struct rather than in package state so the escalation is
// testable: a test drives Escalate directly with its own channel and its own
// exit function, which is the only way to assert the third level at all.
type Lifecycle struct {
	cancel context.CancelFunc
	hurry  chan struct{}
	once   sync.Once
	stderr io.Writer
}

// NewLifecycle is a lifecycle that cancels cancel at the first level and
// tells the operator on stderr what the later levels did.
func NewLifecycle(cancel context.CancelFunc, stderr io.Writer) *Lifecycle {
	return &Lifecycle{cancel: cancel, hurry: make(chan struct{}), stderr: stderr}
}

// Hurry is closed by the second signal, reaching the drain that honours it.
func (lc *Lifecycle) Hurry() <-chan struct{} { return lc.hurry }

// Rush closes the hurry channel, at most once.
//
// Once, because an operator leaning on Ctrl-C sends far more than three signals
// and a second close of the same channel panics — in the middle of a teardown,
// which is the one moment the process must not die.
func (lc *Lifecycle) Rush() {
	lc.once.Do(func() { close(lc.hurry) })
}

// DrainOn answers every drain request with the first level and nothing more:
// however often one arrives it never counts towards Escalate's levels, which
// is what lets a stop repeat it through a crash, a restart or its own retry.
func (lc *Lifecycle) DrainOn(requests <-chan os.Signal) {
	for range requests {
		lc.cancel()
	}
}

// HandleDrainRequests routes launchd.DrainSignal to DrainOn until the returned
// function is called. Installed before the process says it handles the
// request: an unhandled one is dropped by the Go runtime, and a stop that
// believed otherwise would wait on a drain that never began.
func (lc *Lifecycle) HandleDrainRequests() func() {
	requests := make(chan os.Signal, 1)
	signal.Notify(requests, launchd.DrainSignal)

	done := make(chan struct{})

	go func() {
		defer close(done)

		lc.DrainOn(requests)
	}()

	return func() {
		signal.Stop(requests)
		close(requests)
		<-done
	}
}

// Escalate applies the three levels to a stream of signals.
//
// ONE REGISTRATION FEEDS THIS, because two both receive every signal. The first
// attempt used signal.NotifyContext for the graceful stop AND a second
// signal.Notify for the forced exit; Go delivered the first Ctrl-C to both, so
// the goroutine woke on the cancellation it had itself caused, consumed its own
// signal, and exited immediately. That did not add a forced exit, it deleted the
// graceful one.
//
// exit is a parameter so a test can observe the last level without ending the
// test binary.
func (lc *Lifecycle) Escalate(signals <-chan os.Signal, exit func(int)) {
	<-signals
	lc.cancel()

	<-signals
	// WHAT IT ACTUALLY DOES, WHICH IS NOT WHAT THIS USED TO SAY. It warned that
	// the jobs were about to be destroyed and their builds failed, which was true
	// while an ended drain reached a destructive teardown. It no longer does: the
	// wait ends, the work is LEFT RUNNING, and the next control plane re-adopts
	// its leases.
	//
	// A message that overstates what a signal does is not the harmless direction
	// to be wrong in. An operator told their jobs are already dead has no reason
	// to be careful with the host afterwards, and killing that machine is how
	// compute billet is still accounting for disappears outside its accounting.
	// ROLE-NEUTRAL ON PURPOSE. `billet server` and `billet node` install this same
	// handler, and what recovers the work is not the same process for both: a
	// control plane re-adopts leases when IT returns, while a node's provider
	// inventory is recovered when the NODE returns. Naming either one sends half
	// the operators to restart something that will not help.
	fmt.Fprintln(lc.stderr, "billet: second signal; no longer waiting for the jobs "+
		"still running here. They are LEFT RUNNING — their capacity stays charged "+
		"until the compute is proved gone, and they are re-adopted when the billet "+
		"process responsible for them returns. Nothing here destroys them.")
	lc.Rush()

	<-signals
	fmt.Fprintln(lc.stderr, "billet: third signal; exiting without finishing the "+
		"shutdown. Compute this process was destroying may still be running, and its "+
		"capacity is held until the reaper reclaims it.")
	exit(130)
}
