// Package hostupgrade replaces billet on one machine, transactionally.
//
// WHY AN EXECUTOR OUTSIDE THE PROCESS BEING REPLACED. A control plane cannot
// install its own successor: the moment it stops, whatever was going to finish
// the job has stopped too, and a machine left with the old binary hidden and the
// new one half-installed has no process on it that knows what was happening. So
// this runs as its own short-lived program, and everything it is midway through
// is on the disk rather than in its memory.
//
// WHY A JOURNAL AND NOT A SCRIPT. Every step here is irreversible in a different
// way — a drained node, a hidden binary, a migrated ledger — and the recovery for
// each is different too. A script that fails partway leaves a machine in a state
// only the person reading the script can classify; a journal says which step
// completed, which makes the next run's decision mechanical.
package hostupgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Step is how far one upgrade has got.
//
// ORDERED, AND THE ORDER IS THE SAFETY CONTENT. Each step is safe to repeat and
// safe to unwind only because of what has and has not happened before it, so the
// sequence is written down once, here, rather than implied by the order of calls
// in a function somebody may reorder.
type Step string

const (
	// StepClaimed is the exclusion taken and nothing else done.
	StepClaimed Step = "claimed"

	// StepStaged is a verified candidate on the disk, with nothing stopped.
	//
	// EVERYTHING THAT CAN FAIL WITHOUT CONSEQUENCE HAPPENS BEFORE THIS. Resolving
	// a channel, verifying a signature, downloading an archive and checking a
	// digest are all things that can go wrong, and all of them go wrong here —
	// while the deployment is still running normally and the recovery is to do
	// nothing.
	StepStaged Step = "staged"

	// StepStopped is both services stopped and the old binary hidden.
	StepStopped Step = "stopped"
	// StepImaged is a guest generation the candidate's contract accepts in the
	// cluster, for every image this host's microVM tiers boot.
	//
	// BEFORE THE FENCE, because a pull records nothing in the ledger and
	// everything in the cluster and on this host's disk, and AFTER THE STOP,
	// because nothing may launch against a generation while it is being imported
	// and verified. It has no unwinding: a generation the old binary does not
	// accept is inert to it, and the next upgrade finds it already there.
	StepImaged Step = "imaged"

	// StepFenced is the maintenance fence written and flushed.
	StepFenced Step = "fenced"

	// StepSnapshotted is a complete ledger snapshot in the recovery directory.
	//
	// THE POINT BEFORE WHICH NOTHING MAY MIGRATE. A migration is the one step that
	// cannot be undone by putting the old binary back, because the old binary
	// refuses a schema it has never heard of — so the snapshot is what makes the
	// rest of this reversible at all.
	StepSnapshotted Step = "snapshotted"

	// StepInstalled is the candidate binary in place.
	StepInstalled Step = "installed"

	// StepMigrated is the ledger migrated by the candidate, as the only writer.
	StepMigrated Step = "migrated"

	// StepProbed is the candidate proved able to open what it inherited, under
	// units that poll nothing and accept no workload.
	StepProbed Step = "probed"

	// StepCommitted is the durable decision that this upgrade succeeded.
	//
	// THE POINT AFTER WHICH RECOVERY MUST NEVER RESTORE THE SNAPSHOT. The fence
	// may already have opened and admitted operator writes, so putting the old
	// ledger back would discard work committed against the new one. A crash after
	// this retries the startup, never the rollback.
	StepCommitted Step = "committed"

	// StepRolledBack is the durable decision that this upgrade failed and the
	// previous state was restored.
	StepRolledBack Step = "rolled_back"
)

// ordered is the sequence, for the resume decision and for the tests that assert
// it. StepCommitted and StepRolledBack are terminal and are not in it.
var ordered = []Step{
	StepClaimed,
	StepStaged,
	StepStopped,
	StepImaged,
	StepFenced,
	StepSnapshotted,
	StepInstalled,
	StepMigrated,
	StepProbed,
}

// Decided reports whether a step is a durable terminal decision.
func (s Step) Decided() bool { return s == StepCommitted || s == StepRolledBack }

// Journal is the durable record of one upgrade.
type Journal struct {
	// Dir is the unique recovery directory this upgrade owns.
	Dir string `json:"dir"`

	// FromVersion and ToVersion are what an operator reads when they find a
	// machine mid-upgrade.
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`

	// TargetDigest is the manifest this candidate came from, so a resuming run
	// can tell whether the staged bytes belong to the decision it is resuming.
	TargetDigest string `json:"target_digest"`

	// RolloutID and Generation tie this to the fleet decision that asked for it,
	// or are empty for an operator running it by hand.
	//
	// RECORDED RATHER THAN ENFORCED HERE, and the distinction matters. What stops a
	// delayed instruction from a superseded rollout starting a second transaction
	// on this machine is the CLAIM, which refuses while one is in progress at all —
	// the generation cannot do that job, because the two instructions may arrive
	// with nothing of the first left to compare against. What it does is answer
	// "which decision does this machine's half-finished upgrade belong to", for an
	// operator reading a journal and for a coordinator reconciling one.
	RolloutID  string `json:"rollout_id,omitempty"`
	Generation int64  `json:"generation,omitempty"`

	// AllowDowngrade records that a person asked for a release older than the
	// one this ledger has been served by, and that the transaction may lower the
	// ledger's release watermark to admit it.
	//
	// ON THE JOURNAL BECAUSE A RESUME HAS TO KNOW. The mark is lowered inside the
	// migrate step, and a resumed run that had forgotten the permission would
	// have the candidate refused by a ledger this same transaction had already
	// snapshotted and was about to hand it.
	AllowDowngrade bool `json:"allow_downgrade,omitempty"`

	// Ledger says where this host's ledger lives: empty for the SQLite file in
	// the state directory, LedgerExternal for a database billet does not hold.
	//
	// ON THE JOURNAL BECAUSE IT DECIDES WHICH STEPS EXIST. An external ledger is
	// neither fenced, snapshotted nor migrated by the updater — billet copies no
	// PostgreSQL database, and the candidate migrates when it takes the
	// controller claim — and a resumed run has to skip the same steps the
	// interrupted one did, or it would try to restore a snapshot nothing took.
	Ledger string `json:"ledger,omitempty"`

	// PID is the process that claimed this transaction.
	//
	// A NAME FOR THE HOLDER, NOT A HANDLE ON IT. The transaction lock already says
	// whether somebody is working right now — the kernel drops it when the holder
	// dies — but it cannot say WHICH process, and an operator looking at a host
	// that has been draining for an hour needs something to look at. Nothing acts
	// on this: a pid is a number the kernel reuses, and killing an updater that may
	// already be installing is exactly what this command must not do.
	PID int `json:"pid,omitempty"`

	Step      Step   `json:"step"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`

	// Failure is why this upgrade stopped, when it did.
	Failure string `json:"failure,omitempty"`
}

// LedgerExternal marks a journal for a host whose ledger is a database billet
// does not hold.
const LedgerExternal = "external"

// ExternalLedger reports whether this transaction has no ledger file to fence,
// snapshot or migrate.
func (j *Journal) ExternalLedger() bool { return j.Ledger == LedgerExternal }

// JournalName is the file inside the recovery directory.
const JournalName = "journal.json"

// SnapshotName is the ledger snapshot inside the recovery directory.
const SnapshotName = "ledger.db"

// maxJournalBytes bounds what will be parsed. A journal is a few hundred bytes;
// the bound refuses a file something else wrote into the recovery directory.
const maxJournalBytes = 64 << 10

// ErrNoJournal means there is no upgrade in progress.
var ErrNoJournal = errors.New("hostupgrade: no upgrade is in progress")

// ReadJournal loads the record for an upgrade in progress.
func ReadJournal(dir string) (*Journal, error) {
	body, err := os.ReadFile(filepath.Join(dir, JournalName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoJournal
		}

		return nil, fmt.Errorf("hostupgrade: read the recovery journal: %w", err)
	}

	if len(body) > maxJournalBytes {
		return nil, fmt.Errorf("hostupgrade: the recovery journal is %d bytes, which is not a "+
			"journal billet wrote; refusing to act on it", len(body))
	}

	var j Journal
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("hostupgrade: the recovery journal could not be read: %w", err)
	}

	if !KnownStep(j.Step) {
		// A STEP THIS BUILD DOES NOT KNOW IS NOT ONE IT MAY RESUME. A newer binary
		// can write one, and guessing would unwind a machine past a point that
		// build considered committed.
		return nil, fmt.Errorf("hostupgrade: the recovery journal records step %q, which this "+
			"build does not understand. A newer billet may have written it; do not resume "+
			"an upgrade with a binary older than the one that started it", j.Step)
	}

	return &j, nil
}

// KnownStep reports whether a step is one this build understands.
func KnownStep(s Step) bool {
	if s.Decided() {
		return true
	}

	return slices.Contains(ordered, s)
}

// Write makes the journal durable.
//
// FSYNCED, INCLUDING THE DIRECTORY. The whole point of this file is to survive
// the crash that happens between two steps, and a record still in the page cache
// when the machine loses power records a step that did happen as one that did
// not — after which recovery unwinds work that was already committed.
func (j *Journal) Write() error {
	j.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)

	body, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("hostupgrade: render the recovery journal: %w", err)
	}

	path := filepath.Join(j.Dir, JournalName)
	staging := path + ".new"

	if err := os.WriteFile(staging, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("hostupgrade: stage the recovery journal: %w", err)
	}

	if err := syncFile(staging); err != nil {
		return err
	}

	if err := os.Rename(staging, path); err != nil {
		return fmt.Errorf("hostupgrade: publish the recovery journal: %w", err)
	}

	return syncDir(j.Dir)
}

// Advance records that one step completed.
func (j *Journal) Advance(s Step) error {
	j.Step = s

	return j.Write()
}

// Fail records why an upgrade stopped, without deciding what to do about it.
func (j *Journal) Fail(cause error) error {
	j.Failure = cause.Error()

	return j.Write()
}

// SnapshotPath is where this upgrade's ledger snapshot lives.
func (j *Journal) SnapshotPath() string { return filepath.Join(j.Dir, SnapshotName) }

// Reached reports whether an upgrade got at least as far as a step.
//
// THE RESUME DECISION IN ONE PLACE. Everything a recovery does is keyed on this:
// what has to be unwound is exactly what was reached, and comparing positions in
// the ordered list is the only reading of that which cannot disagree with itself.
func (j *Journal) Reached(s Step) bool {
	if j.Step.Decided() {
		// A DECIDED UPGRADE REACHED EVERYTHING IT WAS GOING TO. Committed means the
		// whole sequence happened; rolled back means the unwinding already ran.
		// Either way there is nothing left for a caller to conclude from a position.
		return true
	}

	return position(j.Step) >= position(s)
}

func position(s Step) int {
	for i, known := range ordered {
		if known == s {
			return i
		}
	}

	// AN UNPLACED STEP SORTS BEFORE EVERYTHING, so a caller asking "did we get as
	// far as X" about a journal this build cannot place answers no — which is the
	// direction that unwinds less rather than more.
	return -1
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("hostupgrade: reopen %s to flush it: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("hostupgrade: flush %s: %w", path, err)
	}

	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("hostupgrade: open %s to flush it: %w", dir, err)
	}

	defer func() { _ = f.Close() }()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("hostupgrade: flush %s: %w", dir, err)
	}

	return nil
}
