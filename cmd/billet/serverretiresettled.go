package main

import (
	"context"
	"reflect"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

func retireSettledPurpose(m retireMode) string {
	if m.checkSettledClosing {
		return retirement.PurposeSettledClosing
	}
	return retirement.PurposeSettledEntry
}

func answerRetireModeRefusal(m retireMode, r *retireRefusal) error {
	if !m.checkSettledEntry && !m.checkSettledClosing {
		return answerRetireRefusal(r)
	}
	code := exitRefused
	if r.Outcome == retireOutcomeUnknown {
		code = exitUnknown
	}
	return answerJSON(retirement.SettledRefusal{Schema: retirement.SettledSchema, Purpose: retireSettledPurpose(m), Outcome: r.Outcome,
		Reason: r.Reason, Why: r.Why, State: stateNothingRetire}, code, r.Why)
}

func checkRetireSettledCombination(m retireMode) *retireRefusal {
	if m.checkSettledEntry && m.checkSettledClosing || m.checkNodeConfig || m.input != "" {
		return retireRefuse(retireReasonCombination, "settled entry and closing are exclusive read-only modes and accept no input document", "")
	}
	// Reuse the admission operand table, including every forbidden mutation
	// operand. Its stdin requirement belongs only to node-config.
	m.input = "-"
	if r := checkRetireNodeConfigCombination(m); r != nil {
		r.Why = strings.ReplaceAll(r.Why, "--check-node-config", "settled entry or closing")
		r.Why = strings.ReplaceAll(r.Why, "--input -, ", "")
		return r
	}
	return nil
}

func retireCheckSettled(ctx context.Context, m retireMode) (any, *retireRefusal) {
	return withRetireInspection(ctx, func(root *txLock) (any, *retireRefusal) {
		j, activity, r := observeRetireOrdinaryEntry(ctx, m, root)
		if r != nil {
			return nil, r
		}
		installed, er := observeInstalledConfig(m.configPath, true)
		if er != nil || !installed.present || installed.cfg.Node == nil {
			return nil, retireUnknown(retireReasonConfig, "installed retained configuration could not be observed", "")
		}
		// Closing checks today's rendering and paths after ordinary work. Entry
		// checks those same inputs before any future-operation admission.
		if r := admitRetireNodeOperations(ctx, m, j, installed.cfg, retireNodeOperations{}); r != nil {
			return nil, r
		}
		after, afterActivity, r := observeRetireOrdinaryEntry(ctx, m, root)
		if r != nil {
			return nil, r
		}
		current, er := observeInstalledConfig(m.configPath, true)
		if er != nil || !current.present || !reflect.DeepEqual(installed.body, current.body) ||
			!reflect.DeepEqual(j, after) || activity != afterActivity {
			return nil, retireUnknown(retireReasonMismatch, "settled records, configuration or node activity changed during observation", "")
		}
		if m.checkSettledClosing {
			// No entry result can substitute for this proof, and no tail repair
			// may follow it. Registration is re-proved after path admission.
			if _, r := observeRetirePostconditions(ctx, m, j); r != nil {
				return nil, r
			}
			activity = "active"
		}
		if err := requireRootInPlace(root); err != nil {
			return nil, retireUnknown(retireReasonGuard, err.Error(), "")
		}
		outcome := "admitted"
		if m.checkSettledClosing {
			outcome = "verified"
		}
		changes, r := retireSettledEnablement(ctx, j)
		if r != nil {
			return nil, r
		}
		return &retirement.SettledVerdict{Schema: retirement.SettledSchema, Purpose: retireSettledPurpose(m), Outcome: outcome, EnablementChanges: changes,
			Run: m.run, Guard: m.expectedGuard, Retiring: m.retiringHost, Deployment: j.Deployment,
			TransitionID: j.Provenance.TransitionID, Variant: j.Variant, Phase: j.Phase, RowDone: j.RowDone,
			Settled: j.Settled, CompletedBy: j.CompletedBy, NodeActivity: activity, State: stateNothingRetire}, nil
	})
}

func observeRetireEntryJob(ctx context.Context, insp *lifeops.Inspector) *retireRefusal {
	props, err := insp.UnitProperties(ctx, nodeUnit, "Job", "ControlPID")
	if err != nil || len(props["Job"]) != 1 {
		return retireUnknown("settled-entry-node-job-observation", "queued job absence could not be observed", "")
	}
	if job := props["Job"][0]; job != "" {
		fields := strings.Fields(job)
		if len(fields) == 0 || strings.Join(fields, " ") != job {
			return retireUnknown("settled-entry-node-job-observation", "malformed queued job observation", "")
		}
		id, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != fields[0] || len(fields) > 2 ||
			len(fields) == 2 && fields[1] != "/org/freedesktop/systemd1/job/"+fields[0] {
			return retireUnknown("settled-entry-node-job-observation", "malformed queued job observation", "")
		}
		return retireRefuse("settled-entry-node-queued-job", "node has a queued lifecycle job", "")
	}
	if len(props["ControlPID"]) != 1 {
		return retireUnknown("settled-entry-node-observation-unreadable", "control process absence could not be observed", "")
	}
	pid, err := strconv.ParseUint(props["ControlPID"][0], 10, 32)
	if err != nil || strconv.FormatUint(pid, 10) != props["ControlPID"][0] {
		return retireUnknown("settled-entry-node-observation-unreadable", "control process absence could not be observed", "")
	}
	if pid != 0 {
		return retireRefuse("settled-entry-node-process-present", "node still has a control process", "")
	}
	return nil
}

func retireSettledEnablement(ctx context.Context, j retirement.Journal) (map[string]string, *retireRefusal) {
	changes := make(map[string]string)
	for _, unit := range retireInertUnits {
		props, err := retireOperationInspector().UnitProperties(ctx, unit, "LoadState", "UnitFileState")
		if err != nil || len(props["LoadState"]) != 1 || len(props["UnitFileState"]) != 1 {
			return nil, retireUnknown(retireReasonPostcondition, "controller enablement could not be observed", "")
		}
		state := firstProp(props, "UnitFileState")
		if firstProp(props, "LoadState") == "not-found" {
			continue
		}
		if !knownUnitFileState(state) {
			return nil, retireUnknown(retireReasonPostcondition, "controller enablement is unknown", "")
		}
		if state != "disabled" && state != "masked" && !((unit == backupServiceUnit || unit == upgradeServiceUnit) && state == "static") {
			changes[unit] = state
		}
	}
	if r := proveRetireInert(ctx, j); r != nil {
		return nil, r
	}
	return changes, nil
}
