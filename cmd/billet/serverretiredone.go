package main

import (
	"context"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// A `done` JOURNAL IS WHAT EVERY LATER CONVERGE OF A RETIRED HOST FINDS, and
// what it must be able to read WITHOUT the two things a retirement takes away:
// the identity directory at its configured path, and a configuration with a
// server section. Both are read from the journal instead — the identity from
// the archive it recorded, the ledger from the locator it recorded — which is
// the whole reason those members exist.
//
// TWO ANSWERS. A journal that is SETTLED is the steady state: the tail is
// finished, the marker is gone, and nothing here opens a ledger or takes an
// exclusion — it judges what the host holds and says `unchanged`. A journal
// that is NOT settled still owes something (the row, its acknowledgement, the
// marker, `settled`), and this converge finishes it, which is what makes the
// tail survive a crash between any two of its writes and a takeover by a new
// holder.

// retireDoneAnswer is what a converge over a retired host says.
type retireDoneAnswer struct {
	Schema       int                `json:"schema"`
	Outcome      string             `json:"outcome"`
	Phase        retirement.Phase   `json:"phase"`
	Variant      retirement.Variant `json:"variant"`
	TransitionID string             `json:"transition_id"`
	Settled      bool               `json:"settled"`
	RowDone      bool               `json:"row_done"`
	CompletedBy  string             `json:"completed_by"`
	// Postconditions is what the host holds, member by member, in the words
	// the shared table admits.
	Postconditions retirePostconditions `json:"postconditions"`
	State          string               `json:"state"`
}

// retirePostconditions is the completed retirement's contract, as observed.
type retirePostconditions struct {
	Config        string `json:"config"`
	Server        string `json:"server"`
	Node          string `json:"node"`
	UpgradeTimer  string `json:"upgrade_timer"`
	BackupTimer   string `json:"backup_timer"`
	BackupService string `json:"backup_service"`
	Archive       string `json:"archive"`
}

// The words a postcondition is reported in.
const (
	retireUnitQuiet    = "quiesced"
	retireUnitNotFound = "not-found"
	retireUnitRunning  = "running"

	retireConfigPresent = "present"
	retireConfigAbsent  = "absent"

	retireArchivePresent = "present"
)

// retireDoneProperties is what every postcondition asks systemd for. A timer
// has no main process, and an absent answer for it is not a failure.
var retireDoneProperties = []string{"LoadState", "ActiveState", "UnitFileState", "MainPID"}

// retireDone answers a converge that found a `done` journal on this host.
func retireDone(ctx context.Context, m retireMode, root *txLock, dir *os.File, shape claimShape,
	j retirement.Journal,
) (any, *retireRefusal) {
	// THE IDENTITY IS AT THE ARCHIVE, and it is what says this journal
	// describes THIS deployment's retirement: at `done` the configured path
	// holds nothing, so a journal naming an archive that holds no identity is
	// not something to act on.
	identity, r := retireArchivedIdentity(j)
	if r != nil {
		return nil, r
	}

	// AND IT IS THIS CONVERGE'S TRANSITION. A settled journal is past every
	// holder — the marker is gone and there is nothing left to take away — so
	// it is validated by its provenance alone, which is what lets any later
	// converge, under any holder, read it.
	if !j.Settled && !retireDrivesThisJournal(m, shape, j, retirement.JournalFactDone) {
		return nil, atRetirePhase(j, retireUnknown(retireReasonJournal, "this host's retirement is not finished and this "+
			"converge's guard does not carry its marker, so the tail it still owes is not this run's to finish",
			"the runbook in docs/operating/upgrades.md"))
	}

	if err := j.Validate(retirement.JournalExpectation{Retiring: m.retiringHost, Identity: identity}); err != nil {
		return nil, atRetirePhase(j, retireUnknown(retireReasonJournal, err.Error(),
			"the runbook in docs/operating/upgrades.md"))
	}

	if !j.Settled {
		// THE TAIL, FINISHED BY WHOEVER HOLDS THE GUARD NOW. Everything it
		// needs is in the journal, and the run that recorded the intent may be
		// long gone.
		return retireTail(ctx, m, root, dir, j, nil)
	}

	held, r := observeRetirePostconditions(ctx, m, j)
	if r != nil {
		return nil, r
	}

	return &retireDoneAnswer{Schema: retireSchema, Outcome: retireOutcomeUnchanged, Phase: j.Phase,
		Variant: j.Variant, TransitionID: j.Provenance.TransitionID, Settled: true, RowDone: j.RowDone,
		CompletedBy: j.CompletedBy, Postconditions: held}, nil
}

// retireArchivedIdentity reads the deployment identity where a completed
// retirement put it.
func retireArchivedIdentity(j retirement.Journal) (string, *retireRefusal) {
	archive := j.Locator.Archive
	if archive == "" {
		archive = j.Archive
	}

	if archive == "" {
		return "", atRetirePhase(j, retireUnknown(retireReasonJournal,
			"the journal names no archive, so the identity it retired cannot be read", ""))
	}

	identity, ok, err := state.PeekDeploymentID(archive)

	switch {
	case err != nil:
		return "", atRetirePhase(j, retireUnknown(retireReasonIdentity,
			fmt.Sprintf("read the archived identity at %s: %v", archive, err), ""))
	case !ok:
		return "", atRetirePhase(j, retireUnknown(retireReasonIdentity,
			fmt.Sprintf("the archive %s the journal names holds no deployment identity", archive),
			"the runbook in docs/operating/upgrades.md"))
	}

	return identity, nil
}

// observeRetirePostconditions judges what a completed retirement left, and
// refuses naming the member that does not hold.
//
// EVERY CLAUSE IS A CONJUNCTION and none of them is satisfied by a unit whose
// fragment is gone: a unit systemd does not know keeps its runtime facts, and
// "not found" is a positive answer only when the unit is also inactive with no
// process. `masked-runtime` is admitted for nothing, because it hides
// persistent enablement and is gone at the next boot.
func observeRetirePostconditions(ctx context.Context, m retireMode, j retirement.Journal,
) (retirePostconditions, *retireRefusal) {
	var held retirePostconditions

	present, err := retireDirPresent(j.Archive)

	switch {
	case err != nil:
		return held, atRetirePhase(j, retireUnknown(retireReasonIdentity, err.Error(), ""))
	case !present:
		return held, atRetirePhase(j, retireUnknown(retireReasonPostcondition, fmt.Sprintf("the archive %s a completed "+
			"retirement left is not there", j.Archive), "the runbook in docs/operating/upgrades.md"))
	}

	held.Archive = retireArchivePresent

	config, r := retireConfigPostcondition(m.configPath, j)
	if r != nil {
		return held, r
	}

	held.Config = config

	insp := endpointInspector()

	for _, unit := range []struct {
		name  string
		into  *string
		pid   bool
		alive bool
	}{
		{name: serverUnit, into: &held.Server, pid: true},
		{name: nodeUnit, into: &held.Node, pid: true, alive: j.Variant == retirement.VariantRetainedNode},
		{name: upgradeTimerUnit, into: &held.UpgradeTimer},
		{name: backupTimerUnit, into: &held.BackupTimer},
		{name: backupServiceUnit, into: &held.BackupService, pid: true},
	} {
		word, r := retireUnitPostcondition(ctx, insp, unit.name, unit.pid, unit.alive)
		if r != nil {
			return held, atRetirePhase(j, r)
		}

		*unit.into = word
	}

	return held, nil
}

// retireConfigPostcondition says what the installed configuration must be: a
// host that kept a node keeps one, and a server-only host has none, because a
// configuration with neither role is one `config.Load` refuses.
func retireConfigPostcondition(configPath string, j retirement.Journal) (string, *retireRefusal) {
	obs, endpointRefusal := observeInstalledConfig(configPath, true)
	if endpointRefusal != nil {
		return "", atRetirePhase(j, retireFromEndpointFor(retireReasonPostcondition, endpointRefusal))
	}

	want := j.Variant == retirement.VariantRetainedNode

	switch {
	case obs.present && want:
		return retireConfigPresent, nil
	case !obs.present && !want:
		return retireConfigAbsent, nil
	case obs.present:
		return "", atRetirePhase(j, retireRefuse(retireReasonPostcondition, fmt.Sprintf("this host retired its server and "+
			"kept no node, and a configuration is installed at %s", obs.path), "the runbook in docs/operating/upgrades.md"))
	default:
		return "", atRetirePhase(j, retireRefuse(retireReasonPostcondition, fmt.Sprintf("this host kept a node and no "+
			"configuration is installed at %s", obs.path), "the runbook in docs/operating/upgrades.md"))
	}
}

// retireUnitPostcondition judges one unit. `alive` names the one unit a
// retained-node host must still be running.
func retireUnitPostcondition(ctx context.Context, insp *lifeops.Inspector, unit string, pid, alive bool,
) (string, *retireRefusal) {
	props, err := insp.UnitProperties(ctx, unit, retireDoneProperties...)
	if err != nil {
		return "", retireUnknown(retireReasonUnit, fmt.Sprintf("read %s: %v", unit, err), "")
	}

	load, active := firstProp(props, "LoadState"), firstProp(props, "ActiveState")
	enablement, main := firstProp(props, "UnitFileState"), firstProp(props, "MainPID")

	if alive {
		if active != "active" {
			return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s on a host that kept its node",
				unit, activeWord(active)), "")
		}

		if enablement != "enabled" {
			return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s, not persistently enabled, on a "+
				"host that kept its node", unit, activeWord(enablement)), "")
		}

		return retireUnitRunning, nil
	}

	if active != "inactive" {
		return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s and a completed retirement leaves it "+
			"inactive", unit, activeWord(active)), "")
	}

	if pid && main != "0" && main != "" {
		return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is inactive and still has the main process "+
			"%s", unit, main), "")
	}

	// A UNIT SYSTEMD DOES NOT KNOW is quiescent only as a positive answer: the
	// load state says not-found AND what is above has already said it is
	// inactive with no process.
	if load == "not-found" {
		return retireUnitNotFound, nil
	}

	switch enablement {
	case "disabled", "masked":
		return retireUnitQuiet, nil
	default:
		return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s, and a completed retirement leaves it "+
			"disabled or masked", unit, activeWord(enablement)), "")
	}
}

// activeWord renders a property systemd answered with, or says it answered
// nothing at all.
func activeWord(value string) string {
	if value == "" {
		return "not answered for"
	}

	return value
}
