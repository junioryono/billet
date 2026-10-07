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

	if cancels.Load() != 5 {
		t.Fatalf("five drain requests started the drain %d times, want every one", cancels.Load())
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

	// ONE AT A TIME, each acknowledged before the next, so no two coalesce and
	// every repeat is one the handler actually received.
	for i := int32(1); i <= 3; i++ {
		if err := syscall.Kill(os.Getpid(), launchd.DrainSignal); err != nil {
			t.Fatalf("send the drain request: %v", err)
		}

		waitFor(t, "the drain request to be received", func() bool { return cancels.Load() == i })
	}

	stop()

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
//
// EACH IS A STATEMENT OF cmdNode's OWN BODY, IN ITS SHAPE, not a call found
// anywhere in it: the handler is installed by `stop := lc.handleDrainRequests()`,
// a call that runs where it stands, and `defer stop()` follows it (a call wrapped
// in a deferred closure would run at return), the report is a plain statement after it, the
// probe is the `if *upgradeProbe` block that returns, and serving is the final
// `return n.Run(...)` on the node app.OpenNode returned.
func TestTheNodeHandlesTheDrainRequestBeforeReportingIt(t *testing.T) {
	fn := findFunc(t, "cmdNode")

	probe, handler, stopped, report, serve := -1, -1, -1, -1, -1
	stopName, nodeName := "", ""

	for i, stmt := range fn.Body.List {
		switch s := stmt.(type) {
		case *ast.IfStmt:
			if star, ok := s.Cond.(*ast.StarExpr); ok {
				body := s.Body.List
				if id, ok := star.X.(*ast.Ident); ok && id.Name == "upgradeProbe" && len(body) > 0 {
					if _, returns := body[len(body)-1].(*ast.ReturnStmt); returns {
						probe = i
					}
				}
			}

		case *ast.AssignStmt:
			if opened, ok := s.Rhs[0].(*ast.CallExpr); ok && len(s.Lhs) == 2 && calleeName(opened) == "OpenNode" {
				if name, ok := s.Lhs[0].(*ast.Ident); ok {
					nodeName = name.Name
				}
			}

			if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
				continue
			}

			call, isCall := s.Rhs[0].(*ast.CallExpr)
			name, isName := s.Lhs[0].(*ast.Ident)

			if isCall && isName && calleeName(call) == "handleDrainRequests" {
				handler, stopName = i, name.Name
			}

		case *ast.DeferStmt:
			if id, ok := s.Call.Fun.(*ast.Ident); ok && stopName != "" && id.Name == stopName {
				stopped = i
			}

		case *ast.ExprStmt:
			if call, ok := s.X.(*ast.CallExpr); ok && calleeName(call) == "publishNodeDrainReport" {
				report = i
			}

		case *ast.ReturnStmt:
			if len(s.Results) != 1 {
				continue
			}

			call, ok := s.Results[0].(*ast.CallExpr)
			if !ok {
				continue
			}

			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
				if recv, ok := sel.X.(*ast.Ident); ok && nodeName != "" && recv.Name == nodeName {
					serve = i
				}
			}
		}
	}

	if probe < 0 || handler < 0 || stopped < 0 || report < 0 || serve < 0 {
		t.Fatalf("cmdNode lost a statement this test orders (index -1): probe %d, handler %d, "+
			"deferred stop %d, report %d, serve %d", probe, handler, stopped, report, serve)
	}

	if stopped != handler+1 {
		t.Error("the drain handler's stop is not deferred immediately after it is installed")
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

// THE NODE IS GIVEN WHAT A STOP ASKS OF IT. Node.Run hands the loop whatever
// its host says (internal/app reads that), so only this package's source shows
// the host names the drain-request file, the second signal and the service
// manager's notification: without the first, a node set to hand over would
// leave guests behind under an operation that must not.
func TestTheNodeHostCarriesTheDrainRequest(t *testing.T) {
	fn := findFunc(t, "nodeHost")

	isIdent := func(name string) func(ast.Expr) bool {
		return func(v ast.Expr) bool {
			id, ok := v.(*ast.Ident)

			return ok && id.Name == name
		}
	}

	want := map[string]func(ast.Expr) bool{
		"DrainRequested": isIdent("nodeDrainRequested"),
		"Ready":          isIdent("notifyReady"),
		"Hurry": func(v ast.Expr) bool {
			sel, ok := v.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "hurry" {
				return false
			}

			return isIdent("lc")(sel.X)
		},
	}
	found := map[string]bool{}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}

		if key, ok := kv.Key.(*ast.Ident); ok && want[key.Name] != nil && want[key.Name](kv.Value) {
			found[key.Name] = true
		}

		return true
	})

	for key := range want {
		if !found[key] {
			t.Errorf("nodeHost does not give the node %s", key)
		}
	}
}
