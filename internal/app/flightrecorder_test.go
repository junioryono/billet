package app

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/flightrecorder"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// becomeTestController opens a control plane over a fresh SQLite ledger and
// takes its claim, with the flight recorder on or off.
func becomeTestController(t *testing.T, recorder bool) (*Controller, string) {
	t.Helper()

	identity := ledgertest.Dir(t)

	cfg := &config.Config{Server: &config.ServerConfig{
		Listen:         "127.0.0.1:0",
		IdentityDir:    identity,
		MaxVCPU:        8,
		MaxMemory:      16 * config.GiB,
		FlightRecorder: recorder,
	}}

	host := Host{
		ServerAccess:  noIdentityAccess,
		AuthorityLock: noIdentityAccess,
		Ready:         func() error { return nil },
		Status:        func(string) error { return nil },
		Out:           io.Discard,
	}

	cp, err := OpenControlPlane(t.Context(), cfg, host, nil, ControlPlaneOptions{})
	if err != nil {
		t.Fatalf("OpenControlPlane: %v", err)
	}

	t.Cleanup(func() { _ = cp.Close() })

	ctl, err := cp.BecomeController(t.Context(), func() {})
	if err != nil {
		t.Fatalf("BecomeController: %v", err)
	}

	return ctl, filepath.Join(identity, flightrecorder.DirName)
}

// THE FLIGHT RECORDER IS OFF UNLESS ASKED FOR: a controller without the key
// holds none and writes nothing.
func TestAControllerRecordsNothingByDefault(t *testing.T) {
	t.Parallel()

	ctl, dir := becomeTestController(t, false)

	if ctl.recorder != nil {
		t.Error("a controller without server.flight_recorder holds a flight recorder")
	}

	ctl.heartbeatOverrun()
	ctl.Close()

	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("a controller without server.flight_recorder made %s (%v)", dir, err)
	}
}

// WITH THE KEY, THE CONTROLLER RECORDS, and both of its reasons reach a file
// under the identity directory before Close returns: an overrunning heartbeat
// pass as the listeners report it, and a lost claim as the fence reports it.
//
// NOT PARALLEL: the runtime allows one flight recorder per process, and the
// tests in this package that start one run one at a time.
func TestAControllerRecordsAnOverrunAndALostClaim(t *testing.T) {
	ctl, dir := becomeTestController(t, true)

	if ctl.recorder == nil {
		t.Fatal("a controller with server.flight_recorder holds no flight recorder")
	}

	ctl.heartbeatOverrun()

	replaced := make(chan struct{})
	close(replaced)

	stopped := false
	stopWhenReplaced(t.Context(), replaced, func() { stopped = true }, ctl.recorder, slog.New(slog.DiscardHandler))

	if !stopped {
		t.Error("a replaced controller did not stop")
	}

	ctl.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var reasons []string

	for _, e := range entries {
		name := e.Name()

		switch {
		case strings.HasSuffix(name, "-"+string(flightrecorder.HeartbeatOverrun)+".trace"):
			reasons = append(reasons, string(flightrecorder.HeartbeatOverrun))
		case strings.HasSuffix(name, "-"+string(flightrecorder.LeadershipLost)+".trace"):
			reasons = append(reasons, string(flightrecorder.LeadershipLost))
		default:
			t.Errorf("an unexpected entry %q", name)
		}
	}

	if len(reasons) != 2 || reasons[0] == reasons[1] {
		t.Errorf("the recorder wrote %q, want one snapshot for each reason", reasons)
	}
}

// A LOST CLAIM THE FENCE'S WATCHER MISSED IS RECORDED AS THE RECORDER IS
// SETTLED: the watcher can see the plane's context end first and record
// nothing, and Close settles the recorder with the ledger's latched fact once
// the loops are joined.
//
// NOT PARALLEL, for the reason the test above gives.
func TestALostClaimTheWatcherMissedIsRecordedWhenSettled(t *testing.T) {
	ctl, dir := becomeTestController(t, true)

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	stopWhenReplaced(ended, make(chan struct{}), func() {
		t.Error("a watcher whose context ended stopped the process")
	}, ctl.recorder, slog.New(slog.DiscardHandler))

	settleRecorder(ctl.recorder, true)
	ctl.Close()

	requireOnlySnapshot(t, dir, flightrecorder.LeadershipLost)
}

// A REAL LOST CLAIM IS RECORDED BY CLOSE, through the ledger this controller
// writes, when the fence's watcher is gone: the loops are joined first, which
// ends the watcher with nothing to see; then a successor's claim is staged from
// a second handle on the same ledger and the controller's next write is
// refused. Only Close's settling can have written the snapshot.
//
// NOT PARALLEL, for the reason the test above gives.
func TestALostClaimIsRecordedByCloseOnceTheWatcherIsGone(t *testing.T) {
	ctl, dir := becomeTestController(t, true)

	// Wait is idempotent, so Close's own Wait returns what this one did.
	if errs := ctl.loops.Wait(); len(errs) != 0 {
		t.Fatalf("the controller's loops: %v", errs)
	}

	successor, err := state.OpenAdmin(t.Context(), filepath.Dir(dir))
	if err != nil {
		t.Fatalf("open a second handle on the ledger: %v", err)
	}

	t.Cleanup(func() { _ = successor.Close() })

	ledgertest.StageSuccessor(t, successor, "successor")

	if _, err := ctl.ForgetFleet(t.Context()); !errors.Is(err, state.ErrLeadershipLost) {
		t.Fatalf("the replaced controller's write returned %v, want ErrLeadershipLost", err)
	}

	ctl.Close()

	requireOnlySnapshot(t, dir, flightrecorder.LeadershipLost)
}

// requireOnlySnapshot fails unless dir holds exactly one snapshot, for reason.
func requireOnlySnapshot(t *testing.T, dir string, reason flightrecorder.Reason) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "-"+string(reason)+".trace") {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		t.Errorf("the recorder left %q, want one %s snapshot", names, reason)
	}
}

// THE RECORDER IS WIRED WHERE IT IS ASKED: Schedule hands the listeners this
// controller's heartbeatOverrun, and BecomeController hands the fence this
// controller's recorder. A structural test, because the tests above call both
// directly and neither would notice the wire being cut; the control plane that
// would is one polling GitHub.
func TestTheFlightRecorderIsWiredToItsTwoReasons(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "controlplane.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var overrun, fence bool

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			switch {
			case isMethod(fn, "Controller", "Schedule") && isSelectorCall(call, "server", "WithHeartbeatOverrun"):
				overrun = len(call.Args) == 1 && isSelector(call.Args[0], "c", "heartbeatOverrun")
			case isMethod(fn, "ControlPlane", "BecomeController") && isIdentCall(call, "stopWhenReplaced"):
				fence = len(call.Args) == 5 && isSelector(call.Args[3], "ctl", "recorder")
			}

			return true
		})
	}

	if !overrun {
		t.Error("Controller.Schedule does not pass server.WithHeartbeatOverrun(c.heartbeatOverrun): " +
			"a heartbeat pass that overruns would leave no trace")
	}

	if !fence {
		t.Error("BecomeController does not hand stopWhenReplaced ctl.recorder: a lost claim would leave no trace")
	}
}

func isSelectorCall(call *ast.CallExpr, pkg, name string) bool {
	return isSelector(call.Fun, pkg, name)
}

func isIdentCall(call *ast.CallExpr, name string) bool {
	ident, ok := call.Fun.(*ast.Ident)

	return ok && ident.Name == name
}

func isSelector(e ast.Expr, x, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}

	ident, ok := sel.X.(*ast.Ident)

	return ok && ident.Name == x
}
