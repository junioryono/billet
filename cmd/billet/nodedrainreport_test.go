package main

import (
	"go/ast"
	"os"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops/launchd"
)

// THE DRAIN REQUEST IS THE FIRST LEVEL, HOWEVER OFTEN IT ARRIVES. A stop sends it
// on every poll, so if a repeat counted towards escalate's levels the second
// poll would end the drain's wait; it must cancel and never hurry.
func TestADrainRequestDrainsAndNeverEscalates(t *testing.T) {
	var cancels atomic.Int32

	lc := newLifecycle(func() { cancels.Add(1) })

	requests := make(chan os.Signal, 5)
	for range 5 {
		requests <- launchd.DrainSignal
	}

	close(requests)
	lc.drainOn(requests)

	if cancels.Load() == 0 {
		t.Fatal("a drain request did not start the drain")
	}

	select {
	case <-lc.hurry:
		t.Fatal("repeated drain requests hurried the drain: a stop repeating its request would end the wait")
	default:
	}
}

// AND THE REAL SIGNAL REACHES IT. Serial, because a signal is sent to this whole
// test process; a process with no handler drops it, so the wait below fails
// rather than the process dying.
func TestTheDrainSignalReachesTheDrain(t *testing.T) {
	var cancels atomic.Int32

	lc := newLifecycle(func() { cancels.Add(1) })

	stop := lc.handleDrainRequests()
	defer stop()

	for range 3 {
		if err := syscall.Kill(os.Getpid(), launchd.DrainSignal); err != nil {
			t.Fatalf("send the drain request: %v", err)
		}
	}

	waitFor(t, "the drain request to cancel", func() bool { return cancels.Load() > 0 })

	select {
	case <-lc.hurry:
		t.Fatal("the drain request hurried the drain")
	default:
	}
}

// THE REPORT IS PUBLISHED ON A MAC ONLY, for the node's own label.
func TestTheNodeDrainReportIsPublishedOnlyOnAMac(t *testing.T) {
	restore := publishDrainReport
	t.Cleanup(func() { publishDrainReport = restore })

	var labels []string

	publishDrainReport = func(label, _ string) error {
		labels = append(labels, label)

		return nil
	}

	publishNodeDrainReport("linux")

	if len(labels) != 0 {
		t.Fatalf("a Linux node published a drain report: %v", labels)
	}

	publishNodeDrainReport("darwin")

	if len(labels) != 1 || labels[0] != deploy.NodeAgentLabel {
		t.Errorf("published for %v, want exactly %s", labels, deploy.NodeAgentLabel)
	}
}

// THE NODE HANDLES THE REQUEST BEFORE IT SAYS SO, and says so before it serves.
//
// STRUCTURAL because the hazard is an order: a report published before the
// handler is installed tells a stop to send a signal the Go runtime then drops,
// and the stop waits on a drain that never began. The upgrade probe returns
// before either, because it is not the node a stop asks.
func TestTheNodeHandlesTheDrainRequestBeforeReportingIt(t *testing.T) {
	fn := findFunc(t, "cmdNode")

	first := map[string]int{}
	last := map[string]int{}

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		name := calleeName(call)
		if name == "" {
			return true
		}

		if _, seen := first[name]; !seen {
			first[name] = int(call.Pos())
		}

		last[name] = int(call.Pos())

		return true
	})

	handler, okHandler := first["handleDrainRequests"]
	report, okReport := first["publishNodeDrainReport"]
	probe, okProbe := last["holdProbe"]
	serve, okServe := last["Run"]

	if !okHandler || !okReport || !okProbe || !okServe {
		t.Fatalf("cmdNode lost a call this test orders: handler %v, report %v, probe %v, run %v",
			okHandler, okReport, okProbe, okServe)
	}

	if handler < probe || report < probe {
		t.Error("the upgrade probe can reach the drain handler or its report")
	}

	if report < handler {
		t.Error("the node reports it handles the drain request before it does")
	}

	if serve < report {
		t.Error("the node serves before it reports it handles the drain request")
	}
}
