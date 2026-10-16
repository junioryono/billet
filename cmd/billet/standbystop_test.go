package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"io"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// claimCancellation is the error the rehearsal actually produced, wrap for
// wrap: AwaitController's cancellation branch under becomeController's wrap.
func claimCancellation() error {
	return fmt.Errorf("controller claim: %w",
		fmt.Errorf("state: waiting for the controller claim: %w", context.Canceled))
}

// endedWith is what serverEnded printed and what it answered.
func endedWith(ctx context.Context, fenced bool, err error) (string, error) {
	var out bytes.Buffer

	got := serverEnded(ctx, cli.Env{Stdout: &out, Stderr: io.Discard}, fenced, err)

	return out.String(), got
}

// A CONTROL PLANE STOPPED WHILE IT IS STILL TRYING HAS SUCCEEDED, AND EXITING 1
// COSTS A RETIREMENT.
//
// The first real-host run of the retained-node retirement (2026-09-19, run
// 35411568410) refused at intent with `operation-reactivation:
// billet-server.service ActiveState="failed"`. The journal on the retiring host
// said why: `Stopping billet-server.service`, then `controller claim: state:
// waiting for the controller claim: context canceled`, then `Main process
// exited, code=exited, status=1/FAILURE`. The unit had done exactly what it was
// told; AdmitQuietActivation requires a stopped server to be `active` or
// `inactive`, and `failed` is neither.
//
// AND EVERY OTHER WAY A RUN ENDS IS ANSWERED HERE TOO, each the way it was asked
// for: the stop exits 0, and everything that is not only the stop does not.
func TestAControlPlaneEndsAsItWasAskedTo(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	closeFault := errors.New("close the claim's connection: broken pipe")
	moved := &state.SchemaMoved{From: 59, To: 60}

	for _, c := range []struct {
		name   string
		ctx    context.Context
		fenced bool
		err    error
		// want is nil for a clean stop, and otherwise what the returned error
		// must carry.
		want error
	}{
		{"a standby stopped while it waited for the claim", cancelled, false, claimCancellation(), nil},
		{"a scheduler that stopped", cancelled, false, nil, nil},
		{"a cancellation nobody asked for", t.Context(), false, claimCancellation(), context.Canceled},
		{"a fault joined to the stop", cancelled, false, errors.Join(claimCancellation(), closeFault), closeFault},
		{"a schema moved before the stop", cancelled, false, errors.Join(claimCancellation(), moved), moved},
		{"an inner deadline under the stop", cancelled, false,
			fmt.Errorf("state: migrate on promotion: %w", context.DeadlineExceeded), context.DeadlineExceeded},
		{"a fault with nobody stopping", t.Context(), false, closeFault, closeFault},
		{"a fenced host that was stopped", cancelled, true, claimCancellation(), state.ErrLeadershipLost},
		{"a fenced host whose scheduler said so", t.Context(), true,
			fmt.Errorf("%w. Nothing running here was destroyed", state.ErrLeadershipLost), state.ErrLeadershipLost},
		{"a host fenced as it stopped", cancelled, true, nil, state.ErrLeadershipLost},
	} {
		stdout, got := endedWith(c.ctx, c.fenced, c.err)

		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: exits non-zero with %v, so systemd holds a failed unit for a stop it "+
				"was asked for", c.name, got)
		case c.want == nil && !strings.Contains(stdout, "billet server: stopped"):
			t.Errorf("%s: printed %q, not that it stopped", c.name, stdout)
		case c.want != nil && !errors.Is(got, c.want):
			t.Errorf("%s: returned %v, want an error carrying %v", c.name, got, c.want)
		case c.want != nil && strings.Contains(stdout, "stopped"):
			t.Errorf("%s: reported a clean stop for a run that failed: %q", c.name, stdout)
		}
	}

	// A MOVED SCHEMA SAYS FROM WHICH TO WHICH, because the next thing an operator
	// does depends on knowing the ledger moved (#148).
	_, got := endedWith(cancelled, false, errors.Join(claimCancellation(), moved))
	if got == nil || !strings.Contains(got.Error(), "from schema 59 to schema 60") {
		t.Errorf("a stop after a migration does not say what moved: %v", got)
	}
}

// openStopTestController is a control plane over a seeded SQLite ledger, its
// claim taken, for a test that drives one of its steps itself.
func openStopTestController(t *testing.T, cfg *config.Config) (*app.ControlPlane, *app.Controller) {
	t.Helper()

	cp, err := app.OpenControlPlane(t.Context(), cfg, stopTestHost(stopHooks{}), nil, app.ControlPlaneOptions{})
	if err != nil {
		t.Fatalf("OpenControlPlane: %v", err)
	}

	t.Cleanup(func() { _ = cp.Close() })

	ctl, err := cp.BecomeController(t.Context(), func() {})
	if err != nil {
		t.Fatalf("BecomeController: %v", err)
	}

	t.Cleanup(func() { ctl.Close() })

	return cp, ctl
}

// THE FLEET IS FORGOTTEN BY THE FIRST WRITE AFTER THE CLAIM, and no host hook
// fires between the two, so this drives the step itself: what it returns for a
// stop is a stop, and what it returns once a successor has the deployment is
// the fence, whatever else is true.
func TestForgettingTheFleetEndsAsItWasAskedTo(t *testing.T) {
	t.Parallel()

	cfg := stopTestConfig(t)
	cp, ctl := openStopTestController(t, cfg)

	stopped, stop := context.WithCancel(t.Context())
	stop()

	_, err := ctl.ForgetFleet(stopped)
	if err == nil || !strings.Contains(err.Error(), "could not clear the fleet's liveness") {
		t.Fatalf("forgetting the fleet on a stopped context returned %v, so this test stages nothing", err)
	}

	if _, got := endedWith(stopped, cp.LeadershipLost(), err); got != nil {
		t.Errorf("a control plane stopped as it forgot the fleet exits non-zero with %v", got)
	}

	if _, got := endedWith(t.Context(), cp.LeadershipLost(), err); got == nil {
		t.Error("a cancellation of forgetting the fleet with nobody stopping exits 0")
	}

	operator, err := state.OpenAdmin(t.Context(), cfg.Server.IdentityDir)
	if err != nil {
		t.Fatalf("open an operator handle: %v", err)
	}

	ledgertest.StageSuccessor(t, operator, "successor")

	if err := operator.Close(); err != nil {
		t.Fatalf("close the operator handle: %v", err)
	}

	_, err = ctl.ForgetFleet(t.Context())
	if !errors.Is(err, state.ErrLeadershipLost) || !cp.LeadershipLost() {
		t.Fatalf("forgetting the fleet after a successor claimed returned %v (fenced %v)",
			err, cp.LeadershipLost())
	}

	if _, got := endedWith(stopped, cp.LeadershipLost(), err); !errors.Is(got, state.ErrLeadershipLost) {
		t.Errorf("a fenced control plane that was also stopped exits with %v, want ErrLeadershipLost", got)
	}
}

// THE AUTHORITY IS ADOPTED FROM THE IDENTITY STORE, and a stop that lands in
// that read is a stop. The store's credentials come from the environment and
// its request is made on a context that has already ended, which net/http
// answers before it dials, so nothing here reaches AWS; what it proves is that
// the error the store returns for a stopped read still says it was one.
func TestAStopWhileAdoptingTheAuthorityIsAStop(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_SESSION_TOKEN", "")

	cfg := stopTestConfig(t)
	cfg.Server.Identity = &config.IdentityConfig{
		Backend: config.IdentitySSM,
		AWSSSM:  &config.IdentitySSMConfig{Region: "us-west-2", Prefix: "/billet/stop-test"},
	}

	_, ctl := openStopTestController(t, cfg)

	stopped, stop := context.WithCancel(t.Context())
	stop()

	// THE STORE'S OWN READ, so the stop is the one the store reports and not an
	// earlier step's: the authority lock before it takes no wait.
	_, err := ctl.AdoptAuthority(stopped)
	if err == nil || !strings.Contains(err.Error(), "awsssm: read") {
		t.Fatalf("adopting the authority on a stopped context returned %v, so the stop did not "+
			"land in the store's read", err)
	}

	if _, got := endedWith(stopped, false, err); got != nil {
		t.Errorf("a control plane stopped as it adopted the authority exits non-zero with %v", got)
	}

	if _, got := endedWith(t.Context(), false, err); got == nil {
		t.Error("a cancelled adoption with nobody stopping exits 0")
	}
}

// runServer ANSWERS EVERY ENDING THROUGH serverEnded, AND A PROBE'S THROUGH
// NOTHING. Nothing above reaches runServer itself, whose clients and host are
// the real ones; the hazard is an edit that returns runControlPlane's error
// without the classification, or classifies a probe's verdict, and it leaves no
// trace at run time without a packaged host and a real stop.
func TestRunServerAnswersEveryEndingOnce(t *testing.T) {
	t.Parallel()

	fn := findFunc(t, "runServer")
	body := fn.Body.List

	// THE PROCESS'S OWN CONTEXT, the one a signal cancels, reaches both calls:
	// asked of any other, a stop is a fault again.
	if len(fn.Type.Params.List) == 0 || len(fn.Type.Params.List[0].Names) == 0 {
		t.Fatal("runServer takes no context")
	}

	ctxName := fn.Type.Params.List[0].Names[0].Name
	if len(body) != 3 {
		t.Fatalf("runServer has %d statements, want three: run the control plane, return a "+
			"probe's error as it is, return serverEnded's answer", len(body))
	}

	assign, ok := body[0].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
		t.Fatal("runServer does not begin by assigning runControlPlane's two results")
	}

	run, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || calleeName(run) != "runControlPlane" || len(run.Args) == 0 || !isIdent(run.Args[0], ctxName) {
		t.Fatalf("runServer's first statement is not runControlPlane's call on %s", ctxName)
	}

	fenced, okFenced := assign.Lhs[0].(*ast.Ident)
	errName, okErr := assign.Lhs[1].(*ast.Ident)

	if !okFenced || !okErr {
		t.Fatal("runServer does not name runControlPlane's results")
	}

	probe, ok := body[1].(*ast.IfStmt)
	if !ok || !isFlagRef(probe.Cond, "upgradeProbe") || len(probe.Body.List) != 1 {
		t.Fatal("runServer's second statement is not the probe's branch")
	}

	if ret, ok := probe.Body.List[0].(*ast.ReturnStmt); !ok || len(ret.Results) != 1 ||
		!isIdent(ret.Results[0], errName.Name) {
		t.Error("the probe's branch does not return runControlPlane's error as it is, so a " +
			"probe stopped before it proved anything could read as one that passed")
	}

	ret, ok := body[2].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		t.Fatal("runServer's last statement is not a return of one value")
	}

	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok || calleeName(call) != "serverEnded" || len(call.Args) != 4 || !isIdent(call.Args[0], ctxName) ||
		!isIdent(call.Args[2], fenced.Name) || !isIdent(call.Args[3], errName.Name) {
		t.Error("runServer does not return serverEnded's answer for runControlPlane's results, so " +
			"a control plane asked to stop exits 1 again and every retirement of one refuses at intent")
	}
}

// THE FENCE IS READ ONCE THE CONTROL PLANE HAS STOPPED. serveControlPlane stops
// the wire and joins the controller's loops in its deferred calls, so a fence
// read inside it would be read before them and miss a write refused during
// teardown, and one read after runControlPlane closes the ledger would read
// a closed ledger. No hook lands a refusal in teardown, so the place is read
// from the source: the read is runControlPlane's, after the call that
// serves, and serveControlPlane makes none.
func TestTheFenceIsReadOnceTheControlPlaneHasStopped(t *testing.T) {
	t.Parallel()

	calls := func(n ast.Node, name string) bool {
		found := false

		ast.Inspect(n, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == name {
				found = true
			}

			return !found
		})

		return found
	}

	serve := findFunc(t, "serveControlPlane")

	if calls(serve.Body, "LeadershipLost") {
		t.Error("serveControlPlane reads the fence itself, before its deferred calls stop the wire " +
			"and join the controller's loops")
	}

	// AND THOSE CALLS ARE ITS OWN AND SYNCHRONOUS: deferred directly, so they have
	// returned before serveControlPlane has. One moved into a goroutine, or a
	// deferred function that starts one, would let a teardown write land after
	// the read.
	deferred := map[string]bool{}

	for _, stmt := range serve.Body.List {
		def, ok := stmt.(*ast.DeferStmt)
		if !ok {
			continue
		}

		sel, ok := def.Call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}

		if recv, ok := sel.X.(*ast.Ident); ok {
			deferred[recv.Name+"."+sel.Sel.Name] = true
		}
	}

	for _, want := range []string{"wire.Stop", "ctl.Close"} {
		if !deferred[want] {
			t.Errorf("serveControlPlane does not defer %s() directly, so the fence can be read "+
				"before it has returned", want)
		}
	}

	// AND THE LEDGER CLOSES AFTER THE READ, in a statement of its own after it,
	// never in a defer, which could not hand its error back.
	served, read, closed := -1, -1, -1

	for i, stmt := range findFunc(t, "runControlPlane").Body.List {
		if _, isDefer := stmt.(*ast.DeferStmt); isDefer {
			if calls(stmt, "Close") {
				t.Error("runControlPlane defers the ledger's close, so its failure is dropped")
			}

			continue
		}

		if calls(stmt, "serveControlPlane") && served < 0 {
			served = i
		}

		if calls(stmt, "LeadershipLost") && read < 0 {
			read = i
		}

		if ret, ok := stmt.(*ast.ReturnStmt); ok && calls(ret, "Close") {
			closed = i
		}
	}

	if served < 0 || read <= served || closed <= read {
		t.Errorf("runControlPlane does not read the fence after serveControlPlane has returned and "+
			"before the ledger closes (serve at statement %d, read at %d, close at %d)", served, read, closed)
	}
}

// isIdent reports whether e is the identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)

	return ok && id.Name == name
}
