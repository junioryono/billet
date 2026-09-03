package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A CLOSED LEADERSHIP SIGNAL STOPS THE CONTROL PLANE.
//
// REFUSING THE WRITE IS NOT STOPPING THE PROCESS. Every background writer here
// is deliberately patient with an error it cannot classify, so a replaced
// controller would keep polling GitHub and keep running the cleanup loop that
// calls Runner.Destroy — which never touches the ledger. This is the thing that
// turns the refusal into a stop.
func TestAReplacedControllerStopsItself(t *testing.T) {
	replaced := make(chan struct{})
	stopped := make(chan struct{})

	go stopWhenReplaced(t.Context(), replaced, func() { close(stopped) },
		slog.New(slog.DiscardHandler))

	close(replaced)

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("a replaced controller did not stop itself")
	}
}

// AND AN ORDINARY SHUTDOWN DOES NOT LEAVE IT BLOCKED FOREVER.
//
// The signal is never closed on a healthy deployment, so a watcher that only
// selected on it would outlive every `billet server` this process ever runs —
// and would call stop() from under a later one if the handle were reused.
func TestTheLeadershipWatcherReturnsOnAnOrdinaryShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		stopWhenReplaced(ctx, make(chan struct{}), func() {
			t.Error("an ordinary shutdown must not report this process as replaced")
		}, slog.New(slog.DiscardHandler))
	}()

	cancel()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the leadership watcher did not return when the control plane stopped")
	}
}

// THE CONTROL PLANE IS GIVEN THE LEDGER'S OWN LEADERSHIP CHECK.
//
// PROVING THE MECHANISM IS NOT PROVING IT IS USED, and this is the one seam no
// package-local test can reach. internal/state latches the fact, internal/server
// acts on it, and both are tested where they live — but the only thing that
// joins them is one argument in this file. Delete it and every suite stays
// green while a replaced control plane goes back to destroying compute, closing
// the deployment's message session and handing capacity back on its way out.
//
// A STRUCTURAL TEST BECAUSE THE HAZARD IS AN ABSENCE. There is nothing to
// observe at run time without standing up a control plane, a ledger and a second
// claimant; what has to hold is that the wire exists at all.
//
// WHAT IT DOES NOT PROVE, said rather than implied: that the option reaches
// server.New. It is appended to the same opts slice as every other one, so a
// call built and dropped would be visible in review; an assertion about that
// would have to model the slice, which is a second parser of this function.
func TestTheServerIsGivenTheLedgersLeadershipCheck(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()

	var found, watched []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "runServer" {
				continue
			}

			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				// THE WATCHER, which is the half that turns a refused write into a
				// stopped process. It takes the ledger's own signal, so a caller
				// handing it some other channel would watch something that never
				// closes and leave a replaced controller running.
				if ident, isIdent := call.Fun.(*ast.Ident); isIdent &&
					ident.Name == "stopWhenReplaced" {
					if signalsTheLedger(call) {
						watched = append(watched, fset.Position(call.Pos()).String())
					}

					return true
				}

				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "WithLeadershipLost" || len(call.Args) != 1 {
					return true
				}

				// THE ARGUMENT MATTERS AS MUCH AS THE CALL, and the RECEIVER as much
				// as the method. A literal `func() bool { return false }` satisfies
				// the call graph and disables the whole thing; so does
				// `somethingElse.LeadershipLost` on a handle that is not the ledger
				// this control plane writes through, which would fence against a
				// deployment nobody is scheduling.
				arg, ok := call.Args[0].(*ast.SelectorExpr)
				if !ok || arg.Sel.Name != "LeadershipLost" {
					return true
				}

				recv, ok := arg.X.(*ast.Ident)
				if !ok || recv.Name != "db" {
					return true
				}

				found = append(found, fset.Position(call.Pos()).String())

				return true
			})
		}
	}

	if len(found) == 0 {
		t.Error("runServer does not pass the ledger's LeadershipLost to " +
			"server.WithLeadershipLost.\n" +
			"Without it a control plane that has been replaced tears down normally: it " +
			"destroys the compute its completions asked for, closes this deployment's " +
			"message session and hands capacity back — every one of them an authoritative " +
			"act it no longer has the right to perform, against a ledger that refuses the " +
			"writes and a successor that is already doing all three correctly.")
	}

	if len(watched) == 0 {
		t.Error("runServer does not watch the ledger's LeadershipLostSignal through " +
			"stopWhenReplaced.\n" +
			"Refusing a write is not stopping a process: nothing here treats an " +
			"unclassifiable error as a reason to stop, so a replaced controller would " +
			"keep polling GitHub, keep holding its message session, and keep running the " +
			"cleanup loop that calls Runner.Destroy — which never touches the ledger and " +
			"is therefore fenced by nothing.")
	}
}

// signalsTheLedger reports whether a stopWhenReplaced call is watching
// db.LeadershipLostSignal() rather than some other channel.
func signalsTheLedger(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}

		sel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "LeadershipLostSignal" {
			continue
		}

		if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "db" {
			return true
		}
	}

	return false
}
