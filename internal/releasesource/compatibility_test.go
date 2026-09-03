package releasesource

import (
	"errors"
	"strings"
	"testing"
)

// running is a deployment the `good()` manifest is compatible with.
func running() Current {
	return Current{
		Version:       "v0.3.26",
		Wire:          Range{Min: 12, Max: 13},
		LedgerSchema:  34,
		GuestContract: "billet-guest-1",
		OS:            "linux",
		Arch:          "amd64",
	}
}

func TestACompatibleReleaseIsAccepted(t *testing.T) {
	t.Parallel()

	warnings, err := Compatibility(good(), running())
	if err != nil {
		t.Fatalf("a compatible release was refused: %v", err)
	}

	if len(warnings) != 0 {
		t.Errorf("a compatible release produced warnings: %v", warnings)
	}
}

// A CANDIDATE THAT SHARES NO WIRE VERSION WOULD STRAND THE FLEET.
//
// A rollout is server-first: the control plane is replaced and nodes converge
// onto it one at a time, so the candidate must speak a version the nodes in the
// field still speak. With no overlap every registration is refused the moment the
// new binary starts, and ErrRefused is not something a node retries — the fleet
// is off until every host is rebuilt by hand.
func TestAReleaseSharingNoWireVersionIsRefused(t *testing.T) {
	t.Parallel()

	m := good()
	m.Wire = Range{Min: 20, Max: 22}

	_, err := Compatibility(m, running())
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Compatibility: %v, want ErrIncompatible", err)
	}

	if !strings.Contains(err.Error(), "share no version") {
		t.Errorf("the refusal does not name the overlap: %v", err)
	}

	// AND IT SAYS WHY IT CANNOT BE RETRIED, because an operator whose fleet has
	// fallen off will otherwise restart nodes hoping they reconnect.
	if !strings.Contains(err.Error(), "not something a node retries") {
		t.Errorf("the refusal does not say a refused registration is terminal: %v", err)
	}
}

// A RANGE THAT MERELY TOUCHES IS ENOUGH. The bridge exists precisely so one
// shared version carries a rollout.
func TestASingleSharedWireVersionIsEnough(t *testing.T) {
	t.Parallel()

	m := good()
	m.Wire = Range{Min: 13, Max: 15}

	current := running()
	current.Wire = Range{Min: 11, Max: 13}

	if _, err := Compatibility(m, current); err != nil {
		t.Fatalf("one shared wire version was refused: %v", err)
	}
}

// A CANDIDATE EXPECTING AN OLDER LEDGER CANNOT OPEN THE DATABASE IT INHERITS.
//
// Migrations are append-only and a binary refuses a version it has never heard
// of, so this is the difference between a refusal an operator reads and a control
// plane that will not start with a database no installed binary can read.
func TestAReleaseBehindTheLedgerIsRefused(t *testing.T) {
	t.Parallel()

	m := good()
	m.LedgerSchema = 30

	_, err := Compatibility(m, running())
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Compatibility: %v, want ErrIncompatible", err)
	}

	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("the refusal does not explain why it cannot go back: %v", err)
	}
}

// A BUILD REPORTING A NON-SCHEMA MAY NOT VOUCH FOR ANOTHER ONE.
//
// Zero is what state.LatestSchemaVersion() returns when the embedded migration
// set could not be read, and it is the worst direction for this comparison to
// fail in: `m.LedgerSchema < 0` is false for every candidate, so an unguarded
// zero waves through a release BEHIND the ledger actually installed — the outage
// this comparison exists to prevent, reached through the comparison. Validate
// already refuses zero on the manifest side; this is the incumbent side.
//
// NEGATIVE IS TESTED TOO, and it is not a value LatestSchemaVersion produces.
// Current is a struct any caller can fill in, so the guard is written as "not a
// schema" rather than "zero" — and the diagnostic names only that, because this
// package is handed the number and cannot see where it came from.
func TestABuildThatCannotNameItsLedgerSchemaRefusesEveryCandidate(t *testing.T) {
	t.Parallel()

	for _, schema := range []int{0, -1} {
		current := running()
		current.LedgerSchema = schema

		// A candidate that would otherwise be perfectly acceptable, and one that
		// is behind — neither may be authorised by a build that cannot tell.
		for _, ledger := range []int{good().LedgerSchema, 1} {
			m := good()
			m.LedgerSchema = ledger

			_, err := Compatibility(m, current)
			if !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Compatibility with a running schema of %d and a candidate at %d: "+
					"%v, want ErrIncompatible", schema, ledger, err)
			}

			if !strings.Contains(err.Error(), "which is not a schema") {
				t.Errorf("the refusal does not name the running build as the problem: %v", err)
			}
		}
	}
}

// A RELEASE PUBLISHING NOTHING FOR THIS MACHINE IS REFUSED FIRST, because there
// are no bytes for any other question to be about.
func TestAReleaseWithNothingForThisPlatformIsRefused(t *testing.T) {
	t.Parallel()

	current := running()
	current.OS = "darwin"
	current.Arch = "arm64"

	_, err := Compatibility(good(), current)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Compatibility: %v, want ErrIncompatible", err)
	}

	if !strings.Contains(err.Error(), "darwin/arm64") {
		t.Errorf("the refusal does not name the platform: %v", err)
	}
}

// A GUEST CONTRACT CHANGE IS WORK TO SCHEDULE, NOT A BLOCKER.
//
// A control plane does not boot guests, so a contract change does not stop it
// starting. What it changes is which images each node must have — which is a
// per-node question `billet images compatible` answers against that node's own
// configured images. Refusing the whole rollout would block a deployment whose
// nodes have already imported a compatible generation.
func TestAGuestContractChangeIsReportedAsItsOwnKind(t *testing.T) {
	t.Parallel()

	m := good()
	m.GuestContract = "billet-guest-2"

	warnings, err := Compatibility(m, running())

	// A WARNING, NOT A REFUSAL. A control plane does not boot guests, so a contract
	// change does not stop it starting.
	if err != nil {
		t.Fatalf("a guest contract change was returned as a refusal: %v", err)
	}

	if len(warnings) != 1 {
		t.Fatalf("Compatibility returned %d warning(s), want 1: %v", len(warnings), warnings)
	}

	change, ok := errors.AsType[*GuestContractChange](warnings[0])
	if !ok {
		t.Fatalf("the warning is not a GuestContractChange: %v", warnings[0])
	}

	if change.From != "billet-guest-1" || change.To != "billet-guest-2" {
		t.Errorf("the change reads %s -> %s", change.From, change.To)
	}

	// AND IT NAMES THE COMMAND, because the operator's next step is per node.
	if !strings.Contains(change.Error(), "billet images compatible") {
		t.Errorf("the report does not name what resolves it: %v", change)
	}
}

// A WARNING MUST NEVER HIDE A REFUSAL, and that it could was a P1 a review found.
//
// Compatibility used to join everything into one error and let callers pick the
// guest-contract change out with errors.As — so a candidate that ALSO shared no
// wire version was waved through by a caller that found the change and carried
// on. The two return values make that unrepresentable: a fatal problem can only
// come back as an error.
func TestAGuestContractChangeDoesNotMaskAFatalProblem(t *testing.T) {
	t.Parallel()

	m := good()
	m.GuestContract = "billet-guest-2"
	m.Wire = Range{Min: 20, Max: 22}

	warnings, err := Compatibility(m, running())

	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("a release that shares no wire version was accepted because it also "+
			"changed the guest contract: %v", err)
	}

	// AND THE WARNING STILL COMES BACK, because an operator fixing the refusal
	// meets the contract change next and should hear about both now.
	if len(warnings) != 1 {
		t.Errorf("the warning was dropped alongside the refusal: %v", warnings)
	}
}

// EVERY REASON AT ONCE. An operator planning an upgrade should see all of it in
// one diagnostic rather than clearing one obstacle per maintenance window.
func TestCompatibilityReportsEveryReasonTogether(t *testing.T) {
	t.Parallel()

	m := good()
	m.Wire = Range{Min: 20, Max: 22}
	m.LedgerSchema = 30
	m.GuestContract = "billet-guest-2"

	warnings, err := Compatibility(m, running())
	if err == nil {
		t.Fatal("an incompatible release was accepted")
	}

	for _, want := range []string{"share no version", "append-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic omits %q:\n%v", want, err)
		}
	}

	if len(warnings) != 1 {
		t.Errorf("the guest contract change was not reported beside the refusals: %v",
			warnings)
	}
}

// A ROLLBACK IS WHERE A MIGRATION BECOMES IRREVERSIBLE, and the updater has to
// know that BEFORE it migrates rather than after its candidate has failed.
func TestCanRollBackRefusesALedgerItCannotUndo(t *testing.T) {
	t.Parallel()

	from := good()
	from.LedgerSchema = 40

	to := good()
	to.Version = "v0.3.26"
	to.LedgerSchema = 34

	err := CanRollBack(from, to)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("CanRollBack: %v, want ErrIncompatible", err)
	}

	if !strings.Contains(err.Error(), "in front of a binary that knows") {
		t.Errorf("the refusal does not name the schema gap: %v", err)
	}
}

// AND A ROLLBACK THAT WOULD REFUSE THE FLEET IS REFUSED TOO. Nodes that already
// converged onto the candidate's wire cannot register against a binary that does
// not speak it.
func TestCanRollBackRefusesAWireGap(t *testing.T) {
	t.Parallel()

	from := good()
	from.Wire = Range{Min: 14, Max: 16}

	to := good()
	to.Version = "v0.3.26"
	to.Wire = Range{Min: 11, Max: 13}

	if err := CanRollBack(from, to); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("CanRollBack: %v, want ErrIncompatible", err)
	}
}

// A ROLLBACK THAT CHANGED NEITHER IS ALLOWED, which is the ordinary case: most
// releases migrate nothing and move no wire version.
func TestCanRollBackAcceptsAnUnchangedContract(t *testing.T) {
	t.Parallel()

	from := good()

	to := good()
	to.Version = "v0.3.26"

	if err := CanRollBack(from, to); err != nil {
		t.Errorf("an ordinary rollback was refused: %v", err)
	}
}
