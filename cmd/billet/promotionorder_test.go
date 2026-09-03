package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authoritativeAfterTheClaim names what a control plane may only do once it
// holds this deployment's controller claim.
//
// EVERY ONE OF THESE IS AN ACT ONLY THE CONTROLLER MAY TAKE, and each is here
// because a standby doing it would be wrong in its own way:
//
//   - ForgetEveryNode WRITES, and it writes the fleet's liveness to zero. A
//     standby doing it would blank the running controller's view of its own
//     fleet.
//   - serveNodeWire opens the listeners nodes register and heartbeat against, and
//     it takes the ONE authority read the whole wire is built from. Taken early,
//     a `billet ca rotate` between standby start and promotion would leave the
//     promoted controller presenting, trusting and issuing from a stale snapshot
//     — the not-fail-closed state LoadServing's single-read design exists to
//     prevent, reached again through a new door.
//   - Watch and BarrierLoop are timers that write.
//   - plane.Run polls GitHub, opens message sessions and dispatches.
var authoritativeAfterTheClaim = []string{
	"ForgetEveryNode",
	"serveNodeWire",
	"Watch",
	"BarrierLoop",
	"Run",
	// AND THE AUTHORITY IS ADOPTED AFTER THE CLAIM TOO, because installing
	// identity material is a write on a deployment this host may not hold.
	"adoptSharedAuthority",
}

// EVERYTHING AUTHORITATIVE HAPPENS AFTER THE CLAIM.
//
// A STRUCTURAL TEST BECAUSE THE HAZARD IS AN ORDERING, and an ordering has
// nothing to observe at run time without standing up a control plane, a shared
// ledger and a second claimant. What has to hold is that no call below is
// reachable before becomeController returns — which is the whole of what makes a
// standby safe, because a standby IS runServer stopped at that line.
//
// WHAT IT DOES NOT PROVE, said rather than implied: that the calls do what their
// names suggest, or that becomeController waits rather than returning
// immediately. Those are covered where they live — the state package's standby
// suite for the wait, and the ordinary server tests for the calls. What no other
// test can see is a future edit that hoists one of them above the claim, which
// is exactly the kind of change that reads as a harmless tidy-up.
func TestEverythingAuthoritativeHappensAfterTheControllerClaim(t *testing.T) {
	fn := findFunc(t, "runServer")

	claim := -1
	positions := map[string]int{}

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		name := calleeName(call)
		if name == "" {
			return true
		}

		offset := int(call.Pos())

		if name == "becomeController" {
			claim = offset

			return true
		}

		// THE FIRST OCCURRENCE IS THE ONE THAT MATTERS. A call that appears twice
		// is only safe if its EARLIEST appearance is after the claim.
		for _, want := range authoritativeAfterTheClaim {
			if name == want {
				if prev, seen := positions[name]; !seen || offset < prev {
					positions[name] = offset
				}
			}
		}

		return true
	})

	if claim < 0 {
		t.Fatal("runServer no longer calls becomeController, so nothing takes this " +
			"deployment's controller claim before it schedules")
	}

	for _, name := range authoritativeAfterTheClaim {
		offset, found := positions[name]
		if !found {
			t.Errorf("runServer no longer calls %s; if it moved, move it in this list too "+
				"rather than deleting the guard", name)

			continue
		}

		if offset < claim {
			t.Errorf("%s is called BEFORE becomeController. A standby reaches that line "+
				"while another host is the controller, so this would have it act on a "+
				"deployment it does not hold", name)
		}
	}
}

// THE AUTHORITY IS ADOPTED BEFORE ANYTHING READS ONE.
//
// serveNodeWire's single authority read goes through wirecert.LoadServing, which
// calls LoadOrCreateCA — and that CREATES an authority when the directory is
// empty. A promoted standby that has never held this deployment's CA and reaches
// the wire first therefore mints a RIVAL one, after which every node in the fleet
// fails to verify the control plane and drops off at once, while the control
// plane itself looks perfectly healthy.
//
// AND IT IS PUBLISHED AFTER, for the opposite reason: on a first controller the
// wire is what creates the authority, so publishing earlier would publish
// nothing and leave the store empty for the host that has to adopt from it.
//
// A STRUCTURAL TEST BECAUSE BOTH HAZARDS ARE ORDERINGS, and neither has anything
// to observe at run time without two hosts, a shared store and a real fleet.
func TestTheAuthorityIsAdoptedBeforeTheWireAndPublishedAfterIt(t *testing.T) {
	fn := findFunc(t, "runServer")

	first := map[string]int{}

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		name := calleeName(call)
		if _, seen := first[name]; !seen {
			first[name] = int(call.Pos())
		}

		return true
	})

	adopt, adopted := first["adoptSharedAuthority"]
	wire, served := first["serveNodeWire"]
	publish, published := first["publishSharedAuthority"]

	switch {
	case !adopted:
		t.Fatal("runServer no longer adopts this deployment's authority, so a promoted " +
			"standby with an empty ca directory would mint a rival one and drop the fleet")
	case !served:
		t.Fatal("runServer no longer calls serveNodeWire")
	case !published:
		t.Fatal("runServer no longer publishes this deployment's authority, so a second " +
			"controller would have nothing to adopt")
	}

	if adopt > wire {
		t.Error("the authority is adopted AFTER the node wire is served. The wire's own " +
			"read creates an authority when there is none, so this host would mint a rival " +
			"one before ever looking at the store")
	}

	if publish < wire {
		t.Error("the authority is published BEFORE the node wire is served. On a first " +
			"controller the wire is what creates it, so there would be nothing to publish")
	}
}

// findFunc parses this package's non-test sources and returns one declaration.
func findFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()

	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") ||
			strings.HasSuffix(file, "_test.go") {
			continue
		}

		parsed, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == name {
				return fn
			}
		}
	}

	t.Fatalf("%s is not declared in this package", name)

	return nil
}

// calleeName is the bare function or method name a call invokes.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}
