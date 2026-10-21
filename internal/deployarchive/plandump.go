package deployarchive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
)

// supersededDumpName is the base name a recovery's copy of the PostgreSQL
// ledger it replaces is published under, before supersededSuffix.
const supersededDumpName = "ledger.pgdump"

// dumpTargetPath names the database schema a dump lands in, for the plan's
// Path: there is no file, and the schema is what an operator compares.
func dumpTargetPath(schema string) string { return "postgresql schema " + schema }

// supersededDumpPath is where a recovery keeps its dump of the PostgreSQL ledger
// it replaced, named from the archive so every run of one recovery computes the
// same name, exactly as the SQLite ledger's superseded name is.
func supersededDumpPath(a *Archive, stateDir string) string {
	return filepath.Join(stateDir, supersededDumpName) + supersededSuffix(a)
}

// planDumpLedger decides what happens to the database for an archive carrying a
// PostgreSQL ledger's dump.
//
// WHAT IS IN THE DATABASE DECIDES, read through the target's own connection, and
// "could not tell" is a refusal. A restore installs the dump only into a schema
// with nothing in it or with only what a preflight wrote; a schema holding this
// deployment's ledger keeps it, because those rows are newer than the archive's;
// anything else is refused. A recovery replaces the ledger whatever it holds,
// keeping a dump of what it replaced, because putting a deployment back over
// itself is what it is for.
func (p *Plan) planDumpLedger(ctx context.Context) error {
	facts := p.Archive.Manifest.Ledger.Dump
	path := dumpTargetPath(facts.Schema)
	deployment := p.Archive.Manifest.DeploymentID

	// THE RECOVER GUARD FIRST, for the reason planLedger gives it: an empty
	// database would otherwise plan as an install, and a recovery would run
	// against a host that is not this deployment.
	if p.Intent == ReplaceLedger && p.dispositionOf(EntryIdentity) != AlreadyPresent {
		p.refuseRecoverOntoAnotherHost()

		return nil
	}

	// checkLedgerBackendAgrees has refused a target not configured for
	// PostgreSQL; one that is and has no way to reach it is a wiring defect.
	if p.Target.Postgres == nil {
		if p.Target.LedgerBackend == "postgres" {
			return errors.New("deployarchive: a PostgreSQL ledger's dump needs the database it is " +
				"restored into, and none was given")
		}

		return nil
	}

	led, err := p.Target.Postgres.Peek(ctx)
	if err != nil {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("billet could not read the PostgreSQL ledger %s names: %v",
				p.Target.ConfigPath, err),
			Remedy: "resolve that first; \"billet could not tell\" is never permission to write a " +
				"ledger into a database",
		})

		return nil
	}

	switch {
	case led.Schema == "":
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("the connection %s names resolves to no schema that exists, and this "+
				"backup's ledger was in schema %s", p.Target.ConfigPath, facts.Schema),
			Remedy: fmt.Sprintf("create schema %s in that database, owned by the role billet "+
				"connects as, and run this again", facts.Schema),
		})

		return nil
	case led.Schema != facts.Schema:
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("this backup's ledger was in schema %s and the connection %s names "+
				"resolves to schema %s", facts.Schema, p.Target.ConfigPath, led.Schema),
			Remedy: fmt.Sprintf("pg_restore puts the ledger back into the schema it came from; "+
				"point the connection's search_path at %s", facts.Schema),
		})

		return nil
	}

	// A RUN THAT STARTED WRITING SOMEWHERE IS HELD TO IT before any disposition
	// is chosen: a restore resumed after its pg_restore committed and before its
	// journal said so would otherwise plan the ledger it finds as one to keep,
	// in whatever database the connection reaches by then.
	if p.refuseAnotherDatabase(led) {
		return nil
	}

	// A RESUME FINDS ITS OWN WORK. A run interrupted after its pg_restore
	// committed left a populated schema that, on a ledger never bound, nothing
	// else here could tell from somebody else's; the journal can. And the
	// database is proved to be the one that work went into, because nothing
	// after this point restores anything: what follows seals the ledger the
	// connection reaches and opens the fence over it.
	if prog, restored := p.ledgerRestoredByThisRun(); restored {
		if err := checkRestoredLedger(p.Archive, prog.LedgerDatabase, led); err != nil {
			p.refuse(lifeops.Refusal{
				What: fmt.Sprintf("this %s restored the ledger once already, and the database the "+
					"connection %s names is not proved to be the one it went into: %v",
					p.Intent, p.Target.ConfigPath, err),
				Remedy: "point the connection back at the database this run restored into, or " +
					"abandon it and reconcile the ledger by hand",
			})

			return nil
		}

		// A RECOVERY STILL NAMES ITS COPY, so a resumed run hands it to the service
		// account and reports where it is, as the first run would have, and it
		// must still be there: it is the only record of the ledger replaced.
		if p.Intent == ReplaceLedger {
			if p.refuseMissingCopy(prog) {
				return nil
			}

			p.Superseded = supersededDumpPath(p.Archive, p.Target.StateDir)
		}

		p.add(Action{Entry: EntryLedgerDump, Path: path, What: "ledger", Disposition: AlreadyPresent})

		return nil
	}

	// A RECOVERY REPLACES THE LEDGER WHATEVER ITS SCHEMA'S AGE: Restore drops the
	// tables the archive's dump does not name before it restores, so a ledger a
	// later migration widened comes back as the archive's whole, not as a mix.
	//
	// AND ONLY A LEDGER BOUND TO THIS DEPLOYMENT, FROM AN ARCHIVE BOUND TO IT. The
	// binding is the one fact that names this deployment's ledger wherever the
	// connection points, so it is what every later pass (a resume, an abandon,
	// the seal) holds the database to; a ledger no controller ever claimed has
	// served nothing a recovery could put back.
	if p.Intent == ReplaceLedger {
		switch {
		case p.refuseHierarchy(led):
			return nil
		case slices.ContainsFunc(led.Tables, func(t string) bool { return strings.ContainsFunc(t, unicode.IsSpace) }):
			// REFUSED BEFORE THE QUIESCE AND THE FENCE: the copy this recovery takes
			// of the ledger it replaces could not be restored, so an abandon could
			// never put it back.
			p.refuse(lifeops.Refusal{
				What: fmt.Sprintf("schema %s holds a table with whitespace in its name, so a dump of "+
					"it could not be restored unambiguously", led.Schema),
				Remedy: "move that table out of the ledger's schema and run this again",
			})

			return nil
		case led.Binding != "" && led.Binding != deployment:
			p.refuseForeignLedger(led.Binding)

			return nil
		case led.Binding == "":
			p.refuse(lifeops.Refusal{
				What: fmt.Sprintf("the ledger %s names is bound to no deployment, so billet cannot "+
					"prove it is %s's", p.Target.ConfigPath, deployment),
				Remedy: "`billet local recover` puts a served deployment back over its own ledger; " +
					"for a ledger no controller has claimed, `billet local restore` into an empty " +
					"schema is the operation",
			})

			return nil
		case facts.Binding != deployment:
			p.refuse(lifeops.Refusal{
				What: fmt.Sprintf("this backup's ledger is bound to %q, not to %s, so a recovery "+
					"from it could not prove which ledger it put back", facts.Binding, deployment),
				Remedy: "recover from a backup taken after this deployment's controller first started",
			})

			return nil
		}

		p.Superseded = supersededDumpPath(p.Archive, p.Target.StateDir)
		p.add(Action{Entry: EntryLedgerDump, Path: path, What: "ledger", Disposition: SupersedeLedger})

		return nil
	}

	archived := p.Archive.Manifest.Ledger.HighestVersion()

	switch {
	case len(led.Tables) == 0:
		// A HIERARCHY THE ROLE CANNOT SEE: information_schema lists only the
		// tables billet's role has a privilege on, and the catalogue every one.
		if !p.refuseHierarchy(led) {
			p.add(Action{Entry: EntryLedgerDump, Path: path, What: "ledger", Disposition: Install})
		}
	case (led.Binding == deployment || led.Populated && p.Target.ExternalLedgerAttached) &&
		!slices.Contains(led.Tables, "schema_migrations"):
		// KEPT ONLY IF IT IS A BILLET LEDGER AT ALL. Rows with no migration
		// bookkeeping are some other program's tables, and keeping them would
		// leave this deployment to start on an empty ledger beside them.
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("schema %s holds tables and no schema_migrations, so it is not a "+
				"billet ledger", led.Schema),
			Remedy: "point the connection at this deployment's ledger, or at an empty schema for " +
				"this backup's to be restored into",
		})
	case (led.Binding == deployment || led.Populated && p.Target.ExternalLedgerAttached) &&
		highestOf(led.Migrations) < archived:
		// PROVABLY OLDER THAN THE BACKUP: its schema is behind the archive's, so
		// it was restored from something taken before this backup, and keeping it
		// would throw away rows the backup holds.
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("the ledger in schema %s is at migration %d and this backup's at %d, "+
				"so the database is older than the backup", led.Schema,
				highestOf(led.Migrations), archived),
			Remedy: "empty that schema, and this restore puts the backup's ledger there; or " +
				"restore from a backup no newer than the database",
		})
	case led.Binding == deployment:
		p.add(Action{Entry: EntryLedgerDump, Path: path, What: "ledger", Disposition: KeepLiveLedger})
	case led.Binding != "":
		p.refuseForeignLedger(led.Binding)
	case led.Populated && p.Target.ExternalLedgerAttached:
		// A LEDGER NEVER BOUND CANNOT SAY WHOSE IT IS, so the operator says it,
		// with the flag an identity-only archive already asks for. Its rows are
		// kept for the reason a bound one's are.
		p.add(Action{Entry: EntryLedgerDump, Path: path, What: "ledger", Disposition: KeepLiveLedger})
	case led.Populated:
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("schema %s already holds deployment data (%s), bound to no deployment, "+
				"so billet cannot tell whether it is this backup's ledger", led.Schema,
				joinOrNone(led.NonEmpty)),
			Remedy: "if it is this deployment's own ledger, which the database kept when the host " +
				"did not, run this again with --external-ledger-attached and it is kept as it is. " +
				"Otherwise empty that schema deliberately; billet replaces only a ledger nothing " +
				"has written to",
		})
	case !slices.Contains(led.Tables, "schema_migrations") || len(led.Migrations) == 0:
		// EMPTY IS NOT PRISTINE. Tables with no rows and no migration bookkeeping
		// are somebody's schema, and the restore would drop them with no copy
		// taken; only a ledger billet migrated is replaced as one.
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("schema %s holds tables (%s) and no billet migrations, so it is not a "+
				"ledger billet prepared", led.Schema, joinOrNone(led.Tables)),
			Remedy: "point the connection at an empty schema for this backup's ledger to be " +
				"restored into",
		})
	case newerThanArchive(led.Migrations, p.Archive.Manifest.Ledger.HighestVersion()):
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("schema %s was prepared by a billet newer than the one that took this "+
				"backup (migration %d against the backup's %d)", led.Schema,
				highestOf(led.Migrations), p.Archive.Manifest.Ledger.HighestVersion()),
			Remedy: "pg_restore would leave that billet's newer tables beside the restored ledger; " +
				"empty the schema and run this again",
		})
	case p.refuseHierarchy(led):
	default:
		p.add(Action{Entry: EntryLedgerDump, Path: path, What: "ledger", Disposition: ReplaceEmptyLedger})
	}

	return nil
}

// refuseHierarchy refuses, and reports that it did, a schema holding a table
// hierarchy, for the dispositions that replace the ledger in it.
//
// REFUSED AT THE PLAN, before a recovery seals, fences or copies anything: the
// cleanup refuses a hierarchy anywhere in the schema, and so would an abandon's
// put-back, which would leave the deployment fenced over a database billet never
// changed. A restore that keeps the database's own ledger writes nothing there
// and is not refused.
func (p *Plan) refuseHierarchy(led state.PostgresLedger) bool {
	// A SCHEMA NAME pg_restore's LIST CANNOT CARRY is refused here too, for the
	// same reason: pgdump.Restore refuses it, after a recovery has sealed and
	// fenced, and so would the abandon's put-back.
	if strings.ContainsFunc(led.Schema, unicode.IsSpace) {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("the ledger's schema %q has whitespace in its name, which billet cannot "+
				"restore a dump into unambiguously", led.Schema),
			Remedy: "move the ledger to a schema whose name has no whitespace, take a backup there, " +
				"and run this again",
		})

		return true
	}

	if len(led.Hierarchy) == 0 {
		return false
	}

	p.refuse(lifeops.Refusal{
		What: fmt.Sprintf("schema %s holds %s, which is partitioned, a partition or an inheriting "+
			"table; billet's ledger never holds one, and replacing the ledger would take rows "+
			"outside the schema with it", led.Schema, joinOrNone(led.Hierarchy)),
		Remedy: "move that table out of the ledger's schema, or point the connection at a schema " +
			"of the ledger's own, and run this again",
	})

	return true
}

// refuseMissingCopy refuses, and reports that it did, when a recovery's
// published copy of the ledger it replaced is gone or changed.
func (p *Plan) refuseMissingCopy(prog Progress) bool {
	err := requirePublishedCopy(p.Archive, p.Target.StateDir, prog.SupersededDigests, prog.SupersedePublished)
	if err == nil {
		return false
	}

	p.refuse(lifeops.Refusal{
		What:   err.Error(),
		Remedy: "put the copy back at that name, or abandon the recovery",
	})

	return true
}

// refuseAnotherDatabase refuses, and reports that it did, when this run's
// journal records a database it began writing in and the connection now
// reaches another. A journal billet cannot read is "could not tell" and refuses
// too: what it would have said decides whether this database may be written.
func (p *Plan) refuseAnotherDatabase(led state.PostgresLedger) bool {
	prog, err := InProgress(p.Target.StateDir)

	switch {
	case err != nil:
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("billet could not read this directory's journal to tell which database "+
				"an earlier run began writing in: %v", err),
			Remedy: "resolve that first; \"could not tell\" is never permission to write a ledger",
		})

		return true
	case !prog.IsFor(p.Archive) || !prog.IsFrom(p.Intent) || prog.LedgerDatabase == "":
		return false
	case led.Database != prog.LedgerDatabase:
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("this %s began writing the ledger in database %s, and the connection %s "+
				"names now reaches %s", p.Intent, prog.LedgerDatabase, p.Target.ConfigPath, led.Database),
			Remedy: "point the connection back at the database this run began in, or abandon it",
		})

		return true
	}

	return false
}

// ledgerRestoredByThisRun reports whether this directory's journal records that
// this archive's dump was already restored by this operation. A journal billet
// cannot read answers no, which plans the database afresh: the safe direction,
// since what that plan can do to a populated schema is refuse.
func (p *Plan) ledgerRestoredByThisRun() (Progress, bool) {
	prog, err := InProgress(p.Target.StateDir)
	if err != nil {
		return Progress{}, false
	}

	return prog, prog.IsFor(p.Archive) && prog.IsFrom(p.Intent) && prog.LedgerRestored
}

// refuseRecoverOntoAnotherHost is a recovery planned on a host that is not
// already the archive's deployment.
func (p *Plan) refuseRecoverOntoAnotherHost() {
	p.refuse(lifeops.Refusal{
		What: fmt.Sprintf("%s is not already deployment %s, and `billet local recover` puts "+
			"a deployment back over ITSELF", p.Target.StateDir, p.Archive.Manifest.DeploymentID),
		Remedy: "use `billet local restore`, which is the operation for a host that is not " +
			"already this deployment — it refuses a commissioned one, which is the other " +
			"half of the same rule",
	})
}

// refuseForeignLedger is a database whose rows are bound to another deployment.
func (p *Plan) refuseForeignLedger(bound string) {
	p.refuse(lifeops.Refusal{
		What: fmt.Sprintf("the ledger %s names is bound to deployment %s, and this backup is of %s",
			p.Target.ConfigPath, bound, p.Archive.Manifest.DeploymentID),
		Remedy: "point the connection at this deployment's own database or schema. Billet will not " +
			"replace another deployment's capacity record, and a control plane would refuse to " +
			"serve these rows beside this identity anyway",
	})
}

func newerThanArchive(applied []state.AppliedMigration, archived int) bool {
	return highestOf(applied) > archived
}

func highestOf(applied []state.AppliedMigration) int {
	return LedgerFacts{Migrations: applied}.HighestVersion()
}

func joinOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}

	return fmt.Sprint(s)
}
