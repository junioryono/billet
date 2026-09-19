package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"testing"
)

// claimCancellation is the error the rehearsal actually produced, wrap for
// wrap: AwaitController's cancellation branch under becomeController's wrap.
func claimCancellation() error {
	return fmt.Errorf("controller claim: %w",
		fmt.Errorf("state: waiting for the controller claim: %w", context.Canceled))
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
// Restart=on-failure is why this survived so long. An operator stopping a
// standby by hand saw it come back and stand by again, so the exit status was
// invisible until something read the unit's state afterwards.
func TestAControlPlaneStoppedWhileItTakesTheClaimEndsCleanly(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	if err := stoppedBeforeTheClaim(cancelled, false, claimCancellation()); err != nil {
		t.Errorf("a control plane asked to stop while it took the claim returns %v, so "+
			"it exits non-zero, systemd holds a failed unit, and `billet server retire` "+
			"refuses to act against it", err)
	}

	// A REAL FAULT IS STILL A FAULT. The ledger names another deployment, or the
	// schema is unreadable: neither resolves by waiting and neither may exit 0.
	fault := errors.New("ledger bound to another deployment")
	if err := stoppedBeforeTheClaim(cancelled, false, fault); !errors.Is(err, fault) {
		t.Errorf("a claim that failed for its own reason returns %v, want the fault", err)
	}

	// A CANCELLATION IS NOT BY ITSELF A SHUTDOWN. An inner context cancelled
	// while this one is live is a failure whose error happens to carry
	// context.Canceled, and exiting 0 on it would be could-not-tell read as yes.
	if err := stoppedBeforeTheClaim(t.Context(), false, claimCancellation()); err == nil {
		t.Error("a cancellation reaching a process nobody asked to stop is read as a " +
			"stop, so a real fault exits 0")
	}

	// AND A FENCED HOST EXITS NON-ZERO WHATEVER ELSE IS TRUE. Below the claim a
	// clean exit leaves a healed deployment with no controller at all, which is
	// the failure the whole fence exists to make impossible.
	if err := stoppedBeforeTheClaim(cancelled, true, claimCancellation()); err == nil {
		t.Error("a host whose claim was refused by a successor reports a clean stop, so " +
			"systemd leaves it down and the deployment keeps no controller")
	}

	if err := stoppedBeforeTheClaim(cancelled, false, nil); err != nil {
		t.Errorf("a claim that succeeded returns %v", err)
	}
}

// AND runServer HAS TO ACT ON THE ANSWER, which nothing above can see: the
// classification is worth nothing if the call site returns the error anyway.
// The hazard is an edit that keeps the call and drops the branch, and it leaves
// no trace at run time without a packaged pair and a real stop.
func TestRunServerReturnsNothingWhenTheClaimWasStopped(t *testing.T) {
	t.Parallel()

	fn := findFunc(t, "runServer")

	var branch *ast.IfStmt

	ast.Inspect(fn, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || branch != nil {
			return true
		}

		ast.Inspect(stmt.Init, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == "becomeController" {
				branch = stmt
			}

			return true
		})

		return true
	})

	if branch == nil {
		t.Fatal("runServer no longer takes this deployment's controller claim in an " +
			"`if err := becomeController(...); err != nil` of its own")
	}

	var classified, cleanReturn bool

	ast.Inspect(branch.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if calleeName(node) == "stoppedBeforeTheClaim" {
				classified = true
			}
		case *ast.ReturnStmt:
			// EXACTLY `return nil`, which is the whole behaviour: a branch that
			// classifies and then returns the error anyway is the bug again.
			if len(node.Results) == 1 {
				if id, ok := node.Results[0].(*ast.Ident); ok && id.Name == "nil" {
					cleanReturn = true
				}
			}
		}

		return true
	})

	if !classified {
		t.Error("runServer no longer asks stoppedBeforeTheClaim about a failed claim, so " +
			"a control plane asked to stop exits 1 again and every retirement of one " +
			"refuses at intent")
	}

	if !cleanReturn {
		t.Error("runServer never returns nil from the failed-claim branch, so " +
			"classifying the stop changes nothing about how the process exits")
	}
}
