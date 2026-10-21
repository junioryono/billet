package deployarchive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// publishDump puts a PostgreSQL ledger's dump into the database, and for a
// recovery first keeps a dump of what it replaces.
//
// THE ORDER IS THE CRASH ARGUMENT, as it is for the SQLite supersede. The
// archive's dump is restored only after the copy of the ledger it replaces is
// durable and its digest journaled, so every interrupted state has the
// operator's ledger somewhere billet can vouch for: still in the database, or in
// that copy. And the restore is one transaction, so there is no state between.
func publishDump(ctx context.Context, res *Result, req RestoreRequest, j *journal, a Action) error {
	pg := req.Plan.Target.Postgres
	if pg == nil {
		return errors.New("deployarchive: a PostgreSQL ledger's dump needs the database it is " +
			"restored into, and none was given")
	}

	if a.Disposition == SupersedeLedger {
		if err := supersedeDump(ctx, res, req, j); err != nil {
			return err
		}
	}

	stray, err := restoreArchiveDump(ctx, req)
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

	return j.save(req.Plan.Target.StateDir)
}

// restoreArchiveDump restores the archive's dump through a copy verified
// against its manifest record: Open checked the dump by pathname, and that is
// not the read pg_restore makes.
func restoreArchiveDump(ctx context.Context, req RestoreRequest) (string, error) {
	rec, found := req.Plan.Archive.Manifest.Record(EntryLedgerDump)
	if !found {
		return "", fmt.Errorf("deployarchive: this backup's manifest does not describe %s", EntryLedgerDump)
	}

	facts := req.Plan.Archive.Manifest.Ledger.Dump

	return restoreVerifiedCopy(ctx, req.Plan.Target, req.Plan.Archive.DumpPath(), rec,
		facts.DumpVersion, facts.Schema)
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
func supersedeDump(ctx context.Context, res *Result, req RestoreRequest, j *journal) error {
	target := req.Plan.Superseded
	if target == "" {
		return errors.New("deployarchive: a supersede has nowhere to keep the old ledger")
	}

	base := filepath.Base(target)
	stateDir := req.Plan.Target.StateDir

	// THE DATABASE THIS RECOVERY IS REPLACING THE LEDGER IN, read now and held
	// to what the journal recorded when the copy was taken: a connection pointed
	// at another database since is refused rather than overwritten.
	led, err := req.Plan.Target.Postgres.Peek(ctx)
	if err != nil {
		return fmt.Errorf("deployarchive: read the ledger this recovery replaces: %w", err)
	}

	if j.SupersededDatabase != "" && led.Database != j.SupersededDatabase {
		return fmt.Errorf("deployarchive: this recovery took its copy of the ledger from database "+
			"%s and the connection now reaches %s; billet will not replace a ledger in a database "+
			"it took no copy of", j.SupersededDatabase, led.Database)
	}

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
	j.SupersededDatabase = led.Database

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
// THE DATABASE IS PROVED BEFORE ANYTHING IS RESTORED INTO IT: the one the copy
// was taken from, at the archive's schema, bound to this deployment or to
// nobody. A connection pointed elsewhere since the recovery stopped is refused
// rather than overwritten.
//
// THE COPY IS KEPT afterwards, renamed so a later recovery from the same archive
// does not take it for its own; nothing deletes a record of a ledger.
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

	switch _, err := os.Lstat(copyPath); {
	case errors.Is(err, os.ErrNotExist):
		if j.SupersedePublished || j.LedgerRestored {
			return unrestoredLedger(t.StateDir, copyPath,
				"this recovery published that copy of the ledger it replaced, and it is gone, so "+
					"billet cannot say what the database holds or put back what it held")
		}

		return nil
	case err != nil:
		return fmt.Errorf("deployarchive: inspect %s: %w", copyPath, err)
	}

	want, recorded := j.SupersededDigests[filepath.Base(copyPath)]
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
	case j.SupersededDatabase != "" && before.Database != j.SupersededDatabase:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"it was taken from database %s and the connection now reaches %s",
			j.SupersededDatabase, before.Database))
	case before.Schema != schema:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"it was taken from schema %s and the connection now resolves to %q", schema, before.Schema))
	case before.Binding != "" && before.Binding != a.Manifest.DeploymentID:
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"the database the connection reaches is bound to %s, not to this deployment",
			before.Binding))
	}

	if err := t.Postgres.Barrier(ctx); err != nil {
		return err
	}

	stray, err := restoreVerifiedCopy(ctx, t, copyPath,
		FileRecord{Path: copyPath, SHA256: want, Size: size}, j.SupersededDumpVersion, schema)
	if stray != "" {
		res.Kept = append(res.Kept, stray)
	}

	if err != nil {
		return fmt.Errorf("deployarchive: put the ledger this recovery replaced back from %s: %w",
			copyPath, err)
	}

	led, err := t.Postgres.Peek(ctx)
	if err != nil {
		return fmt.Errorf("deployarchive: read back the ledger put back from %s: %w", copyPath, err)
	}

	if led.Binding != "" && led.Binding != a.Manifest.DeploymentID {
		return unrestoredLedger(t.StateDir, copyPath, fmt.Sprintf(
			"the database is bound to %s after the put-back, not to this deployment", led.Binding))
	}

	// KEPT UNDER ANOTHER NAME, no-clobber: a second abandoned recovery of the same
	// archive leaves the first one's copy where it is and says so.
	aside := copyPath + ".abandoned"

	switch err := os.Link(copyPath, aside); {
	case err == nil:
		if err := os.Remove(copyPath); err != nil {
			return fmt.Errorf("deployarchive: remove %s after keeping it at %s: %w", copyPath, aside, err)
		}
	case errors.Is(err, os.ErrExist):
		aside = copyPath
	default:
		return fmt.Errorf("deployarchive: keep %s at %s: %w", copyPath, aside, err)
	}

	res.Restored = append(res.Restored, fmt.Sprintf("the ledger in %s, from the copy now kept at %s",
		dumpTargetPath(led.Schema), aside))

	return syncDir(t.StateDir)
}

// restoreVerifiedCopy hands pg_restore a copy of a dump that this call
// verified against want, never the pathname whose bytes were checked earlier.
//
// THE DIGEST IS CHECKED ON THE BYTES THE RESTORE READS, for the reason copyFile
// gives: a file verified by pathname and then reopened by pathname can be other
// bytes by then, and these bytes become the deployment's ledger. The copy is
// private to the state directory and goes when the restore is done.
func restoreVerifiedCopy(ctx context.Context, t Target, src string, want FileRecord,
	dumpVersion int, schema string,
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

	return stray, t.Postgres.Restore(ctx, staged, dumpVersion, schema)
}
