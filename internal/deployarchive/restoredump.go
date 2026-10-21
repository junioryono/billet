package deployarchive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/junioryono/billet/internal/state"
)

// publishDump puts a PostgreSQL ledger's dump into the database, and for a
// recovery first keeps a dump of what it replaces.
//
// THE ORDER IS THE CRASH ARGUMENT, as it is for the SQLite supersede. The
// archive's dump is restored only after the copy of the ledger it replaces is
// durable and its digest journaled, so every interrupted state has the
// operator's ledger somewhere billet can vouch for: still in the database, or in
// that copy. And the restore is one transaction, so there is no state between.
//
// THE DATABASE IS JOURNALED BEFORE ANYTHING IS WRITTEN THERE, and every later
// pass is held to it: a connection pointed elsewhere between two runs is refused
// rather than written to.
func publishDump(ctx context.Context, res *Result, req RestoreRequest, j *journal, a Action) error {
	pg := req.Plan.Target.Postgres
	if pg == nil {
		return errors.New("deployarchive: a PostgreSQL ledger's dump needs the database it is " +
			"restored into, and none was given")
	}

	stateDir := req.Plan.Target.StateDir

	led, err := pg.Peek(ctx)
	if err != nil {
		return fmt.Errorf("deployarchive: read the ledger this run writes: %w", err)
	}

	if err := bindLedgerDatabase(stateDir, j, led); err != nil {
		return err
	}

	// THE LEDGER THE WRITE REPLACES, which the writer re-proves on its own
	// connection: this deployment's for a recovery, none for a restore into an
	// empty or pristine schema.
	into := state.RestoreInto{Database: j.LedgerDatabase, Schema: req.Plan.Archive.Manifest.Ledger.Dump.Schema}

	if a.Disposition == SupersedeLedger {
		// RE-PROVED ON THIS READ, not taken from the plan's: the ledger a
		// recovery replaces is this deployment's, or nothing is replaced.
		deployment := req.Plan.Archive.Manifest.DeploymentID
		if led.Binding != deployment {
			return fmt.Errorf("deployarchive: the ledger in database %s is bound to %q, not to "+
				"deployment %s, so this recovery will not replace it", led.Database, led.Binding, deployment)
		}

		into.Binding = deployment

		if err := supersedeDump(ctx, res, req, j); err != nil {
			return err
		}
	}

	stray, err := restoreArchiveDump(ctx, req, into)
	if stray != "" {
		res.Strays = append(res.Strays, stray)
	}

	if err != nil {
		return err
	}

	if err := verifyRestored(ctx, req.Plan.Archive, pg); err != nil {
		return err
	}

	j.LedgerRestored = true

	return j.save(stateDir)
}

// bindLedgerDatabase records the database a run writes the ledger into, before
// its first write there, and holds every later pass to it.
//
// A JOURNAL THAT RECORDS WORK AND NO DATABASE COULD NOT TELL where that work
// went, and is refused rather than read as "anywhere".
func bindLedgerDatabase(stateDir string, j *journal, led state.PostgresLedger) error {
	switch {
	case led.Database == "":
		return errors.New("deployarchive: the PostgreSQL server did not say which database the " +
			"connection reaches, so billet will not write a ledger there")
	case j.LedgerDatabase == "" && (len(j.SupersededDigests) > 0 || j.SupersedePublished || j.LedgerRestored):
		return fmt.Errorf("deployarchive: this run's journal records work on a database without "+
			"saying which, so billet cannot tell whether %s is it; reconcile the ledger by hand",
			led.Database)
	case j.LedgerDatabase == "":
		j.LedgerDatabase = led.Database

		return j.save(stateDir)
	case led.Database != j.LedgerDatabase:
		return fmt.Errorf("deployarchive: this run began writing the ledger in database %s and "+
			"the connection now reaches %s; billet will not write a ledger into a database it "+
			"did not start in", j.LedgerDatabase, led.Database)
	}

	return nil
}

// restoreArchiveDump restores the archive's dump through a copy verified
// against its manifest record: Open checked the dump by pathname, and that is
// not the read pg_restore makes.
func restoreArchiveDump(ctx context.Context, req RestoreRequest, into state.RestoreInto) (string, error) {
	rec, found := req.Plan.Archive.Manifest.Record(EntryLedgerDump)
	if !found {
		return "", fmt.Errorf("deployarchive: this backup's manifest does not describe %s", EntryLedgerDump)
	}

	return restoreVerifiedCopy(ctx, req.Plan.Target, req.Plan.Archive.DumpPath(), rec,
		req.Plan.Archive.Manifest.Ledger.Dump.DumpVersion, into)
}

// verifyRestored reads back what the database holds after a restore, through
// billet's own connection, and refuses a ledger that is not the archive's.
//
// THE PROOF A FILE'S DIGEST GIVES A SQLITE RESTORE, reached the only way a
// database allows: the migrations the dump's own snapshot recorded must be what
// the schema now records, and the binding must be the archive's. It also proves
// pg_restore reached the database billet reads, which is the half of the
// connection a different client could have resolved differently.
func verifyRestored(ctx context.Context, a *Archive, pg PostgresLedger) error {
	led, err := pg.Peek(ctx)
	if err != nil {
		return fmt.Errorf("deployarchive: read back the restored ledger: %w", err)
	}

	facts := a.Manifest.Ledger.Dump

	switch {
	case led.Schema != facts.Schema:
		return fmt.Errorf("deployarchive: the ledger was restored and billet's connection now "+
			"resolves to schema %q rather than %q", led.Schema, facts.Schema)
	case !slices.Equal(led.Migrations, a.Manifest.Ledger.Migrations):
		return fmt.Errorf("deployarchive: schema %s does not record the migrations this backup's "+
			"ledger carried (%d against %d), so billet cannot vouch that pg_restore put this "+
			"backup's ledger where billet reads it", led.Schema, len(led.Migrations),
			len(a.Manifest.Ledger.Migrations))
	case facts.Binding != "" && led.Binding != facts.Binding:
		return fmt.Errorf("deployarchive: schema %s is bound to %q after the restore and this "+
			"backup's ledger was bound to %s", led.Schema, led.Binding, facts.Binding)
	}

	return nil
}

// ProveRestoredLedger proves the database a PostgreSQL restore or recovery is
// about to seal or open is the one it restored the archive's ledger into.
//
// FOR THE STEPS AFTER THE RESTORE, which read the connection afresh: a seal, a
// finish, a resume. Each of them would otherwise write to, or open the fence
// over, whatever database the connection reaches by then. It is a no-op for an
// archive that carries no dump.
func ProveRestoredLedger(ctx context.Context, plan Plan) error {
	if plan.Archive == nil || !plan.Archive.Manifest.Ledger.IsDump() {
		return nil
	}

	if plan.Target.Postgres == nil {
		return errors.New("deployarchive: a PostgreSQL ledger cannot be proved without the " +
			"database it is in")
	}

	prog, err := InProgress(plan.Target.StateDir)
	if err != nil {
		return err
	}

	if plan.Intent == ReplaceLedger {
		if err := requirePublishedCopy(plan.Archive, plan.Target.StateDir,
			prog.SupersededDigests, prog.SupersedePublished); err != nil {
			return err
		}
	}

	led, err := plan.Target.Postgres.Peek(ctx)
	if err != nil {
		return fmt.Errorf("deployarchive: read the restored ledger: %w", err)
	}

	return checkRestoredLedger(plan.Archive, prog.LedgerDatabase, led)
}

// requirePublishedCopy refuses a recovery whose published copy of the ledger it
// replaced is gone or changed: past publication that copy is the only record of
// the operator's ledger, and a recovery that finished without it would leave
// nothing an abandon could ever put back.
func requirePublishedCopy(a *Archive, stateDir string, digests map[string]string, published bool) error {
	if !published {
		return nil
	}

	path := supersededDumpPath(a, stateDir)

	want, ok := digests[filepath.Base(path)]
	if !ok {
		return fmt.Errorf("deployarchive: this recovery published a copy of the ledger it replaced "+
			"and its journal does not record that copy's digest at %s, so billet cannot vouch for it", path)
	}

	switch sum, _, err := digestFile(path); {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("deployarchive: %s, this recovery's copy of the ledger it replaced, is "+
			"gone; it is the only record of that ledger, so billet will not finish the recovery "+
			"without it. Put it back, or abandon the recovery", path)
	case err != nil:
		return err
	case sum != want:
		return fmt.Errorf("deployarchive: %s is not the copy this recovery took of the ledger it "+
			"replaced, so billet will not finish the recovery over it", path)
	}

	return nil
}

// checkRestoredLedger is ProveRestoredLedger's judgement: the journaled
// database, the archive's schema and binding, and the archive's migrations as
// the ones the ledger starts with (a seal's open may migrate it forward).
func checkRestoredLedger(a *Archive, database string, led state.PostgresLedger) error {
	facts := a.Manifest.Ledger.Dump
	archived := a.Manifest.Ledger.Migrations

	switch {
	case database == "":
		return errors.New("deployarchive: this run's journal does not say which database it " +
			"restored the ledger into, so billet cannot tell whether the connection reaches it")
	case led.Database != database:
		return fmt.Errorf("deployarchive: this run restored the ledger into database %s and the "+
			"connection now reaches %s", database, led.Database)
	case led.Schema != facts.Schema:
		return fmt.Errorf("deployarchive: this run restored the ledger into schema %s and the "+
			"connection now resolves to %q", facts.Schema, led.Schema)
	case facts.Binding != "" && led.Binding != facts.Binding:
		return fmt.Errorf("deployarchive: the ledger in database %s is bound to %q and the "+
			"one this run restored was bound to %s", led.Database, led.Binding, facts.Binding)
	case len(led.Migrations) < len(archived) || !slices.Equal(led.Migrations[:len(archived)], archived):
		return fmt.Errorf("deployarchive: schema %s does not begin with the migrations this "+
			"backup's ledger carried, so it is not the ledger this run restored", led.Schema)
	}

	return nil
}

// supersedeDump keeps a dump of the PostgreSQL ledger a recovery is about to
// replace, published under the superseded name.
//
// RECORDED BEFORE IT IS PUBLISHED, and published before anything replaces the
// ledger: the dump is taken into a private staging name, synced, digested, the
// digest journaled, and only then linked to its name, which fails rather than
// replaces. An abandon puts back only a dump whose digest the journal holds.
//
// A RESUME FINDS ITS OWN COPY. The name is the archive's, so a second pass
// computes the same one; a file there whose digest the journal holds is this
// recovery's, and anything else there is refused rather than replaced, because
// it is the only record of the work this recovery fails.
//
// AND A PUBLISHED COPY THAT IS GONE STOPS THE RESUME. Past publication the
// archive's restore may have begun on the database, so a fresh copy taken now
// would record a ledger part-way replaced as the operator's own.
func supersedeDump(ctx context.Context, res *Result, req RestoreRequest, j *journal) error {
	target := req.Plan.Superseded
	if target == "" {
		return errors.New("deployarchive: a supersede has nowhere to keep the old ledger")
	}

	base := filepath.Base(target)
	stateDir := req.Plan.Target.StateDir

	switch _, err := os.Lstat(target); {
	case err == nil:
		want, recorded := j.SupersededDigests[base]
		if !recorded {
			return fmt.Errorf("deployarchive: %s already exists and this recovery did not write it, "+
				"so billet will not take it for the copy of the ledger it is about to replace. Move "+
				"it somewhere else and run this again", target)
		}

		sum, _, err := digestFile(target)
		if err != nil {
			return err
		}

		if sum != want {
			return fmt.Errorf("deployarchive: %s is not the dump this recovery took of the ledger it "+
				"replaces, so billet will not go on with it as the record of that ledger", target)
		}

		// A RESUME MAKES IT DURABLE AGAIN BEFORE ANYTHING REPLACES THE LEDGER:
		// the run that linked it may have died before its directory sync, and
		// the archive's restore is only allowed once the copy cannot vanish.
		if err := publishedDurably(stateDir, target, j); err != nil {
			return err
		}

		res.Superseded = append(res.Superseded, target)

		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("deployarchive: inspect %s: %w", target, err)
	}

	if j.SupersedePublished {
		// AN ABANDON THAT ALREADY PUT THE LEDGER BACK moved the copy to its kept
		// name; finishing that abandon is the way on, not this recovery.
		if want, ok := j.SupersededDigests[base]; ok {
			if sum, _, err := digestFile(keptCopyPath(target, want)); err == nil && sum == want {
				return fmt.Errorf("deployarchive: an abandon of this recovery already put the ledger "+
					"back and kept its copy at %s; run `billet local recover --abandon` again to "+
					"finish it", keptCopyPath(target, want))
			}
		}

		return fmt.Errorf("deployarchive: %s was published as the copy of the ledger this "+
			"recovery replaces, and it is gone. The archive's restore may already have begun on "+
			"the database, so a copy taken now could hold a ledger part-way replaced; put the "+
			"copy back at that name, or reconcile the database by hand", target)
	}

	dir, err := os.MkdirTemp(stateDir, ".billet-supersede-*")
	if err != nil {
		return fmt.Errorf("deployarchive: stage the superseded ledger: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	staged := filepath.Join(dir, supersededDumpName)

	taken, err := req.Plan.Target.Postgres.Dump(ctx, staged)
	if err != nil {
		return fmt.Errorf("deployarchive: take a copy of the ledger this recovery replaces: %w", err)
	}

	// THE COPY IS OF THE LEDGER BEING REPLACED, read in the dump's own snapshot,
	// before its digest is journaled as the operator's: the database the run
	// journaled, the archive's schema, bound to this deployment. A copy of
	// anything else would become the ledger an abandon puts back.
	deployment := req.Plan.Archive.Manifest.DeploymentID
	if taken.Database != j.LedgerDatabase || taken.Facts.Schema != req.Plan.Archive.Manifest.Ledger.Dump.Schema ||
		taken.Facts.Binding != deployment {
		return fmt.Errorf("deployarchive: the copy pg_dump took is of %s.%s bound to %q, and this "+
			"recovery replaces the ledger in %s.%s bound to %s; billet will not keep it as the "+
			"record of that ledger", taken.Database, taken.Facts.Schema, taken.Facts.Binding,
			j.LedgerDatabase, req.Plan.Archive.Manifest.Ledger.Dump.Schema, deployment)
	}

	if err := os.Chmod(staged, entryMode); err != nil {
		return fmt.Errorf("deployarchive: tighten %s: %w", staged, err)
	}

	if err := syncFile(staged); err != nil {
		return err
	}

	sum, _, err := digestFile(staged)
	if err != nil {
		return err
	}

	j.SupersededDigests = map[string]string{base: sum}
	j.SupersededDumpVersion = taken.Facts.DumpVersion
	j.SupersededMigrations = taken.Migrations

	if err := j.save(stateDir); err != nil {
		return err
	}

	if err := os.Link(staged, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("deployarchive: %s appeared while this recovery was writing it, and "+
				"billet will not write over it", target)
		}

		return fmt.Errorf("deployarchive: keep the superseded ledger at %s: %w", target, err)
	}

	if err := publishedDurably(stateDir, target, j); err != nil {
		return err
	}

	res.Superseded = append(res.Superseded, target)

	return nil
}

// publishedDurably makes a recovery's copy and its name durable, then records
// that they are, which is what permits the archive's restore and what an
// abandon reads to tell "never replaced" from "replaced, and the copy is gone".
func publishedDurably(stateDir, target string, j *journal) error {
	if err := syncFile(target); err != nil {
		return err
	}

	if err := syncDir(stateDir); err != nil {
		return err
	}

	if j.SupersedePublished {
		return nil
	}

	j.SupersedePublished = true

	return j.save(stateDir)
}

// putSupersededDumpBack undoes a PostgreSQL recovery's replacement of the
// ledger, or says what a restore left in the database.
//
// THE JOURNAL SAYS WHETHER THE LEDGER WAS REPLACED, AND THE COPY PUTS IT BACK.
// The archive's dump is restored only after the copy is durable and the journal
// says so (SupersedePublished), so a journal that never got that far means the
// database was never touched. Past it, the copy is the only record of the
// operator's ledger: a copy whose digest the journal holds is restored, in one
// transaction after the tables it does not name are dropped, and a copy that is
// gone or that the journal does not vouch for is refused with the fence and the
// journal standing. What cannot be had is a digest of the database itself; the
// copy's digest, the restore's atomicity and the read-back stand in for it.
//
// THE DATABASE IS PROVED BEFORE ANYTHING IS RESTORED INTO IT: the one the
// journal names, at the archive's schema, bound to this deployment (a recovery
// replaces only a ledger bound to it, and every state between holds that
// binding). A connection pointed elsewhere since the recovery stopped is
// refused rather than overwritten. And it is proved after: the ledger read back
// records exactly the migrations the copy did, which the archive's ledger
// (bound to the same deployment) would not.
//
// THE COPY IS KEPT afterwards, renamed so a later recovery from the same archive
// does not take it for its own; nothing deletes a record of a ledger. An abandon
// that stopped after that rename finds its copy at the kept name, by digest, and
// finishes rather than refusing.
//
// A RESTORE'S ABANDON TOUCHES NO DATABASE. What its pg_restore put there is
// either nothing or the archive's whole ledger, and removing it is dropping a
// schema somebody may still want: that is said, not done.
func putSupersededDumpBack(ctx context.Context, a *Archive, t Target, intent Intent,
	j *journal, res *AbandonResult,
) error {
	if intent != ReplaceLedger {
		if j.LedgerRestored {
			res.LedgerLeftInDatabase = a.Manifest.Ledger.Dump.Schema
		}

		return nil
	}

	copyPath := supersededDumpPath(a, t.StateDir)
	want, recorded := j.SupersededDigests[filepath.Base(copyPath)]
	aside := keptCopyPath(copyPath, want)

	switch _, err := os.Lstat(copyPath); {
	case errors.Is(err, os.ErrNotExist):
		if recorded && j.SupersedePublished {
			// AN ABANDON THAT HAD ALREADY PUT THE LEDGER BACK: the copy moves to
			// its kept name only after the put-back was read back, so this
			// recovery's digest there is that proof, read back once more. A kept
			// name billet cannot read is "could not tell", said as itself.
			switch sum, _, err := digestFile(aside); {
			case err == nil && sum == want:
				if err := requirePutBack(ctx, a, t, j, aside); err != nil {
					return err
				}

				res.Restored = append(res.Restored, fmt.Sprintf("the ledger in %s, from the copy kept at %s",
					dumpTargetPath(a.Manifest.Ledger.Dump.Schema), aside))

				return syncDir(t.StateDir)
			case err != nil && !errors.Is(err, os.ErrNotExist):
				return fmt.Errorf("deployarchive: read %s, where an abandon keeps this recovery's "+
					"copy: %w", aside, err)
			}
		}

		if j.SupersedePublished || j.LedgerRestored {
			return unrestoredLedger(t.StateDir, copyPath,
				"this recovery published that copy of the ledger it replaced, and it is gone, so "+
					"billet cannot say what the database holds or put back what it held")
		}

		return nil
	case err != nil:
		return fmt.Errorf("deployarchive: inspect %s: %w", copyPath, err)
	}

	if !recorded {
		return unrestoredLedger(t.StateDir, copyPath,
			"this recovery never recorded taking a copy of that name, so billet cannot say which "+
				"ledger it holds")
	}

	sum, size, err := digestFile(copyPath)
	if err != nil {
		return err
	}

	if sum != want {
		return unrestoredLedger(t.StateDir, copyPath,
			"it is not the copy this recovery took of the ledger it replaced")
	}

	if j.LedgerDatabase == "" || j.SupersededDumpVersion <= 0 || len(j.SupersededMigrations) == 0 {
		return unrestoredLedger(t.StateDir, copyPath,
			"this recovery's journal does not say which database the copy came from, which "+
				"pg_dump took it or what it held, so billet cannot vouch for putting it back")
	}

	if t.Postgres == nil {
		return errors.New("deployarchive: putting a PostgreSQL ledger back needs the database it " +
			"goes into, and none was given")
	}

	schema := a.Manifest.Ledger.Dump.Schema

	before, err := t.Postgres.Peek(ctx)
	if err != nil {
		return fmt.Errorf("deployarchive: read the database the ledger goes back into: %w", err)
	}

	switch {
	case before.Database != j.LedgerDatabase:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"it was taken from database %s and the connection now reaches %s",
			j.LedgerDatabase, before.Database))
	case before.Schema != schema:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"it was taken from schema %s and the connection now resolves to %q", schema, before.Schema))
	case before.Binding != a.Manifest.DeploymentID:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"the database the connection reaches is bound to %q, not to this deployment",
			before.Binding))
	}

	if err := t.Postgres.Barrier(ctx); err != nil {
		return err
	}

	stray, err := restoreVerifiedCopy(ctx, t, copyPath,
		FileRecord{Path: copyPath, SHA256: want, Size: size}, j.SupersededDumpVersion,
		state.RestoreInto{Database: j.LedgerDatabase, Schema: schema, Binding: a.Manifest.DeploymentID})
	if stray != "" {
		res.Kept = append(res.Kept, stray)
	}

	if err != nil {
		return fmt.Errorf("deployarchive: put the ledger this recovery replaced back from %s: %w",
			copyPath, err)
	}

	if err := requirePutBack(ctx, a, t, j, copyPath); err != nil {
		return err
	}

	// KEPT UNDER ANOTHER NAME, no-clobber, and the kept name durable before the
	// recovery's name goes: a crash between the two leaves the copy at one name
	// or both, never neither. The kept name carries the copy's digest, so a
	// second abandoned recovery of the same archive keeps its own beside the
	// first's, and a run that finds this copy already kept only finishes the
	// unlink.
	switch err := os.Link(copyPath, aside); {
	case err == nil:
	case errors.Is(err, os.ErrExist):
		sum, _, err := digestFile(aside)
		if err != nil {
			return err
		}

		if sum != want {
			return fmt.Errorf("deployarchive: %s holds other bytes than this recovery's copy, and "+
				"billet will not write over it; move it and run the abandon again", aside)
		}
	default:
		return fmt.Errorf("deployarchive: keep %s at %s: %w", copyPath, aside, err)
	}

	if err := syncDir(t.StateDir); err != nil {
		return err
	}

	if err := os.Remove(copyPath); err != nil {
		return fmt.Errorf("deployarchive: remove %s after keeping it at %s: %w", copyPath, aside, err)
	}

	res.Restored = append(res.Restored, fmt.Sprintf("the ledger in %s, from the copy now kept at %s",
		dumpTargetPath(schema), aside))

	return syncDir(t.StateDir)
}

// keptCopyPath is where an abandon keeps a recovery's copy once the ledger is
// back: the recovery's name, ".abandoned-", and the head of the copy's digest.
func keptCopyPath(copyPath, digest string) string {
	return copyPath + ".abandoned-" + digest[:min(12, len(digest))]
}

// requirePutBack reads back a put-back: the journaled database, at the archive's
// schema, bound to this deployment, recording exactly the migrations the copy
// did.
func requirePutBack(ctx context.Context, a *Archive, t Target, j *journal, copyPath string) error {
	if t.Postgres == nil {
		return errors.New("deployarchive: reading back a PostgreSQL ledger needs the database it " +
			"is in, and none was given")
	}

	led, err := t.Postgres.Peek(ctx)
	if err != nil {
		return fmt.Errorf("deployarchive: read back the ledger put back from %s: %w", copyPath, err)
	}

	switch {
	case led.Database != j.LedgerDatabase:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"the put-back was read from database %s, not %s", led.Database, j.LedgerDatabase))
	case led.Schema != a.Manifest.Ledger.Dump.Schema:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"the put-back was read from schema %q, not %s", led.Schema, a.Manifest.Ledger.Dump.Schema))
	case led.Binding != a.Manifest.DeploymentID:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"the database is bound to %q after the put-back, not to this deployment", led.Binding))
	case !slices.Equal(led.Migrations, j.SupersededMigrations):
		return unrestoredLedger(t.StateDir, copyPath,
			"the ledger read back after the put-back does not record the migrations the copy did")
	}

	return nil
}

// restoreVerifiedCopy hands pg_restore a copy of a dump that this call
// verified against want, never the pathname whose bytes were checked earlier.
//
// THE DIGEST IS CHECKED ON THE BYTES THE RESTORE READS, for the reason copyFile
// gives: a file verified by pathname and then reopened by pathname can be other
// bytes by then, and these bytes become the deployment's ledger. The copy is
// private to the state directory and goes when the restore is done.
func restoreVerifiedCopy(ctx context.Context, t Target, src string, want FileRecord,
	dumpVersion int, into state.RestoreInto,
) (string, error) {
	dir, err := os.MkdirTemp(t.StateDir, ".billet-restore-dump-*")
	if err != nil {
		return "", fmt.Errorf("deployarchive: stage %s: %w", src, err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	staged := filepath.Join(dir, supersededDumpName)

	stray, err := copyFile(src, staged, want)
	if err != nil {
		return stray, err
	}

	return stray, t.Postgres.Restore(ctx, staged, dumpVersion, into)
}
