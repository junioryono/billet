package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

// THE TRANSITION AFTER INTENT is one loop: observe the host, ask the phase
// table what that leaves, do exactly that, write the phase, observe again.
// Nothing here re-derives eligibility — that was decided before `intent` and
// recorded in the journal — and every action leaves a state the table names,
// so an interruption at any point is resumed by the same loop.

// The bounds the transition imposes on itself, and the seams a test moves.
var (
	// retireBackupWait bounds the wait for a running backup. A backup started
	// under a stopped timer is an operator's: it is AWAITED and never killed,
	// because it holds descriptors into the directory about to be renamed, and
	// a wait that ends leaves the retirement exactly where it stood.
	retireBackupWait = 30 * time.Minute
	retireBackupPoll = 2 * time.Second
	// retireBeforeRename runs immediately before the archive's rename, under
	// every lock it holds; a test observes the host there. Nil in production.
	retireBeforeRename func()
	// retireBeforeConfigRename observes the boundary after the staged file flush.
	retireBeforeConfigRename func()
	// retireResetFailedFn clears the one failure this transition reconciles.
	retireResetFailedFn = retireResetFailed
	// retireSyncDir flushes a directory entry; a test fails it between a
	// namespace change and the phase that would certify it.
	retireSyncDir = syncDir
)

// cldExited is the si_code systemd reports in ExecMainCode for a process that
// EXITED rather than being killed by a signal (`CLD_EXITED`, 1; `CLD_KILLED` is
// 2). The refusal this transition reconciles is an exit and nothing else.
const cldExited = 1

// retireStateUnknown is the `state` of an answer that cannot say which phase
// the host holds, which is not the same as the phase this run last knew.
const retireStateUnknown = "unknown"

// retireStepLimit bounds the loop. Seven actions carry a retirement from
// intent to done and each one moves the phase or the host, so a run that has
// taken more than this is deciding from facts its own writes did not move.
const retireStepLimit = 12

// The properties each observation asks systemd for.
var (
	retireBackupProperties = []string{"LoadState", "ActiveState", "SubState", "Result", "MainPID",
		"ExecMainCode", "ExecMainStatus", "ExecMainStartTimestamp"}
	retireNodeProperties = []string{"LoadState", "ActiveState", "SubState", "UnitFileState", "MainPID",
		"InvocationID", "ExecMainStartTimestamp"}
)

// retireStep is one action the driver performed, and the phase it was in when
// the table chose it.
type retireStep struct {
	Action string `json:"action"`
	Phase  string `json:"phase"`
}

// retireTransition drives the phases from wherever the journal stands to
// `done`, and answers the journal as it then is.
//
// THE LIFECYCLE LOCK COVERS THE STOP-TO-ARCHIVE WINDOW AND NOTHING MORE:
// `local up` must not start what this is stopping, and the node's restart past
// the archive is a drain nobody should be queued behind.
func retireTransition(ctx context.Context, m retireMode, obs *installedConfigObservation, j retirement.Journal,
) (retirement.Journal, []retireStep, *retireRefusal) {
	var (
		steps []retireStep
		host  *hostLock
	)

	defer func() {
		if host == nil {
			return
		}

		if err := host.release(); err != nil {
			fmt.Fprintln(os.Stderr, "billet: release the lifecycle lock: "+err.Error())
		}
	}()

	if r := admitRetireRemaining(ctx, m, j); r != nil {
		return j, steps, r
	}

	for range retireStepLimit {
		facts, r := observeRetireFacts(ctx, m, j)
		if r != nil {
			return j, steps, r
		}

		d := retirement.Decide(j.Variant, j.Phase, facts)

		if d.Action == retirement.ActionRefuse {
			return j, steps, retireUnknown(retireReasonPhase, fmt.Sprintf("the host does not hold what a retirement at "+
				"%s admits (%s); the retirement stands where it is and nothing was changed", j.Phase, d.Reason),
				"the runbook in docs/operating/upgrades.md")
		}

		if d.Action == retirement.ActionPostconditions {
			_, r := observeRetirePostconditions(ctx, m, j)

			return j, steps, r
		}

		if r := admitRetireOperations(ctx, j, retireServiceSequence(j, d)); r != nil {
			return j, steps, r
		}

		if r := retireHoldLifecycle(&host, d.Action); r != nil {
			return j, steps, r
		}

		// Taking or releasing the lifecycle lock may wait past the admission.
		if r := admitRetireOperations(ctx, j, retireServiceSequence(j, d)); r != nil {
			return j, steps, r
		}

		steps = append(steps, retireStep{Action: string(d.Action), Phase: string(j.Phase)})

		j, r = performRetireAction(ctx, m, obs, j, d.Action)
		if r != nil {
			return j, steps, r
		}
	}

	return j, steps, retireUnknown(retireReasonPhase, fmt.Sprintf("the transition took %d steps without reaching done, "+
		"so the host is not moving under its own writes", retireStepLimit), "the runbook in docs/operating/upgrades.md")
}

// retireHoldLifecycle takes the lifecycle lock for the actions inside the
// stop-to-archive window and drops it for every action past it.
func retireHoldLifecycle(host **hostLock, action retirement.Action) *retireRefusal {
	switch action {
	case retirement.ActionAwaitBackup, retirement.ActionStop, retirement.ActionArchive, retirement.ActionAdvanceArchived:
		if *host != nil {
			return nil
		}

		l, err := lifecycleLock()
		if err != nil {
			return retireUnknown(retireReasonLifecycle, err.Error(), "")
		}

		*host = l

		return nil
	default:
		if *host == nil {
			return nil
		}

		err := (*host).release()
		*host = nil

		if err != nil {
			return retireUnknown(retireReasonLifecycle, "release the lifecycle lock: "+err.Error(), "")
		}

		return nil
	}
}

// performRetireAction is the one place an action is carried out, so the order
// the table chose is the order the host sees.
func performRetireAction(ctx context.Context, m retireMode, obs *installedConfigObservation, j retirement.Journal,
	action retirement.Action,
) (retirement.Journal, *retireRefusal) {
	switch action {
	case retirement.ActionAwaitBackup:
		return j, awaitRetireBackup(ctx, j)
	case retirement.ActionStop:
		return retireStop(ctx, j)
	case retirement.ActionArchive:
		return retireArchive(ctx, j)
	case retirement.ActionAdvanceArchived:
		if r := proveRetireStopped(ctx, j); r != nil {
			return j, r
		}
		for _, dir := range []string{filepath.Dir(j.IdentityDir), filepath.Dir(j.Archive)} {
			if err := retireSyncDir(dir); err != nil {
				return j, retireUnknown(retireReasonJournal, "flush "+dir+" before recording archived: "+err.Error(), "")
			}
		}
		if r := proveRetireStopped(ctx, j); r != nil {
			return j, r
		}
		return retireAdvancePhase(j, retirement.PhaseArchived)
	case retirement.ActionRewrite:
		if r := admitRetireRemaining(ctx, m, j); r != nil {
			return j, r
		}
		if j.Variant == retirement.VariantRetainedNode {
			if r := proveRetireInvocation(ctx, j.RetainedInvocation); r != nil {
				return j, r
			}
		}
		return retireRewrite(ctx, m, obs, j)
	case retirement.ActionAdvanceRewritten:
		return retireAdvance(j, retirement.PhaseConfigRewritten, filepath.Dir(m.configPath))
	case retirement.ActionRestart:
		return retireRestartNode(ctx, m.configPath, j)
	case retirement.ActionDone:
		return retireMarkDone(ctx, m, j)
	default:
		return j, retireUnknown(retireReasonPhase, fmt.Sprintf("the phase table answered %q, which this does not "+
			"perform", action), "")
	}
}

// observeRetireFacts is one fresh observation of everything the table reads at
// this phase. A fact that cannot be established is its own could-not-tell
// value, which the table refuses by name; only a read that says nothing at all
// refuses here.
func observeRetireFacts(ctx context.Context, m retireMode, j retirement.Journal) (retirement.Facts, *retireRefusal) {
	f := retirement.Facts{}

	identity, r := retireIdentityFact(j)
	if r != nil {
		return f, r
	}

	f.Identity = identity

	cfg, r := retireConfigFact(m.configPath, j)
	if r != nil {
		return f, r
	}

	f.Config = cfg

	stage, r := retireStageFact(j)
	if r != nil {
		return f, r
	}

	f.Stage = stage

	insp := endpointInspector()

	switch j.Phase {
	case retirement.PhaseIntent, retirement.PhaseStopped:
		props, err := insp.UnitProperties(ctx, backupServiceUnit, retireBackupProperties...)
		f.Backup = retirement.BackupUnknown
		if err == nil {
			f.Backup = retireBackupState(props)
			if retireBackupRefusedHere(props, j) {
				// Classify completion without resetting the manager during the
				// read-only decision. The admitted action reconciles it later.
				f.Backup = retirement.BackupInactive
			}
		}
	case retirement.PhaseArchived, retirement.PhaseConfigRewritten:
		if j.Variant == retirement.VariantRetainedNode {
			f.NodeChanged = retireNodeChangedFact(ctx, insp, m.configPath)
		}
	case retirement.PhaseNodeRestarted:
		f.NodeUnit = retireNodeUnitFact(ctx, insp, m.configPath)
	}

	return f, nil
}

// retireIdentityFact says where the identity directory is. A name that holds
// something other than a directory is could-not-tell, because the rename this
// transition makes is a directory's.
func retireIdentityFact(j retirement.Journal) (retirement.IdentityFact, *retireRefusal) {
	configured, err := retireDirPresent(j.IdentityDir)
	if err != nil {
		return "", retireUnknown(retireReasonIdentity, err.Error(), "")
	}

	archived, err := retireDirPresent(j.Archive)
	if err != nil {
		return "", retireUnknown(retireReasonIdentity, err.Error(), "")
	}

	switch {
	case configured && archived:
		return retirement.IdentityBoth, nil
	case configured:
		return retirement.IdentityConfigured, nil
	case archived:
		return retirement.IdentityArchive, nil
	default:
		return retirement.IdentityNeither, nil
	}
}

func retireDirPresent(path string) (bool, error) {
	info, err := os.Lstat(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("examine %s: %w", path, err)
	case !info.IsDir():
		return false, fmt.Errorf("%s is not a directory, and a retirement moves one", path)
	default:
		return true, nil
	}
}

// retireNamePresent says whether anything at all stands at a pathname.
//
// ANYTHING IS A CONFIGURATION for the clause that asks whether one is
// installed: a symlink, a directory or a device at that name is not the absence
// a retired server-only host must hold, and the name is what the role and
// systemd both point at. Only a positive ENOENT is absence; every other failure
// to look is could-not-tell.
func retireNamePresent(path string) (bool, error) {
	_, err := os.Lstat(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("examine %s: %w", path, err)
	default:
		return true, nil
	}
}

// retireConfigFact reads the installed configuration's digest and says which
// of the two recorded digests it is. An absent file is a positive ENOENT and
// never a failed read.
func retireConfigFact(path string, j retirement.Journal) (retirement.ConfigFact, *retireRefusal) {
	sum, _, err := hashRegular(path, maxConfigBytes)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return retirement.ConfigAbsent, nil
	case err != nil:
		return "", retireUnknown(retireReasonConfig, "digest the installed configuration: "+err.Error(), "")
	case sum == j.InstalledSHA256:
		return retirement.ConfigInstalled, nil
	case j.StagedSHA256 != "" && sum == j.StagedSHA256:
		return retirement.ConfigStaged, nil
	default:
		return retirement.ConfigOther, nil
	}
}

// retireStageFact reads the staged serverless rendering. Its digest is the one
// recorded at intent or the stage is not this retirement's.
func retireStageFact(j retirement.Journal) (retirement.StageFact, *retireRefusal) {
	body, presence, err := retirement.ReadStage()

	switch presence {
	case retirement.StageFileAbsent:
		return retirement.StageAbsent, nil
	case retirement.StageFilePresent:
		if j.StagedSHA256 != "" && retirement.Digest(body) == j.StagedSHA256 {
			return retirement.StageRecorded, nil
		}

		return retirement.StageOther, nil
	default:
		return "", retireUnknown(retireReasonStage, "read the staged configuration: "+errorText(err), "")
	}
}

// retireBackupFact judges the backup service.
//
// THE ONE FAILURE IT RECONCILES is the refusal this retirement caused: a
// backup that started after the timers were stopped, met the closed status and
// exited with the retiring code. That one is cleared with `reset-failed` and
// re-observed; every other failure, and every answer it cannot read, is
// could-not-tell, which the table refuses.
func retireBackupFact(ctx context.Context, insp *lifeops.Inspector, j retirement.Journal) retirement.BackupFact {
	props, err := insp.UnitProperties(ctx, backupServiceUnit, retireBackupProperties...)
	if err != nil {
		return retirement.BackupUnknown
	}

	fact := retireBackupState(props)
	if fact != retirement.BackupUnknown {
		return fact
	}

	if !retireBackupRefusedHere(props, j) {
		return retirement.BackupUnknown
	}

	if err := retireResetFailedFn(ctx, backupServiceUnit); err != nil {
		return retirement.BackupUnknown
	}

	after, err := insp.UnitProperties(ctx, backupServiceUnit, retireBackupProperties...)
	if err != nil {
		return retirement.BackupUnknown
	}

	if retireBackupState(after) == retirement.BackupInactive && firstProp(after, "Result") == "success" {
		return retirement.BackupInactive
	}

	return retirement.BackupUnknown
}

// retireBackupState reads the unit's state alone: inactive (an absent unit
// included, when systemd positively says so), running, or neither.
func retireBackupState(props map[string][]string) retirement.BackupFact {
	active := firstProp(props, "ActiveState")

	if firstProp(props, "LoadState") == "not-found" {
		if active == "inactive" {
			return retirement.BackupInactive
		}

		return retirement.BackupUnknown
	}

	switch active {
	case "inactive":
		if firstProp(props, "MainPID") == "0" {
			return retirement.BackupInactive
		}

		return retirement.BackupUnknown
	case "active", "activating", "deactivating", "reloading":
		return retirement.BackupActive
	default:
		return retirement.BackupUnknown
	}
}

// retireBackupRefusedHere is the proved refusal: the unit failed by EXITING
// with the retiring status, and it started after this retirement stopped the
// timers. Every field is required, because a backup that failed for its own
// reasons is an operator's to look at and not a fact to clear away.
func retireBackupRefusedHere(props map[string][]string, j retirement.Journal) bool {
	if firstProp(props, "ActiveState") != "failed" ||
		firstProp(props, "Result") != "exit-code" ||
		firstProp(props, "ExecMainCode") != strconv.Itoa(cldExited) ||
		firstProp(props, "ExecMainStatus") != strconv.Itoa(exitRetiring) ||
		firstProp(props, "MainPID") != "0" {
		return false
	}

	stopped, err := time.Parse(time.RFC3339Nano, j.TimerStoppedAt)
	if err != nil {
		return false
	}

	started, ok := retireSystemdTimestamp(firstProp(props, "ExecMainStartTimestamp"))

	return ok && started.After(stopped)
}

// retireSystemdTimestamp parses systemd's rendered timestamp
// (`Fri 2026-09-11 14:02:03 UTC`), in UTC alone.
//
// A ZONE THIS CANNOT RESOLVE IS NOT READ AS UTC. systemd renders the host's own
// zone abbreviation, and reading `11:30 CEST` as `11:30Z` moves the instant two
// hours LATER than it was, which is the admitting direction for both callers: a
// backup that failed before the timers stopped would be cleared as this
// retirement's, and a configuration older than the running node would read as
// newer. An abbreviation is refused as could-not-tell, and the refusal it
// produces is the safe answer.
//
// The residual, stated: on a host whose systemd renders a local zone, the
// backup reconciliation refuses and the retirement waits for an operator. The
// units billet ships log and render in UTC.
func retireSystemdTimestamp(rendered string) (time.Time, bool) {
	fields := strings.Fields(rendered)
	if len(fields) < 4 || (fields[3] != "UTC" && fields[3] != "GMT") {
		return time.Time{}, false
	}

	t, err := time.ParseInLocation(time.DateTime, fields[1]+" "+fields[2], time.UTC)
	if err != nil {
		return time.Time{}, false
	}

	return t, true
}

// retireNodeChangedFact says whether the node's configuration changed since
// the running node started.
//
// A NODE THAT IS POSITIVELY NOT RUNNING ANSWERS `true`, because the predicate
// is about a process reading the file this transition rewrote and no process
// is reading it: the phases that require `false` require a running node, and
// the phase that acts on `true` is the restart, which is what such a host
// needs. Anything else is could-not-tell.
func retireNodeChangedFact(ctx context.Context, insp *lifeops.Inspector, configPath string) retirement.Verdict {
	props, err := insp.UnitProperties(ctx, nodeUnit, retireNodeProperties...)
	if err != nil {
		return retirement.VerdictUnknown
	}

	switch firstProp(props, "ActiveState") {
	case "inactive", "failed":
		return retirement.VerdictTrue
	case "active":
	default:
		return retirement.VerdictUnknown
	}

	started, ok := retireSystemdTimestamp(firstProp(props, "ExecMainStartTimestamp"))
	if !ok {
		return retirement.VerdictUnknown
	}

	info, err := os.Stat(configPath)
	if err != nil {
		return retirement.VerdictUnknown
	}

	// SYSTEMD RENDERS SECONDS AND THE FILESYSTEM KEEPS NANOSECONDS, so a start
	// and a write inside one second have no observable order: the parsed start
	// is the FLOOR of the true one, and only a modification a whole second
	// later, or one before the floor, orders itself against it. Anything
	// between is could-not-tell, which the table refuses by name rather than
	// manufacturing an ordering out of a truncation.
	switch mod := info.ModTime(); {
	case !mod.Before(started.Add(time.Second)):
		return retirement.VerdictTrue
	case mod.Before(started):
		return retirement.VerdictFalse
	default:
		return retirement.VerdictUnknown
	}
}

// retireNodeUnitFact judges the node at `node-restarted`: ready is active,
// persistently enabled, and its own registration record naming this
// incarnation and the endpoint the installed configuration now carries.
func retireNodeUnitFact(ctx context.Context, insp *lifeops.Inspector, configPath string) retirement.NodeUnitFact {
	props, err := insp.UnitProperties(ctx, nodeUnit, retireNodeProperties...)
	if err != nil {
		return retirement.NodeUnknown
	}

	switch firstProp(props, "ActiveState") {
	case "inactive", "failed":
		return retirement.NodeInactive
	case "active":
	default:
		return retirement.NodeUnknown
	}

	switch state := firstProp(props, "UnitFileState"); state {
	case "enabled":
	case "enabled-runtime", "disabled", "static", "masked", "masked-runtime", "indirect", "generated",
		"transient", "linked", "linked-runtime", "alias", "bad":
		// A RECOGNISED STATE THAT IS NOT PERSISTENT ENABLEMENT, which the
		// restart the table then performs establishes.
		return retirement.NodeUnenabled
	default:
		// An empty answer, or a word systemd has added since: not a state this
		// knows, and never a reason to stop a running node.
		return retirement.NodeUnknown
	}

	// THE INSTALLED CONFIGURATION AS IT NOW STANDS, which on a retiring host
	// has no server section at all: the observer the request uses requires one,
	// and this is the file the restarted node reads.
	obs, r := observeInstalledConfig(configPath, true)
	if r != nil {
		return retirement.NodeUnknown
	}

	want, present, err := nodeEndpointOf(obs.cfg)
	if err != nil || !present {
		return retirement.NodeUnknown
	}

	ev := readRegistrationRecord(registrationRecordPath)
	if ev.record == nil {
		return retirement.NodeUnknown
	}

	// A RECORD FROM ANOTHER INVOCATION is not permission to restart a node that
	// is positively ACTIVE: it is a node whose registration has not been
	// published yet, or a process that moved under this observation, and
	// stopping it would drain work to answer a question the next converge
	// answers for nothing. Could-not-tell, which the table refuses by name.
	if ev.record.InvocationID != firstProp(props, "InvocationID") {
		return retirement.NodeUnknown
	}

	dialled, err := endpoint.ParseCanonical(ev.record.Endpoint)
	if err != nil || !dialled.Equal(want) {
		return retirement.NodeUnknown
	}

	return retirement.NodeReady
}

// awaitRetireBackup waits for a running backup to finish, bounded. It holds no
// authority lock: the backup is taking one, and waiting under it would be a
// deadlock this transition made itself.
func awaitRetireBackup(ctx context.Context, j retirement.Journal) *retireRefusal {
	insp := endpointInspector()

	// THE BOUND IS A TIMER AND NOT THE RECORD'S CLOCK: every time this command
	// writes into a record comes from one seam a caller can pin, and a wait
	// measured against a pinned clock is a wait that never ends.
	timer := time.NewTimer(retireBackupWait)
	defer timer.Stop()

	for {
		switch retireBackupFact(ctx, insp, j) {
		case retirement.BackupInactive:
			return nil
		case retirement.BackupActive:
		default:
			return retireUnknown(retireReasonBackup, "the backup service's state could not be read while waiting for it "+
				"to finish", "")
		}

		select {
		case <-ctx.Done():
			return retireUnknown(retireReasonBackup, "the wait for the backup ended: "+ctx.Err().Error(), "")
		case <-timer.C:
			return retireUnknown(retireReasonBackup, fmt.Sprintf("%s was still running after %s; a backup is awaited and "+
				"never killed, and the retirement stands at %s until it finishes", backupServiceUnit, retireBackupWait,
				j.Phase), "")
		case <-time.After(retireBackupPoll):
		}
	}
}

// retireStop is the phase after intent: the timers stop, their stop is
// recorded, the server stops and is disabled, the backup is proved finished,
// the status closes, and only then does the journal advance.
//
// THE TIMERS' STOP IS RECORDED BEFORE THE STATUS CLOSES, because a backup that
// starts in that window meets the closed status and refuses, and a resume can
// only recognise that refusal as this retirement's by the time the timers
// stopped.
func retireStop(ctx context.Context, j retirement.Journal) (retirement.Journal, *retireRefusal) {
	c := converge()

	for _, unit := range []string{upgradeTimerUnit, backupTimerUnit} {
		if r := stopAndDisableForRetirement(ctx, c, j, unit); r != nil {
			return j, r
		}
	}

	if j.TimerStoppedAt == "" {
		j.TimerStoppedAt = retireNow().UTC().Format(time.RFC3339Nano)

		if err := j.Write(retireNow()); err != nil {
			return j, retireUnknown(retireReasonJournal, "record the timers' stop: "+err.Error(), "")
		}
	}

	if r := stopAndDisableForRetirement(ctx, c, j, serverUnit); r != nil {
		return j, r
	}

	if r := awaitRetireBackup(ctx, j); r != nil {
		return j, r
	}

	if r := proveRetireStopped(ctx, j); r != nil {
		return j, r
	}

	if err := retirement.WriteStatus(retirement.PhaseStopped, j.Variant, retireNow()); err != nil {
		return j, retireUnknown(retireReasonStatus, "publish the status: "+err.Error(), "")
	}

	if r := proveRetireStopped(ctx, j); r != nil {
		return j, r
	}

	return retireAdvancePhase(j, retirement.PhaseStopped)
}

// stopAndDisableForRetirement stops a unit and disables it, so nothing systemd
// knows about starts it again on this host or at the next boot.
func stopAndDisableForRetirement(ctx context.Context, c converger, j retirement.Journal, unit string) *retireRefusal {
	if r := admitRetireOperation(ctx, j, "stop", unit); r != nil {
		return r
	}

	if _, err := c.StopAndProve(ctx, unit); err != nil {
		return retireUnknown(retireReasonStop, fmt.Sprintf("stop %s: %v", unit, err), "")
	}

	if r := admitRetireOperation(ctx, j, "disable", unit); r != nil {
		return r
	}

	if err := c.Disable(ctx, unit); err != nil {
		return retireUnknown(retireReasonStop, fmt.Sprintf("disable %s: %v", unit, err), "")
	}

	return nil
}

// retireArchive renames the identity directory to the archive recorded at
// intent, UNDER THE IDENTITY EXCLUSION, so no authority writer is inside the
// directory when it moves. The access is told the directory moved, because
// after the rename there is nothing at its name to hand back.
func retireArchive(ctx context.Context, j retirement.Journal) (retirement.Journal, *retireRefusal) {
	// THE TRANSITION'S OWN EXCLUSION: it acquires the global lock and does not
	// admit itself through the status it published, because that status is what
	// it is publishing and it is the one writer the status allows.
	acc, err := openRetiringIdentityAccess(ctx, j.IdentityDir, identityAccessWait)
	if err != nil {
		return j, retireUnknown(retireReasonIdentity, "take the identity exclusion for the archive: "+err.Error(), "")
	}

	j, moved, r := archiveUnderExclusion(ctx, j)

	if moved {
		acc.moved()
	}

	if err := acc.Release(); err != nil && r == nil {
		r = retireUnknown(retireReasonIdentity, "release the identity exclusion after the archive: "+err.Error(), "")
	}

	return j, r
}

// archiveUnderExclusion is the rename and its flushes, with the exclusion held
// by the caller; it answers whether the directory moved, so the release knows
// there is nothing left at its name to hand back.
func archiveUnderExclusion(ctx context.Context, j retirement.Journal) (retirement.Journal, bool, *retireRefusal) {
	if retireBeforeRename != nil {
		retireBeforeRename()
	}

	if r := admitRetireOperations(ctx, j, retireServiceSequence(j, retirement.Decision{Action: retirement.ActionArchive})); r != nil {
		return j, false, r
	}
	if r := proveRetireStopped(ctx, j); r != nil {
		return j, false, r
	}

	if err := os.Rename(j.IdentityDir, j.Archive); err != nil {
		return j, false, retireUnknown(retireReasonArchive, fmt.Sprintf("move %s to %s: %v", j.IdentityDir, j.Archive,
			err), "")
	}

	for _, dir := range []string{filepath.Dir(j.IdentityDir), filepath.Dir(j.Archive)} {
		if err := retireSyncDir(dir); err != nil {
			return j, true, retireUnknown(retireReasonArchive, "flush "+dir+": "+err.Error(), "")
		}
	}

	if r := proveRetireStopped(ctx, j); r != nil {
		return j, true, r
	}
	j, r := retireAdvancePhase(j, retirement.PhaseArchived)

	return j, true, r
}

// retireRewrite installs the staged serverless configuration over the
// installed one, or removes the installed one on a server-only host, durably
// in both cases, and only then publishes the phase that certifies it.
func retireRewrite(ctx context.Context, m retireMode, obs *installedConfigObservation, j retirement.Journal,
) (retirement.Journal, *retireRefusal) {
	if j.Variant == retirement.VariantServerOnly {
		if r := proveRetireActivation(ctx, false); r != nil {
			return j, r
		}
		if err := os.Remove(m.configPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return j, retireUnknown(retireReasonRewrite, "remove "+m.configPath+": "+err.Error(), "")
		}

		if err := retireSyncDir(filepath.Dir(m.configPath)); err != nil {
			return j, retireUnknown(retireReasonRewrite, err.Error(), "")
		}

		return retireAdvancePhase(j, retirement.PhaseConfigRewritten)
	}

	body, presence, err := retirement.ReadStage()
	if presence != retirement.StageFilePresent {
		return j, retireUnknown(retireReasonStage, "read the staged configuration to install it: "+errorText(err), "")
	}

	// THE BYTES ARE THE ONES INTENT RECORDED, checked again here rather than
	// trusted from the observation the table decided on: what is installed is
	// what the digest chain says was staged, or nothing is installed at all.
	if retirement.Digest(body) != j.StagedSHA256 {
		return j, retireUnknown(retireReasonStage, "the staged configuration is not the one recorded at intent", "")
	}

	if r := proveRetireActivation(ctx, false); r != nil {
		return j, r
	}
	var boundaryRefusal *retireRefusal
	if err := installRetireConfig(m.configPath, body, func() error {
		boundaryRefusal = admitRetireRemaining(ctx, m, j)
		if boundaryRefusal == nil {
			boundaryRefusal = proveRetireActivation(ctx, false)
		}
		if boundaryRefusal != nil {
			return errors.New(boundaryRefusal.Why)
		}
		return nil
	}); err != nil {
		if boundaryRefusal != nil {
			return j, boundaryRefusal
		}
		return j, retireUnknown(retireReasonRewrite, err.Error(), "")
	}

	if obs != nil {
		obs.sha256 = retirement.Digest(body)
	}

	return retireAdvancePhase(j, retirement.PhaseConfigRewritten)
}

// installRetireConfig writes body over path with the installed file's own
// owner and mode: a temporary file beside it, its bytes flushed, renamed over
// the name, and the directory flushed, so a power loss leaves either
// configuration whole and never half of one.
func installRetireConfig(path string, body []byte, beforeMutation func() error) error {
	dir := filepath.Dir(path)

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("examine the installed configuration %s: %w", path, err)
	}

	if err := beforeMutation(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".billet-serverless-*")
	if err != nil {
		return fmt.Errorf("stage the serverless configuration beside %s: %w", path, err)
	}

	installed := false

	defer func() {
		_ = tmp.Close()

		if !installed {
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("write the serverless configuration: %w", err)
	}

	// THE MODE AND THE OWNER BEFORE THE FLUSH, because a metadata change made
	// after it is not something the flush committed.
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("set the mode of the serverless configuration: %w", err)
	}

	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return fmt.Errorf("own the serverless configuration: %w", err)
		}
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flush the serverless configuration: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close the serverless configuration: %w", err)
	}

	if retireBeforeConfigRename != nil {
		retireBeforeConfigRename()
	}
	// The temporary-file flush may block beyond the caller's admission.
	if err := beforeMutation(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("install the serverless configuration at %s: %w", path, err)
	}

	installed = true

	return retireSyncDir(dir)
}

// retireRestartNode establishes the node's persistent enablement and restarts
// it, so the node reads the serverless configuration it will keep.
//
// THE ENABLEMENT COMES FIRST and is read back, because a node that was active
// under `enabled-runtime` satisfies every running predicate and would leave a
// retirement whose completion requires a fact nothing made true.
func retireRestartNode(ctx context.Context, configPath string, j retirement.Journal) (retirement.Journal, *retireRefusal) {
	c := converge()

	if r := proveRetireNodeExecution(ctx, configPath); r != nil {
		return j, r
	}
	if r := admitRetireOperation(ctx, j, "enable", nodeUnit); r != nil {
		return j, r
	}

	if err := c.Enable(ctx, nodeUnit); err != nil {
		return j, retireUnknown(retireReasonRestart, "enable "+nodeUnit+": "+err.Error(), "")
	}

	state, err := c.EnabledNow(ctx, nodeUnit)
	if err != nil {
		return j, retireUnknown(retireReasonRestart, "read back the enablement of "+nodeUnit+": "+err.Error(), "")
	}

	// SYSTEMD'S OWN WORD, and exactly one of them: `enabled-runtime` is an
	// enablement this boot alone, which is not what a retirement leaves behind.
	if state.How != "enabled" {
		return j, retireUnknown(retireReasonRestart, fmt.Sprintf("%s is %q after being enabled, and a retirement needs it "+
			"persistently enabled", nodeUnit, state.How), "")
	}

	// Enabling the node can also enable the controller through [Install] Also=.
	// Read that back before disturbing the node or certifying its restart.
	controller, err := c.EnabledNow(ctx, serverUnit)
	if err != nil {
		return j, retireUnknown(retireReasonRestart, "read back the controller's enablement after enabling "+
			nodeUnit+": "+err.Error(), "")
	}

	if controller.How != "disabled" && controller.How != "masked" {
		return j, retireUnknown(retireReasonRestart, fmt.Sprintf("after enabling %s, %s is %q; the controller must remain "+
			"persistently disabled or masked before the node is stopped or started", nodeUnit, serverUnit, controller.How), "")
	}

	// THE NODE'S OWN STOP, which waits for the work it is running for as long
	// as that takes; nothing here bounds it.
	if r := admitRetireOperation(ctx, j, "stop", nodeUnit); r != nil {
		return j, r
	}

	if _, err := c.StopAndProve(ctx, nodeUnit); err != nil {
		return j, retireUnknown(retireReasonRestart, "stop "+nodeUnit+": "+err.Error(), "")
	}

	// Disappearance alone permits a failed drain. Only the complete successful
	// stop observation permits starting another invocation and advancing the phase.
	post, problem := observeUnit(ctx, endpointInspector(), nodeUnit)
	if problem != "" {
		return j, retireUnknown(retireReasonRestart, "after the node stop: "+problem+"; the node was not restarted", "")
	}

	if _, present := post.raw["Result"]; !present || post.ActiveState != "inactive" || post.SubState != "dead" ||
		post.Result != "success" {
		return j, retireUnknown(retireReasonRestart, fmt.Sprintf("the successful stop of %s is not proved: "+
			"ActiveState=%s SubState=%s Result=%s; only inactive/dead/success permits restart; the node was not restarted",
			nodeUnit, orUnknownWord(post.ActiveState), orUnknownWord(post.SubState), orUnknownWord(post.Result)), "")
	}

	if r := admitRetireOperation(ctx, j, "start", nodeUnit); r != nil {
		return j, r
	}

	if r := proveRetireNodeExecution(ctx, configPath); r != nil {
		return j, r
	}

	if r := proveRetireActivation(ctx, false); r != nil {
		return j, r
	}

	if _, err := c.StartAndProve(ctx, nodeUnit); err != nil {
		return j, retireUnknown(retireReasonRestart, "start "+nodeUnit+": "+err.Error(), "")
	}

	return retireAdvancePhase(j, retirement.PhaseNodeRestarted)
}

// retireMarkDone publishes the last phase and the status that goes with it.
// The tail after it (the receipt, the row and the marker) is the next change.
func retireMarkDone(ctx context.Context, m retireMode, j retirement.Journal) (retirement.Journal, *retireRefusal) {
	if _, r := observeRetirePostconditions(ctx, m, j); r != nil {
		return j, r
	}

	now := retireNow()
	j.DoneAt = now.UTC().Format(time.RFC3339Nano)

	next, r := retireAdvancePhase(j, retirement.PhaseDone)
	if r != nil {
		return next, r
	}

	if _, r := retireStatusPostcondition(ctx, m, next); r != nil {
		return next, r
	}

	return next, nil
}

// retireAdvance publishes a phase whose act ANOTHER RUN performed and was
// interrupted before it could flush: the directories that run owed are flushed
// here, before the phase that certifies them is written. A rename or an unlink
// is visible to the next observation long before its parent's entry is
// durable, so a phase published over an unflushed change could survive a power
// loss the change itself did not.
func retireAdvance(j retirement.Journal, phase retirement.Phase, dirs ...string) (retirement.Journal, *retireRefusal) {
	for _, dir := range dirs {
		if err := retireSyncDir(dir); err != nil {
			return j, retireUnknown(retireReasonJournal, fmt.Sprintf("flush %s before recording %s: %v", dir, phase,
				err), "")
		}
	}

	return retireAdvancePhase(j, phase)
}

// retireAdvancePhase writes the journal at its next phase and nothing else.
//
// A FAILED WRITE LEAVES THE PHASE WHERE IT WAS, and answers the journal the
// host still holds: the refusal's `state` is then what the next converge will
// find, never the phase this run was reaching for.
func retireAdvancePhase(j retirement.Journal, phase retirement.Phase) (retirement.Journal, *retireRefusal) {
	next := j
	next.Phase = phase

	err := next.Write(retireNow())
	if err == nil {
		return next, nil
	}

	// A PUBLISH CAN FAIL AFTER ITS RENAME, at the directory's flush, and the
	// journal on disk is then the new phase while this call failed: the answer
	// reads the journal back and says what the host HOLDS, with the durability
	// of that phase unknown, rather than naming a phase a reader would not
	// find.
	why := fmt.Sprintf("record the phase %s: %v", phase, err)

	read, presence, readErr := retirement.ReadJournal()
	switch {
	case presence != retirement.JournalPresent:
		// NEITHER PHASE IS KNOWN NOW: the write may have installed the new
		// journal before it failed, and the read that would say so failed too.
		r := retireUnknown(retireReasonJournal, fmt.Sprintf("%s; and reading the journal back: %v", why, readErr),
			"the runbook in docs/operating/upgrades.md")
		r.State = retireStateUnknown

		return j, r
	case read.Phase == phase:
		return read, retireUnknown(retireReasonJournal, why+"; the journal reads "+string(phase)+
			" on disk and whether that is durable cannot be told here", "")
	default:
		return j, retireUnknown(retireReasonJournal, why, "")
	}
}

// retireResetFailed clears one failed unit, so the backup's proved refusal is
// not left as a failure the next observation cannot explain.
func retireResetFailed(ctx context.Context, unit string) error {
	ctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, systemctlBinary, "reset-failed", "--", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl reset-failed %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// errorText is a cause's words, or the absence that had none.
func errorText(err error) string {
	if err == nil {
		return "the stage is absent"
	}

	return err.Error()
}
