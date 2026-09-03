package state

import (
	"errors"
	"strings"
	"testing"
)

// A CLAIM BINDS THE LEDGER TO THE DEPLOYMENT THAT CLAIMED IT.
//
// Two facts recorded by one transaction, because becoming the controller and
// recording whose rows these are is one decision. A ledger carrying a generation
// of a controller whose identity it does not name is a state nothing could act
// on.
func TestClaimingBindsTheLedgerToItsDeployment(t *testing.T) {
	db := open(t)

	if _, err := db.ClaimController(t.Context(), "host-a", testDeployment); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	bound, err := db.DeploymentBinding(t.Context())
	if err != nil {
		t.Fatalf("DeploymentBinding: %v", err)
	}

	if bound != testDeployment {
		t.Errorf("the ledger is bound to %q, want %q", bound, testDeployment)
	}
}

// AN UNBOUND LEDGER IS AN ORDINARY STATE.
//
// Every deployment upgrading through the release that adds migration 45 has one,
// and so does a fresh ledger nothing has claimed. Reporting that as an error
// would fail `billet status` on exactly the deployment an operator is setting
// up, which is the same reasoning ControllerHolder follows one file over.
func TestAnUnboundLedgerReportsNoDeployment(t *testing.T) {
	db := open(t)

	bound, err := db.DeploymentBinding(t.Context())
	if err != nil {
		t.Fatalf("DeploymentBinding on a fresh ledger: %v", err)
	}

	if bound != "" {
		t.Errorf("a fresh ledger reports %q, want no binding", bound)
	}
}

// A LEDGER REFUSES A CONTROLLER CARRYING ANOTHER DEPLOYMENT'S IDENTITY.
//
// THE FAILURE IT PREVENTS is two hosts, one DSN and two identity directories:
// each control plane admits nodes against its own authority while both charge
// capacity into one ledger, and nothing anywhere names the disagreement. The
// first thing anybody notices is a fleet that will not connect, on the day a
// failover was supposed to save them.
func TestALedgerRefusesAControllerCarryingAnotherDeploymentsIdentity(t *testing.T) {
	db := open(t)

	if _, err := db.ClaimController(t.Context(), "host-a", testDeployment); err != nil {
		t.Fatalf("the first controller could not claim: %v", err)
	}

	_, err := db.ClaimController(t.Context(), "host-b", "a-different-deployment")
	if !errors.Is(err, ErrForeignLedger) {
		t.Fatalf("a controller claimed a ledger bound to another deployment; got %v", err)
	}

	// AND IT NAMES BOTH, because billet cannot tell which half is wrong — an
	// identity directory restored from the wrong backup, or a DSN pointing at
	// another deployment's schema — and an operator who is told only one of them
	// has to guess at the other.
	for _, want := range []string{testDeployment, "a-different-deployment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q; got: %v", want, err)
		}
	}
}

// AND THE REFUSED CLAIM DOES NOT MOVE THE EPOCH.
//
// WHAT THIS PINS IS ATOMICITY, NOT THE ORDER OF TWO STATEMENTS. Both happen
// inside one transaction, so either both land or neither does; reordering them
// within it changes nothing and this test would not notice, which is said out
// loud rather than left to be discovered.
//
// WHAT IT DOES CATCH is the split that reads as an obvious tidy-up: with the
// binding in a second transaction after the claim, a process pointed at another
// deployment's rows advances the epoch, is then refused, and has fenced the REAL
// controller out of its own ledger on the way past. Measured by making that
// change — the incumbent's next write came back "host-b now holds it at epoch
// 2" — so a misconfiguration that should have changed nothing takes the
// deployment down instead.
func TestARefusedClaimLeavesTheEpochWhereItWas(t *testing.T) {
	db := open(t)

	first, err := db.ClaimController(t.Context(), "host-a", testDeployment)
	if err != nil {
		t.Fatalf("the first controller could not claim: %v", err)
	}

	if _, err := db.ClaimController(
		t.Context(), "host-b", "a-different-deployment"); !errors.Is(err, ErrForeignLedger) {
		t.Fatalf("expected ErrForeignLedger; got %v", err)
	}

	held, err := db.ControllerHolder(t.Context())
	if err != nil {
		t.Fatalf("ControllerHolder: %v", err)
	}

	if held.Epoch != first.Epoch || held.Holder != "host-a" {
		t.Errorf("a refused claim left the ledger at %q epoch %d, want host-a epoch %d",
			held.Holder, held.Epoch, first.Epoch)
	}
}

// THE BINDING IS WRITTEN ONCE AND NEVER REPLACED.
//
// A rebind is not offered, and its absence is the guarantee rather than an
// omission: the operation it would serve is relabelling a deployment, which
// billet refuses everywhere else it can arise. Re-claiming under the SAME
// identity must still work, because that is every ordinary restart.
func TestTheBindingIsWrittenOnceAndNeverReplaced(t *testing.T) {
	db := open(t)

	for _, holder := range []string{"host-a", "host-a-again", "host-a-once-more"} {
		if _, err := db.ClaimController(t.Context(), holder, testDeployment); err != nil {
			t.Fatalf("re-claiming under the same identity as %s: %v", holder, err)
		}
	}

	bound, err := db.DeploymentBinding(t.Context())
	if err != nil {
		t.Fatalf("DeploymentBinding: %v", err)
	}

	if bound != testDeployment {
		t.Errorf("the ledger is bound to %q after three claims, want %q", bound, testDeployment)
	}
}

// AN EMPTY IDENTITY IS REFUSED RATHER THAN BOUND.
//
// A caller that does not know which deployment it is would otherwise write a
// label nothing can ever match — the column's CHECK refuses it anyway, and this
// says why before the insert rather than after it.
func TestClaimingWithNoIdentityIsRefused(t *testing.T) {
	db := open(t)

	if _, err := db.ClaimController(t.Context(), "host-a", ""); err == nil {
		t.Fatal("a controller claimed without naming its deployment")
	}
}

// THE READ-ONLY VERIFICATION ANSWERS THE SAME QUESTION AND WRITES NOTHING.
//
// It is what every operator command asks, because a command binds nothing but
// pointing one at another deployment's ledger is exactly as wrong as pointing a
// control plane at them.
func TestVerifyingABindingRefusesAStranger(t *testing.T) {
	db := open(t)

	// AN UNBOUND LEDGER ANSWERS YES, which is what every deployment upgrading
	// through this release looks like.
	if err := db.VerifyDeploymentBinding(t.Context(), testDeployment); err != nil {
		t.Fatalf("an unbound ledger refused a verification: %v", err)
	}

	if _, err := db.ClaimController(t.Context(), "host-a", testDeployment); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if err := db.VerifyDeploymentBinding(t.Context(), testDeployment); err != nil {
		t.Errorf("the deployment that bound this ledger was refused: %v", err)
	}

	if err := db.VerifyDeploymentBinding(
		t.Context(), "a-different-deployment"); !errors.Is(err, ErrForeignLedger) {
		t.Errorf("a stranger verified against a bound ledger; got %v", err)
	}

	// AND AN UNKNOWN IDENTITY IS NOT A MISMATCH. A host being prepared has no
	// identity file yet, and refusing there would break `billet check` on exactly
	// the deployment it exists to prepare.
	if err := db.VerifyDeploymentBinding(t.Context(), ""); err != nil {
		t.Errorf("a caller with no identity was refused: %v", err)
	}
}
