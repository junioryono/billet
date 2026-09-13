package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

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
	Status        string `json:"status"`
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

	// The published status either already said `done`, or this run said it.
	retireStatusDone        = "done"
	retireStatusRepublished = "republished"
)

// retireDoneProperties is what every postcondition asks systemd for. A timer
// has no main process, and an absent answer for it is not a failure; for a
// service an absent answer is could-not-tell.
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
	//
	// AN ACKNOWLEDGED ROW WITH NO MARKER IS THE TAIL'S OWN CRASH WINDOW, not a
	// stranger's journal. The tail clears the marker and THEN writes
	// `settled`, deliberately, so that a crash between the two leaves exactly
	// this: `row_done` true, `settled` false, no marker. Requiring the marker
	// here would make the one window the ordering was chosen for the one
	// nothing can finish. What still has to hold is ownership — this holder,
	// or one this guard's takeover chain reached — which `Validate` below
	// asks, and which is the same tie a marker would have provided.
	if !j.Settled && !j.RowDone && !retireDrivesThisJournal(m, shape, j, retirement.JournalFactDone) {
		return nil, atRetirePhase(j, retireUnknown(retireReasonJournal, "this host's retirement is not finished and this "+
			"converge's guard does not carry its marker, so the tail it still owes is not this run's to finish",
			"the runbook in docs/operating/upgrades.md"))
	}

	// A SETTLED JOURNAL IS ASKED FOR ITS PROVENANCE ALONE; anything still
	// owing something is asked who owns it, because that is what says this
	// converge may finish it.
	expectation := retirement.JournalExpectation{Retiring: m.retiringHost, Identity: identity}
	if !j.Settled {
		expectation.Holder, expectation.TakenOverFrom = m.run, shape.Guard.TakenOverFrom
	}

	if err := j.Validate(expectation); err != nil {
		return nil, atRetirePhase(j, retireUnknown(retireReasonJournal, err.Error(),
			"the runbook in docs/operating/upgrades.md"))
	}

	// THE HOST IS JUDGED BEFORE EITHER BRANCH, and an unfinished tail is not
	// an exemption: every phase of the transition has run by the time a
	// journal reads `done`, so the units, the archive and the configuration
	// are what the retirement promised — and finishing a tail over a host that
	// has drifted from them would settle a retirement whose host no longer
	// holds what it recorded. The published status is the one thing left to
	// the tail, which repairs it.
	held, r := observeRetirePostconditions(ctx, m, j)
	if r != nil {
		return nil, r
	}

	if !j.Settled {
		// THE TAIL, FINISHED BY WHOEVER HOLDS THE GUARD NOW. Everything it
		// needs is in the journal, and the run that recorded the intent may be
		// long gone.
		return retireTail(ctx, m, root, dir, j, nil)
	}

	// THE PUBLISHED STATUS IS THE ONE POSTCONDITION THAT IS NOT A RECORD: it
	// is the file every ordinary authority writer reads to learn this host is
	// closed, so a retired host whose status went missing is one the next
	// `ca rotate` would be admitted on. It is repaired here rather than
	// refused — the journal is the record and this run knows what the status
	// should say — and the answer says which happened.
	status, r := retireStatusPostcondition(j)
	if r != nil {
		return nil, r
	}

	held.Status = status

	return &retireDoneAnswer{Schema: retireSchema, Outcome: retireOutcomeUnchanged, Phase: j.Phase,
		Variant: j.Variant, TransitionID: j.Provenance.TransitionID, Settled: true, RowDone: j.RowDone,
		CompletedBy: j.CompletedBy, Postconditions: held}, nil
}

// retireStatusPostcondition holds the published status to the journal, and
// republishes one that went missing, was damaged, or says something below what
// the journal reached.
func retireStatusPostcondition(j retirement.Journal) (string, *retireRefusal) {
	st, presence, err := retirement.ReadStatus()

	switch presence {
	case retirement.StatusPresent:
		if st.Phase == retirement.PhaseDone && st.Variant == j.Variant {
			return retireStatusDone, nil
		}
	case retirement.StatusAbsent, retirement.StatusMalformed:
	default:
		return "", atRetirePhase(j, retireUnknown(retireReasonStatus,
			"the published status could not be read: "+errorText(err), ""))
	}

	if err := retirement.WriteStatus(retirement.PhaseDone, j.Variant, retireNow()); err != nil {
		return "", atRetirePhase(j, retireUnknown(retireReasonStatus, "publish the status: "+errorText(err), ""))
	}

	return retireStatusRepublished, nil
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

	// A FILE IS NOT THE CONFIGURATION A RETIREMENT LEAVES. What the rewrite
	// installed has the node and NO server section, and the two ways a host
	// drifts back are both admitted by presence alone: the original
	// configuration restored, and a server-only one installed under a node
	// that is still running and could not restart with it. What is NOT
	// required is the staged digest: after `done` the ordinary render owns
	// this file, and a later legitimate change to the node's configuration is
	// rendered the ordinary way.
	if obs.present && want {
		switch {
		case obs.cfg.Server != nil:
			return "", atRetirePhase(j, retireRefuse(retireReasonPostcondition, fmt.Sprintf("the configuration at %s has "+
				"a server section again, and this host retired its server", obs.path),
				"the runbook in docs/operating/upgrades.md"))
		case obs.cfg.Node == nil:
			return "", atRetirePhase(j, retireRefuse(retireReasonPostcondition, fmt.Sprintf("the configuration at %s has "+
				"no node section, and this host kept its node", obs.path), "the runbook in docs/operating/upgrades.md"))
		}

		return retireConfigPresent, nil
	}

	switch {
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

	// A PROPERTY SYSTEMD DID NOT ANSWER, OR ANSWERED WITH A WORD THIS BILLET
	// DOES NOT KNOW, IS COULD-NOT-TELL. A refusal is a claim about what this
	// host holds, and an empty answer establishes nothing; the two are
	// different exit statuses to the role for that reason.
	if !knownActiveState(active) {
		return "", retireUnknown(retireReasonPostcondition, fmt.Sprintf("systemd answered %s's state as %s, which this "+
			"billet does not know", unit, activeWord(active)), "")
	}

	if alive {
		if active != "active" {
			return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s on a host that kept its node",
				unit, active), "")
		}

		if !knownUnitFileState(enablement) {
			return "", retireUnknown(retireReasonPostcondition, fmt.Sprintf("systemd answered %s's enablement as %s, "+
				"which this billet does not know", unit, activeWord(enablement)), "")
		}

		if enablement != "enabled" {
			return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s, not persistently enabled, on a "+
				"host that kept its node", unit, enablement), "")
		}

		// AND IT HAS A PROCESS. systemd reports a service whose process has
		// EXITED as active (SERVICE_EXITED maps to UNIT_ACTIVE), so the state
		// alone is not evidence that the node this host kept is running.
		switch running, err := strconv.Atoi(main); {
		case main == "" || err != nil || running < 0:
			return "", retireUnknown(retireReasonPostcondition, fmt.Sprintf("systemd did not answer %s's main process "+
				"as a process id (%s), so whether the node is running cannot be established", unit, activeWord(main)), "")
		case running == 0:
			return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is active with no main process on a host "+
				"that kept its node", unit), "")
		}

		return retireUnitRunning, nil
	}

	if active != "inactive" {
		return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s and a completed retirement leaves it "+
			"inactive", unit, active), "")
	}

	// A UNIT THAT NAMES NO PROCESS IS NOT A UNIT WITH NO PROCESS. systemd
	// answers `MainPID=0` for a service that has none, including one it does
	// not know; an EMPTY answer is one it did not give, and reading that as
	// "no process" would let a systemd that cannot be asked satisfy the one
	// clause the archive's safety rests on. Only a service is asked: a timer
	// has no main process and answers nothing for it.
	if pid {
		// EXACTLY ZERO SATISFIES IT. A negative answer is not evidence of no
		// process: it is an answer no systemd gives for a main pid, and
		// reading it as one would let a malformed property satisfy the clause
		// the archive's safety rests on.
		switch running, err := strconv.Atoi(main); {
		case main == "" || err != nil || running < 0:
			return "", retireUnknown(retireReasonPostcondition, fmt.Sprintf("systemd did not answer %s's main process as "+
				"a process id (%s), so whether it still has one cannot be established", unit, activeWord(main)), "")
		case running > 0:
			return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is inactive and still has the main process "+
				"%d", unit, running), "")
		}
	}

	// A UNIT SYSTEMD DOES NOT KNOW is quiescent only as a positive answer: the
	// load state says not-found AND what is above has already said it is
	// inactive with no process.
	if load == "not-found" {
		return retireUnitNotFound, nil
	}

	if !knownUnitFileState(enablement) {
		return "", retireUnknown(retireReasonPostcondition, fmt.Sprintf("systemd answered %s's enablement as %s, which "+
			"this billet does not know", unit, activeWord(enablement)), "")
	}

	switch enablement {
	case "disabled", "masked":
		return retireUnitQuiet, nil
	default:
		// `masked-runtime` is among these deliberately: it hides persistent
		// enablement and is gone at the next boot.
		return "", retireRefuse(retireReasonPostcondition, fmt.Sprintf("%s is %s, and a completed retirement leaves it "+
			"disabled or masked", unit, enablement), "")
	}
}

// knownActiveState and knownUnitFileState are systemd's own vocabularies. A
// word outside them is one systemd has added since, and no reason to say
// anything definite about this host.
func knownActiveState(state string) bool {
	switch state {
	case "active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance", "refreshing":
		return true
	}

	return false
}

func knownUnitFileState(state string) bool {
	switch state {
	case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias", "masked", "masked-runtime", "static",
		"indirect", "disabled", "generated", "transient", "bad":
		return true
	}

	return false
}

// activeWord renders a property systemd answered with, or says it answered
// nothing at all.
func activeWord(value string) string {
	if value == "" {
		return "not answered for"
	}

	return value
}
