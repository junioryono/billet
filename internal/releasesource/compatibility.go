package releasesource

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrIncompatible means a candidate release must not replace this build.
//
// A SENTINEL SO THE ROLLOUT CAN TELL IT FROM A FAILURE TO LOOK. "This release
// cannot be installed here" is a durable verdict a rollout records as a blocker
// and stops on; "the manifest could not be fetched" is a retry. Collapsing the
// two makes a network blip look like an incompatible release and a genuinely
// incompatible release look like something worth retrying forever.
var ErrIncompatible = errors.New("releasesource: this release cannot replace the running build")

// Current is what the running deployment is, for the compatibility check.
//
// PASSED IN RATHER THAN READ HERE, and that is what makes this package testable
// and honest. Reading nodeapi and internal/state directly would make the check
// assert facts about the process running it, so a test could only ever confirm
// that this build is compatible with itself — which is the one case that never
// fails in production.
type Current struct {
	// Version is the release the running binary reports itself as.
	Version string
	// Wire is the node-wire range the running control plane speaks.
	Wire Range
	// LedgerSchema is the highest migration the running binary knows.
	LedgerSchema int
	// GuestContract is the guest protocol the running binary speaks.
	GuestContract string
	// OS and Arch are the platform this deployment runs on.
	OS   string
	Arch string
}

// Host describes the machine this process is on.
func Host(version string, wire Range, ledgerSchema int, guestContract string) Current {
	return Current{
		Version:       version,
		Wire:          wire,
		LedgerSchema:  ledgerSchema,
		GuestContract: guestContract,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
	}
}

// Compatibility reports every reason a candidate must not be installed here, and
// separately every reason it needs work first.
//
// TWO RETURN VALUES BECAUSE ONE WAS A DEFECT. This used to join everything into
// a single error and let callers pick the guest-contract change out with
// errors.As — which meant a candidate that ALSO shared no wire version, or
// published nothing for this platform, was waved through by a caller that found
// the change and proceeded. A warning that can hide a refusal is worse than no
// warning, and the type is what stops it: a fatal problem can only be returned
// as an error, and an error is not something a caller can mistake for advice.
//
// IT RUNS BEFORE ANY LIVE MUTATION. The whole point of carrying these facts in a signed manifest is
// that a rollout can learn them while the control plane is still running: a
// candidate that turns out to speak no wire version in common with the fleet, or
// to refuse the ledger it would inherit, is discovered after the switch — with
// the old binary already hidden and the services already stopped.
//
// EVERYTHING AT ONCE. An operator planning an upgrade should see all of it in one
// diagnostic rather than clearing one obstacle per attempt, each of which costs a
// maintenance window.
func Compatibility(m *Manifest, current Current) ([]error, error) {
	var (
		problems []error
		warnings []error
	)

	report := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	// THE PLATFORM FIRST, because a manifest that publishes nothing for this
	// machine makes every other question moot — there are no bytes to have an
	// opinion about.
	if _, err := m.Select(current.OS, current.Arch, KindArchive); err != nil {
		report("%s publishes nothing this machine can run (%s/%s)",
			m.Version, current.OS, current.Arch)
	}

	// THE WIRE RANGES MUST OVERLAP, and this is the check that stops a rollout
	// stranding a fleet. A rollout is server-first: the control plane is replaced
	// and nodes converge onto it one at a time, so the candidate has to speak a
	// version the nodes already in the field still speak. With no overlap every
	// node's registration is refused the moment the new binary starts, and
	// ErrRefused is not something a node retries — the fleet is off until every
	// host is rebuilt by hand.
	//
	// COMPARED AGAINST THE RUNNING BUILD'S RANGE rather than against the fleet's
	// recorded versions, because this is the question "could this release serve
	// what this one serves". Whether every individual host has converged is a
	// different question, and `billet status` under `protocol` is what answers it.
	if !overlaps(m.Wire, current.Wire) {
		report("%s speaks node wire %d-%d and this build speaks %d-%d, which share no "+
			"version. Every node would be refused the moment it started, and a refused "+
			"registration is not something a node retries",
			m.Version, m.Wire.Min, m.Wire.Max, current.Wire.Min, current.Wire.Max)
	}

	// ZERO ON THE INCUMBENT SIDE MEANS "COULD NOT TELL", AND IT IS REFUSED RATHER
	// THAN READ AS A SCHEMA.
	//
	// Validate already refuses a manifest that names no ledger schema. This side
	// was not checked, and zero here is the worst direction available: nothing is
	// less than zero, so the comparison below never fires and a running build that
	// cannot say what schema it knows authorises EVERY release — including one
	// behind the ledger actually installed, which is the outage that comparison
	// exists to prevent, reached through it.
	//
	// IT REPORTS THE FACT AND NOT A CAUSE. Current is handed in by a caller, so
	// this function knows the number and nothing about where it came from —
	// state.LatestSchemaVersion() returning zero for an unreadable migration set is
	// one way to get here and not the only one, and naming it would be this package
	// asserting something it cannot see. The caller knows; this does not.
	if current.LedgerSchema <= 0 {
		report("this build reports ledger schema %d, which is not a schema, so nothing can "+
			"decide whether %s could open the database it would inherit",
			current.LedgerSchema, m.Version)
	}

	// A CANDIDATE MAY MIGRATE FORWARD AND MAY NOT GO BACK. Migrations are
	// append-only and a binary refuses a database carrying a version it has never
	// heard of, so a release expecting an OLDER schema cannot open the ledger it
	// would inherit — it starts, refuses, and the control plane is down with a
	// database no installed binary can read.
	//
	// THIS IS THE CHECK THAT MAKES ROLLBACK HONEST. It is the same comparison a
	// rollback makes in the other direction, which is why the manifest carries
	// rollback_to instead of the updater assuming the previous tag.
	if m.LedgerSchema < current.LedgerSchema {
		report("%s expects ledger schema %d and this deployment is at %d. Migrations are "+
			"append-only, so that binary would refuse the database it inherits and the "+
			"control plane would not start",
			m.Version, m.LedgerSchema, current.LedgerSchema)
	}

	// THE GUEST CONTRACT IS COMPARED FOR EQUALITY, never ordered. A newer contract
	// is not backward compatible by default; treating "greater" as acceptable
	// turns a refusal an operator can act on into guests that boot and never
	// report, one job at a time.
	//
	// REPORTED RATHER THAN REFUSED HERE, and that split is deliberate. A control
	// plane does not boot guests, so a contract change does not stop IT starting —
	// what it changes is which images the nodes must have, which
	// `billet images compatible` answers per node against that node's own
	// configured images. Refusing the whole rollout here would block a deployment
	// whose nodes have already imported a compatible generation.
	if m.GuestContract != current.GuestContract {
		warnings = append(warnings, &GuestContractChange{
			From: current.GuestContract,
			To:   m.GuestContract,
		})
	}

	if len(problems) == 0 {
		return warnings, nil
	}

	// THE WARNINGS COME BACK EVEN WITH A REFUSAL, because an operator fixing the
	// refusal will meet the contract change next and should hear about both now
	// rather than one per attempt.
	return warnings, fmt.Errorf("%w: %w", ErrIncompatible, errors.Join(problems...))
}

// GuestContractChange is a candidate that needs different guest images.
//
// A WARNING RATHER THAN A REFUSAL. Every other problem Compatibility reports is a
// reason the binary cannot be installed at all; this one is a reason each NODE
// needs an image before it converges, which `billet images compatible` answers
// per node against that node's own configured images. Refusing the whole rollout
// would block a deployment whose nodes have already imported a compatible
// generation.
//
// IT IS RETURNED IN A SEPARATE LIST, not mixed into the error. Callers used to
// pick it out with errors.As and proceed, which waved through a candidate that
// also shared no wire version — the warning hiding a refusal.
type GuestContractChange struct {
	From string
	To   string
}

func (c *GuestContractChange) Error() string {
	return fmt.Sprintf("this release boots guest contract %s and this deployment runs %s; "+
		"every firecracker node needs a compatible image generation before it can "+
		"converge (`billet images compatible`)", c.To, c.From)
}

// CanRollBack reports whether a failed update TO `from` can restore `to`.
//
// THE NAMES READ THE WAY THE ROLLBACK MOVES: out of `from` and back to `to`, which
// is what the diagnostic says and what the comparison does. Its doc comment used
// to have the two the other way round, which made a reader believe the candidate
// was the second argument.
//
// THE SAME COMPARISON IN THE OTHER DIRECTION, and it exists because a rollback is
// where a schema migration becomes irreversible. If the candidate migrated the
// ledger past what the previous release knows, restoring that binary produces a
// control plane that refuses its own database — so an updater has to know this
// BEFORE it migrates, not after its candidate has failed.
//
// A ROLLBACK IS AUTHORISED BY THE SNAPSHOT, NOT BY THIS. The transactional
// updater takes a ledger snapshot before it migrates and restores that on
// failure, so the schema comparison here is what decides whether the candidate
// may migrate at all while still leaving a way back.
func CanRollBack(from, to *Manifest) error {
	if from.LedgerSchema > to.LedgerSchema {
		return fmt.Errorf("%w: rolling %s back to %s would leave ledger schema %d in front "+
			"of a binary that knows %d",
			ErrIncompatible, from.Version, to.Version, from.LedgerSchema, to.LedgerSchema)
	}

	if !overlaps(from.Wire, to.Wire) {
		return fmt.Errorf("%w: %s and %s share no node wire version, so a rollback would "+
			"refuse every node that had already converged",
			ErrIncompatible, from.Version, to.Version)
	}

	return nil
}

// overlaps reports whether two inclusive ranges share a version.
func overlaps(a, b Range) bool {
	return max(a.Min, b.Min) <= min(a.Max, b.Max)
}
