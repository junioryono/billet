package deployarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// Target is the deployment a restore would land on.
//
// PATHS AND IDENTITY, NOT A *config.Config. The planner has to be callable
// against a host whose config is being reasoned about rather than loaded, and
// narrowing the input is what keeps this function pure — everything it needs is
// visible in its signature.
type Target struct {
	ConfigPath string
	StateDir   string
	AppKeyPath string
	GitHub     GitHubIdentity

	// LedgerBackend is what THIS host's config says its ledger is — "sqlite",
	// "postgres", or empty for a config that names none (which is sqlite).
	//
	// COMPARED AGAINST THE ARCHIVE'S, because the halves have to be paired and
	// this is the half billet can actually check. An identity-only archive
	// restored onto a host whose config says the ledger is local produces a
	// control plane that mints a FRESH empty ledger beside a restored identity —
	// which is the same lost fleet as restoring an identity with no ledger at
	// all, arriving through a config mistake instead of a missing file.
	LedgerBackend string

	// ExternalLedgerAttached is the operator asserting that the ledger this
	// archive's identity belongs to is back and reachable.
	//
	// REQUIRED FOR AN IDENTITY-ONLY ARCHIVE, and it is a flag rather than a check
	// because there is nothing here to check: the database is on the other end of
	// a DSN this process has not been given, and its restore is pg_dump or the
	// provider's snapshot — the operator's to run. Billet verifies what it can
	// (the backend the config names) and asks about exactly the part it cannot.
	//
	// A ledger that is NOT back produces a control plane that starts against an
	// empty database holding a restored identity: it advertises capacity for a
	// fleet it has no record of, and reaps as orphans the compute the old one
	// launched.
	ExternalLedgerAttached bool
}

// Disposition is what a restore would do with one item.
type Disposition int

const (
	// Install means the destination is free and the archive's copy goes there.
	Install Disposition = iota
	// AlreadyPresent means the destination already holds exactly these bytes, so
	// there is nothing to do. This is what makes a resume after an interruption
	// converge rather than refuse.
	AlreadyPresent
	// ReplaceEmptyLedger is the ONE destructive disposition a RESTORE can reach,
	// and it is reachable only for a ledger `billet check` created on a host
	// nobody has commissioned: no deployment identity, no authority, and every
	// table provably empty.
	ReplaceEmptyLedger
	// SupersedeLedger renames a POPULATED ledger aside and installs the archive's
	// in its place. Only `billet local recover` reaches it, only for the
	// deployment the archive already belongs to, and only after that deployment
	// has been sealed and proved to hold nothing.
	//
	// THE OLD LEDGER IS RENAMED, NEVER UNLINKED. It is the only record of the
	// jobs this operation fails, and an operator who has just accepted losing
	// them needs to be able to say which they were.
	SupersedeLedger
)

func (d Disposition) String() string {
	switch d {
	case Install:
		return "install"
	case AlreadyPresent:
		return "already present"
	case ReplaceEmptyLedger:
		return "replace the empty preflight ledger"
	case SupersedeLedger:
		return "supersede this deployment's ledger"
	default:
		return "unknown"
	}
}

// Intent is which operation a plan is for.
//
// IT TRAVELS ON THE PLAN because the executor RE-DERIVES the plan inside its
// exclusion and acts only on that one — so the intent has to be recoverable
// there, or a recover would re-plan as a restore and refuse its own target.
type Intent int

const (
	// RestoreFresh is `billet local restore`: put a deployment onto a host that
	// is not already one.
	RestoreFresh Intent = iota
	// ReplaceLedger is `billet local recover`: put a deployment back over ITSELF,
	// replacing a ledger that has rows in it.
	//
	// A SEPARATE OPERATION RATHER THAN A FLAG, and this is why: the restored
	// ledger has no lease for compute created after the backup, so node recovery
	// destroys those instances as orphans and GitHub does not requeue a job that
	// already started. That is not something a flag on the ordinary path should
	// be able to reach.
	ReplaceLedger
)

// valid refuses an Intent that is not one of the two.
//
// AT EVERY EXPORTED MUTATION, before a fence is raised or a file is written. The
// CLI only ever builds the two, but Plan is a struct anybody can construct, and
// an out-of-range Intent used to be indistinguishable from RestoreFresh — which
// meant a journal recording an operation billet does not have, refused by
// understood on every subsequent resume and abandon. Publishing a record nothing
// can act on is worse than refusing to start.
func (i Intent) valid() error {
	switch i {
	case RestoreFresh, ReplaceLedger:
		return nil
	default:
		return fmt.Errorf("deployarchive: %s is not an operation billet has", i)
	}
}

// String is the word an operator sees, and it is also what the journal records
// and what two readers compare — so an unrecognised value gets its OWN spelling
// rather than the nearest real one. Aliasing it to "restore" made every invalid
// Intent a valid record of the wrong operation, in the field that decides which
// abandon may act.
func (i Intent) String() string {
	switch i {
	case RestoreFresh:
		return "restore"
	case ReplaceLedger:
		return "recover"
	default:
		return fmt.Sprintf("unknown intent %d", int(i))
	}
}

// Action is one thing a restore would do.
type Action struct {
	// Entry is the archive entry this action installs, and it is the action's
	// IDENTITY: exactly one action exists per entry, and it is never empty. The
	// ledger's sidecars are not actions — they are removed by name at execution
	// time, because what the planner saw is stale by then. actionsDiffer keys on
	// this, so a duplicate or an empty one would let a changed action hide.
	Entry string
	// Path is where it lands.
	Path string
	// What names the item in the words an operator uses.
	What        string
	Disposition Disposition
}

// Plan is what a restore would do, and every reason it will not.
//
// PURE. Building one opens no ledger through state.Open or state.OpenAdmin —
// both create and chmod directories, take the process lock and MIGRATE, so
// using either to ask a question would upgrade a stopped ledger on the way to
// telling the operator the restore is refused.
type Plan struct {
	Archive *Archive
	Target  Target
	// Intent is which operation this plan is for. The executor re-plans with it
	// rather than assuming a restore.
	Intent Intent

	Actions  []Action
	Refusals []lifeops.Refusal

	// Superseded is where a replaced ledger is renamed to. Set only under
	// ReplaceLedger, and reported so an operator knows where the record of the
	// jobs they accepted losing has gone.
	Superseded string

	// LedgerSidecars are the -wal and -shm files that must go with a replaced
	// ledger. Orphaning a -wal beside a restored billet.db would corrupt it.
	LedgerSidecars []string
}

// Nothing reports whether every item is already in place.
func (p *Plan) Nothing() bool {
	for _, a := range p.Actions {
		if a.Disposition != AlreadyPresent {
			return false
		}
	}

	return true
}

// PlanRestore decides what would happen, and refuses everything it must.
//
// EVERY REFUSAL IS COLLECTED RATHER THAN RETURNED AT THE FIRST ONE. A restore
// is run under pressure and each re-run to discover the next problem costs an
// outage minutes; the command layer renders them together.
//
// An error, as opposed to a refusal, means billet could not LOOK. Those two are
// kept apart on purpose: "I could not read the target" must never become "the
// target is empty", because what follows an empty answer is writing credentials
// into it.
func PlanRestore(ctx context.Context, a *Archive, t Target) (Plan, error) {
	return planFor(ctx, a, t, RestoreFresh)
}

// PlanRecover decides what `billet local recover` would do.
//
// THE SAME PLANNER, ONE INTENT APART, and that is the whole design: the
// identity, the authority and the App key are decided by exactly the code a
// restore uses — every one of them will read AlreadyPresent on a healthy target,
// and any difference is refused there as it always was. The only thing that
// moves is the ledger, which a restore refuses and this supersedes.
func PlanRecover(ctx context.Context, a *Archive, t Target) (Plan, error) {
	return planFor(ctx, a, t, ReplaceLedger)
}

func planFor(ctx context.Context, a *Archive, t Target, intent Intent) (Plan, error) {
	if a == nil {
		return Plan{}, errors.New("deployarchive: a restore plan needs an archive")
	}

	if t.StateDir == "" {
		return Plan{}, errors.New(
			"deployarchive: this config declares no control plane, so there is nowhere on this " +
				"host to restore a deployment to")
	}

	if err := intent.valid(); err != nil {
		return Plan{}, err
	}

	p := Plan{Archive: a, Target: t, Intent: intent}

	p.checkBinaryUnderstandsArchive()
	p.checkConfigNamesThisApp()
	p.checkLedgerBackendAgrees()

	p.planIdentity()

	if err := p.planLedger(ctx); err != nil {
		return Plan{}, err
	}

	p.planAuthority()
	p.planAppKey()

	return p, nil
}

// checkBinaryUnderstandsArchive refuses a ledger written by a NEWER billet.
//
// ITS OWN DIAGNOSTIC, DELIBERATELY NOT state.ErrSchemaBehind. That error means
// something else entirely — a running control plane is holding a ledger this
// binary would have to migrate — and its remedy is a restart. This one's remedy
// is a newer binary, and collapsing the two sends an operator to restart a
// service that is not the problem.
//
// An archive BEHIND this binary is fine and is not a refusal: the next
// `billet server` start migrates it, exactly as it would any older ledger.
func (p *Plan) checkBinaryUnderstandsArchive() {
	if err := state.RefuseUnknownVersions(p.Archive.Manifest.Ledger.Migrations); err != nil {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf(
				"the ledger in this backup carries a migration this billet has never heard of "+
					"(%v); it was written by a newer version (%s)",
				err, orUnrecorded(p.Archive.Manifest.BilletVersion)),
			Remedy: "restore it with a billet at least as new as the one that wrote it — this " +
				"binary cannot know what those rows mean",
		})
	}
}

// checkConfigNamesThisApp refuses a key paired with unrelated configuration.
//
// THE KEY FILE SAYS NOTHING ABOUT WHICH APP IT IS FOR. Installing this backup's
// key beside a config naming a different app_id produces a control plane that
// authenticates as nothing and reports a bare 401 on its first poll, hours after
// whoever ran the restore has gone home.
func (p *Plan) checkConfigNamesThisApp() {
	if p.Target.GitHub.Same(p.Archive.Manifest.GitHub) {
		return
	}

	// NO App AT ALL IS ITS OWN SENTENCE. Rendering the zero value produces "org
	// , app 0, installation 0", which reads as a corrupt config rather than an
	// absent section and sends an operator looking for the wrong thing.
	if (p.Target.GitHub == GitHubIdentity{}) {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s has no github section, and this backup is of a deployment on %s",
				p.Target.ConfigPath, p.Archive.Manifest.GitHub),
			Remedy: "the App key is part of the deployment unit, not an extra — add the github " +
				"block this backup belongs to, or point --config at the configuration that has it",
		})

		return
	}

	p.refuse(lifeops.Refusal{
		What: fmt.Sprintf(
			"this backup is of a deployment on %s and %s names %s",
			p.Archive.Manifest.GitHub, p.Target.ConfigPath, p.Target.GitHub),
		Remedy: "point --config at the configuration this backup belongs to, or correct the " +
			"github block; billet will not install an App key beside a config for another App",
	})
}

// planIdentity decides what happens to the deployment identity.
//
// A DIFFERENT IDENTITY IS ALWAYS A REFUSAL, whatever else is true. This is
// AdoptDeploymentID's existing invariant: relabelling a directory makes the
// compute it is already managing invisible to both installations, so its leases
// expire, its capacity is resold, and it runs forever. There is deliberately no
// flag for it — a destructive replacement, after proving the old deployment
// holds nothing, is a separate operation.
func (p *Plan) planIdentity() {
	path := filepath.Join(p.Target.StateDir, "deployment-id")
	want := p.Archive.Manifest.DeploymentID

	have, found, err := state.PeekDeploymentID(p.Target.StateDir)
	if err != nil {
		// COULD NOT TELL. An unreadable or malformed identity file is not an
		// absent one, and treating it as absent would install a second identity
		// beside whatever is there.
		p.refuse(lifeops.Refusal{
			What:   fmt.Sprintf("billet could not read the deployment identity at %s: %v", path, err),
			Remedy: "look at that file; billet will not write an identity beside one it cannot read",
		})

		return
	}

	switch {
	case !found:
		p.add(Action{Entry: EntryIdentity, Path: path, What: "deployment identity",
			Disposition: Install})
	case have == want:
		p.add(Action{Entry: EntryIdentity, Path: path, What: "deployment identity",
			Disposition: AlreadyPresent})
	default:
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf(
				"%s says this host belongs to deployment %s, and this backup is of %s",
				path, have, want),
			Remedy: "restore onto a host that is not already a different deployment. Billet will " +
				"not relabel this one: the compute it is already managing carries the old " +
				"identity and would become invisible to both installations",
		})
	}
}

// strayLedgerSidecars names the SQLite sidecars sitting beside an absent ledger.
//
// LSTAT RATHER THAN Stat, and anything that is THERE counts: what makes this
// dangerous is a file SQLite will open, and billet is deciding whether it may
// write a database next to it rather than what the thing is.
//
// ONLY ErrNotExist IS ABSENCE. Folding every other error into "there is nothing
// there" is the could-not-tell/no collapse, and here it ends in installing a
// database beside a write-ahead log billet failed to look at — the exact
// corruption this exists to prevent, reached by the guard itself.
func strayLedgerSidecars(ledgerPath string) ([]string, error) {
	var stray []string

	for _, sidecar := range LedgerSidecarPaths(ledgerPath) {
		switch _, err := os.Lstat(sidecar); {
		case err == nil:
			stray = append(stray, sidecar)
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, fmt.Errorf("deployarchive: inspect %s: %w", sidecar, err)
		}
	}

	return stray, nil
}

// abandonRemedy names the command that would actually undo what is here, or
// nothing.
//
// THE JOURNAL'S OWN INTENT DECIDES, not "a journal exists". A restore's abandon
// clears a restore's fence and never reaches a recovery's put-back, and a
// FINISHED journal is refused by both — so answering "there is a journal, run
// abandon" sends an operator to a command that reports success and moves
// nothing. An unreadable journal answers nothing, which is the safe direction:
// the manual remedy is always correct.
func abandonRemedy(stateDir string) string {
	prog, err := InProgress(stateDir)
	if err != nil {
		return ""
	}

	for _, intent := range []Intent{ReplaceLedger, RestoreFresh} {
		if prog.AbandonFinishesIt(intent) {
			return "billet local " + intent.String() + " --abandon --from " + prog.ArchiveDir
		}
	}

	return ""
}

// planLedger decides what happens to billet.db.
//
// THE FILE'S PRESENCE CANNOT BE THE TEST, and that is not a concession. A
// preflight `billet check` creates billet.db and its schema on a host nobody has
// commissioned, which is the documented way to prepare one — so refusing on the
// file would refuse the main case this command exists for. What proves a
// directory is committed is the deployment identity and the authority marker,
// and what proves the ledger beside them is the preflight's is that every table
// a deployment writes to is empty.
func (p *Plan) planLedger(ctx context.Context) error {
	if p.Archive.Manifest.Ledger.IsExternal() {
		p.planExternalLedger()

		return nil
	}

	path := LedgerPath(p.Target.StateDir)

	// A RECOVER IS REFUSED BEFORE THE FILE IS EVEN LOOKED AT, and that ordering
	// is not cosmetic: an absent ledger takes the Install branch below and
	// returns, so a guard placed after it let a recover run against an
	// uncommissioned host — where the only reason it was caught at all is that
	// sealing the deployment CREATES a ledger, so the executor's re-plan then
	// refused what the printed plan had allowed.
	//
	// THE ARCHIVE'S OWN DEPLOYMENT, PROVED BY THE IDENTITY ALREADY ON DISK.
	// planIdentity ran first and reports AlreadyPresent only when the file there
	// names exactly the deployment this archive is of — so asking it is asking
	// whether this host IS the deployment being put back.
	if p.Intent == ReplaceLedger && p.dispositionOf(EntryIdentity) != AlreadyPresent {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s is not already deployment %s, and `billet local recover` puts "+
				"a deployment back over ITSELF", p.Target.StateDir,
				p.Archive.Manifest.DeploymentID),
			Remedy: "use `billet local restore`, which is the operation for a host that is not " +
				"already this deployment — it refuses a commissioned one, which is the other " +
				"half of the same rule",
		})

		return nil
	}

	info, err := os.Lstat(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		// AN ABSENT LEDGER WITH ITS SIDECARS STILL THERE IS NOT AN EMPTY
		// DIRECTORY. SQLite replays a write-ahead log it finds beside a database,
		// so installing the archive's billet.db next to a -wal belonging to a
		// different one is corruption rather than a stale file — and that state is
		// reachable: it is what a supersede interrupted between its -wal and its
		// ledger leaves behind, and what any older billet that moved them in one
		// unordered batch could leave. Refusing here is what stops a re-plan
		// walking into it, and the remedy is a person's because billet cannot tell
		// which database that log belongs to.
		stray, err := strayLedgerSidecars(path)
		if err != nil {
			return err
		}

		if len(stray) > 0 {
			// THE REMEDY IS CONDITIONAL, because only one of the two actually does
			// anything: an abandon with no journal here clears the fence it raised
			// and never reaches the put-back, so naming it would send an operator
			// to a command that reports success and moves nothing.
			remedy := "move those files aside once you have decided which database " +
				"they belong to — billet will not put one down beside a log it cannot " +
				"account for. Look for a billet.db.superseded-* file next to them: an " +
				"interrupted `billet local recover` leaves exactly this state"

			if undo := abandonRemedy(p.Target.StateDir); undo != "" {
				remedy = "an interrupted operation's journal is still here, so `" + undo +
					"` undoes it and puts back what it moved. Otherwise " + remedy
			}

			p.refuse(lifeops.Refusal{
				What: fmt.Sprintf("%s does not exist, but %s does. SQLite replays a write-ahead "+
					"log it finds beside a database, and that one belongs to a ledger that is "+
					"no longer there", path, strings.Join(stray, " and ")),
				Remedy: remedy,
			})

			return nil
		}

		p.add(Action{Entry: EntryLedger, Path: path, What: "ledger", Disposition: Install})

		return nil
	case err != nil:
		p.refuse(lifeops.Refusal{
			What:   fmt.Sprintf("billet could not inspect the ledger at %s: %v", path, err),
			Remedy: "resolve that before restoring; billet will not write a ledger beside one it cannot see",
		})

		return nil
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		p.refuse(lifeops.Refusal{
			What:   fmt.Sprintf("%s is not a regular file", path),
			Remedy: "move it aside; a ledger is a file billet opens, not a link to one",
		})

		return nil
	}

	// ALREADY THIS ARCHIVE'S LEDGER, which is what makes a resume converge. A
	// run interrupted after the ledger landed and before the authority did must
	// be able to continue, and by then the target IS commissioned — it has the
	// identity this restore installed a step earlier — so the refusal below
	// would otherwise fire on the restore's own work.
	//
	// COMPARED BY DIGEST, not by "a restore wrote something here". Once a
	// control plane has started on it the file diverges immediately, and at that
	// point it is a live capacity record and the refusal is right again.
	if same, err := p.ledgerIsTheArchives(path); err != nil {
		p.refuse(lifeops.Refusal{
			What:   fmt.Sprintf("billet could not read the ledger at %s: %v", path, err),
			Remedy: "look at that file; billet will not replace a ledger it cannot read",
		})

		return nil
	} else if same {
		p.add(Action{Entry: EntryLedger, Path: path, What: "ledger", Disposition: AlreadyPresent})

		return nil
	}

	committed, why := p.committedTarget()

	// THE TWO INTENTS DIVERGE HERE AND NOWHERE ELSE.
	//
	// A restore refuses a commissioned deployment. A recover REQUIRES one — it
	// exists to put a deployment back over itself — and its guard is the mirror
	// image: the target must be this archive's own deployment, which planIdentity
	// has already established by comparing the identity file against the
	// manifest. So a recover can never relabel a host, which is the invariant
	// AdoptDeploymentID states and the reason this is a separate operation
	// rather than a flag.
	if p.Intent == ReplaceLedger {
		p.Superseded = path + supersededSuffix(p.Archive)
		p.add(Action{
			Entry: EntryLedger, Path: path, What: "ledger", Disposition: SupersedeLedger,
		})

		return nil
	}

	if committed {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s already holds a ledger, and this host is a commissioned "+
				"deployment (%s)", path, why),
			Remedy: "billet will not replace a deployment's capacity record: the restored ledger " +
				"has no lease for compute created since the backup, so node recovery would " +
				"destroy live jobs as orphans. `billet local recover` is the operation for " +
				"putting this deployment back over itself — it seals the deployment, waits for " +
				"it to hold nothing, and names every job it would fail",
		})

		return nil
	}

	contents, err := state.PeekLedger(ctx, path)
	if err != nil {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s exists and billet could not read it to tell whether it holds "+
				"anything: %v", path, err),
			Remedy: "look at that file; \"billet could not tell\" is never permission to delete a ledger",
		})

		return nil
	}

	if contents.Populated {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s already holds deployment data (%s)", path,
				strings.Join(contents.NonEmpty, ", ")),
			Remedy: "billet only replaces a ledger a preflight created and nothing has written " +
				"to. Move this state directory aside deliberately if you mean to discard it",
		})

		return nil
	}

	p.add(Action{Entry: EntryLedger, Path: path, What: "ledger", Disposition: ReplaceEmptyLedger})

	// REPORTED, NOT ACTED ON. The executor removes the sidecars by NAME rather
	// than from this list, because PeekLedger above opens the database and SQLite
	// deletes the -wal and -shm when that connection closes — so what this loop
	// finds is usually already gone by the time a restore runs. It is here so a
	// --dry-run can say what is beside the ledger right now.
	for _, suffix := range ledgerSidecarSuffixes {
		if _, err := os.Lstat(path + suffix); err == nil {
			p.LedgerSidecars = append(p.LedgerSidecars, path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("deployarchive: inspect %s: %w", path+suffix, err)
		}
	}

	return nil
}

// planExternalLedger decides an archive that carries no ledger because the
// deployment's ledger is somebody else's to back up.
//
// IT ADDS NO ACTION, and that is the point rather than an omission: there is
// nothing to install, and every later stage keys off the action list — the
// publication order, the journal's digests, the abandon's put-back. An action
// with nothing behind it would have to be special-cased in all three.
//
// WHAT IT ADDS INSTEAD IS TWO REFUSALS, one billet can prove and one it cannot.
func (p *Plan) planExternalLedger() {
	// A RECOVER HAS NO MEANING HERE. `billet local recover` exists to put a
	// deployment back over ITSELF, and the only thing it moves is the ledger:
	// it seals, waits for quiescence, renames the live billet.db aside and
	// installs the archive's in its place. None of that exists for a database
	// billet does not hold — and the seal itself goes through a SQLite handle,
	// so the operation could not run even if the ledger half were skipped.
	if p.Intent == ReplaceLedger {
		p.refuse(lifeops.Refusal{
			What: "this backup's ledger is external (" + p.archiveLedgerBackend() +
				"), and `billet local recover` is an operation on the ledger — it seals the " +
				"deployment, waits for it to go quiet, and renames the live one aside",
			Remedy: "restore the identity with `billet local restore` onto a host that is not " +
				"already this deployment, and put the ledger back with your database's own " +
				"restore. There is no billet operation that replaces an external ledger " +
				"in place",
		})

		return
	}

	// THE HALF BILLET CAN CHECK IS checkLedgerBackendAgrees, WHICH RUNS FOR EVERY
	// ARCHIVE RATHER THAN ONLY THIS ONE — see there for why the reverse direction
	// matters just as much.
	//
	// THE HALF IT CANNOT IS HERE. Whether the database on the other end of that DSN
	// is the one this identity was minted beside is not something this process
	// can establish, so it is asked rather than assumed.
	if !p.Target.ExternalLedgerAttached {
		p.refuse(lifeops.Refusal{
			What: "this backup holds the deployment identity, the node-wire authority and the " +
				"App key, and NOT the ledger — that one is " + p.archiveLedgerBackend() +
				", which is your database's own backup to restore",
			Remedy: "restore the ledger first, then run this again with " +
				"--external-ledger-attached to say so. Billet cannot check it: the database is " +
				"on the other end of a connection string this command has not been given, and " +
				"an identity restored beside an EMPTY ledger is a control plane that reaps the " +
				"fleet it should have adopted",
		})
	}
}

// checkLedgerBackendAgrees refuses an archive whose ledger this host is not
// configured for, IN EITHER DIRECTION.
//
// IT IS NOT ABOUT EXTERNAL LEDGERS, WHICH IS WHY IT IS NOT INSIDE
// planExternalLedger. The first version put it there and covered one direction:
// an identity-only archive landing on a host configured for a local ledger,
// where the control plane creates an empty billet.db beside the restored
// identity and starts against it.
//
// THE REVERSE IS THE SAME FAILURE AND WAS UNGUARDED — found by review. Restoring
// a SQLite archive onto a PostgreSQL-configured host installs billet.db, the
// identity, the CA and the App key and reports SUCCESS, and the control plane
// then ignores that file entirely and connects to the database its config names.
// If that database is empty the restored controller advertises capacity against
// no leases at all and reaps the surviving compute as orphans — which is exactly
// the half-deployment this package exists to refuse, arriving through a config
// mismatch rather than a missing file.
//
// AN ABSENT `state:` BLOCK IS SQLITE, not "unknown". config.ServerConfig
// .LedgerBackend answers the same way, and treating the empty string as a
// wildcard here would exempt every config written before that key existed —
// which is most of them.
func (p *Plan) checkLedgerBackendAgrees() {
	target := p.Target.LedgerBackend
	if target == "" {
		target = "sqlite"
	}

	if p.Archive.Manifest.Ledger.IsExternal() {
		// THE SET IS ASKED, NOT JUST THE TWO STRINGS COMPARED. Equality alone
		// accepted target `X` for an archive naming `X` — two identical typos
		// agreeing with each other — and this package is exported, so "config
		// validation would have caught it" is true of one caller rather than of
		// the rule. decodeManifest has already refused an unnamed or unknown
		// backend, so what remains here is whether the TARGET is that one.
		if externalBackends[target] && target == p.Archive.Manifest.Ledger.Backend {
			return
		}

		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf(
				"this backup's ledger is external (%s) and %s says this host's ledger is %s. "+
					"Restoring the identity here would leave the control plane to create an "+
					"empty ledger of its own and start against it",
				p.archiveLedgerBackend(), p.Target.ConfigPath, p.targetLedgerBackend()),
			Remedy: "point this host's config at the ledger this identity belongs to — " +
				"`state: {backend: " + p.archiveLedgerBackend() + ", ...}` beside " +
				"`server.identity_dir` — and run this again",
		})

		return
	}

	if target == "sqlite" {
		return
	}

	p.refuse(lifeops.Refusal{
		What: fmt.Sprintf(
			"this backup carries its own ledger and %s says this host's ledger is %s. The "+
				"control plane would not read the restored file at all — it would connect to "+
				"the database that config names, and if that one is empty it advertises "+
				"capacity against no leases and reaps the compute this deployment is still "+
				"running", p.Target.ConfigPath, p.targetLedgerBackend()),
		Remedy: "restore this backup onto a host whose ledger is sqlite, or — if this " +
			"deployment has genuinely moved to " + p.targetLedgerBackend() + " — migrate the " +
			"ledger into that database yourself and restore the identity from an archive " +
			"taken after the move",
	})
}

// archiveLedgerBackend names the engine the archive says its ledger lives in.
func (p *Plan) archiveLedgerBackend() string {
	if b := p.Archive.Manifest.Ledger.Backend; b != "" {
		return b
	}

	return "an unnamed engine"
}

// targetLedgerBackend is what this host's config says, with the default spelled
// out — an absent `state:` block IS sqlite, and reporting it as "unset" would
// describe the refusal as being about a missing key rather than about a local
// ledger.
func (p *Plan) targetLedgerBackend() string {
	if p.Target.LedgerBackend == "" {
		return "sqlite (no state block, which is the default)"
	}

	return p.Target.LedgerBackend
}

// ledgerIsTheArchives reports whether the ledger already on disk is byte-for-
// byte the one this archive carries.
func (p *Plan) ledgerIsTheArchives(path string) (bool, error) {
	rec, found := p.Archive.Manifest.Record(EntryLedger)
	if !found {
		return false, nil
	}

	sum, size, err := digestFile(path)
	if err != nil {
		return false, err
	}

	return size == rec.Size && sum == rec.SHA256, nil
}

// committedTarget reports whether this state directory belongs to a deployment
// somebody has commissioned, and what says so.
//
// THE IDENTITY AND THE AUTHORITY MARKER, not the presence of files. Those two
// are what a control plane writes when an installation BEGINS, and they are
// exactly what a prepared-but-uncommissioned host does not have.
func (p *Plan) committedTarget() (bool, string) {
	if _, found, err := state.PeekDeploymentID(p.Target.StateDir); err != nil || found {
		return true, "it has a deployment identity"
	}

	marker := filepath.Join(p.Target.StateDir, "authority-created")
	if _, err := os.Lstat(marker); err == nil {
		return true, "it has " + marker
	}

	for _, f := range []string{"ca.key", "ca.crt"} {
		path := filepath.Join(p.Target.StateDir, "ca", f)
		if _, err := os.Lstat(path); err == nil {
			return true, "it has " + path
		}
	}

	return false, ""
}

// planAuthority decides what happens to each allowlisted authority file.
//
// OVER THE WHOLE ALLOWLIST, NOT OVER WHAT THE ARCHIVE HAPPENS TO CARRY, and
// that direction matters as much as the other. A target holding a leftover
// ca-previous.crt from an abandoned rotation, restored from an archive that is
// NOT rotating, would never have that file looked at: the restore would install
// the current authority, report success, and the next control-plane start would
// read the leftover as an active rotation and try to serve with an authority
// whose key is not there. An allowlisted file the archive does not have is
// preserved and REFUSED, exactly like one whose bytes differ.
func (p *Plan) planAuthority() {
	carried := map[string]bool{}
	for _, name := range p.Archive.AuthorityNames() {
		carried[name] = true
	}

	for _, f := range wirecert.AuthorityFiles {
		path := authorityTargetPath(p.Target.StateDir, f.Name)

		if !carried[f.Name] {
			p.refuseLeftoverAuthority(f.Name, path)

			continue
		}

		entry := AuthorityEntry(f.Name)
		want, _ := p.Archive.Entry(entry)

		disposition, refusal := comparePublish(path, want, "node-wire authority file")
		if refusal != nil {
			p.refuse(*refusal)

			continue
		}

		p.add(Action{Entry: entry, Path: path, What: "authority " + f.Name,
			Disposition: disposition})
	}
}

// refuseLeftoverAuthority refuses an allowlisted authority file the target has
// and the archive does not.
func (p *Plan) refuseLeftoverAuthority(name, path string) {
	switch _, err := os.Lstat(path); {
	case errors.Is(err, os.ErrNotExist):
		// Nothing there and nothing to install. The ordinary case for
		// ca-previous.* against an archive taken outside a rotation.
	case err != nil:
		p.refuse(lifeops.Refusal{
			What:   fmt.Sprintf("billet could not inspect %s: %v", path, err),
			Remedy: "resolve that first; billet will not restore an authority it cannot see all of",
		})
	default:
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s holds %s and this backup does not carry it", p.Target.StateDir,
				name),
			Remedy: "that file is left exactly as it is. A ca-previous pair the archive does not " +
				"have is an abandoned rotation on this host: a control plane reads it as an " +
				"active one and tries to serve a certificate whose key is not there. Finish or " +
				"undo that rotation (`billet ca retire --force`) before restoring",
		})
	}
}

func authorityTargetPath(stateDir, name string) string {
	if name == "authority-created" {
		return filepath.Join(stateDir, name)
	}

	return filepath.Join(stateDir, "ca", name)
}

// planAppKey decides what happens to the GitHub App private key.
//
// NEVER REPLACED. GitHub returns it exactly once, from the manifest conversion,
// and there is no re-issue — so an occupied destination holding DIFFERENT bytes
// is preserved and refused, whatever the archive says. The installer already
// uses no-clobber publication for exactly this reason.
// reservedTargetPaths are the files billet itself owns inside a state
// directory.
//
// THE WHOLE DIRECTORY, NOT A LIST OF FILES IN IT. The first version of this
// enumerated the ledger, the lock, the identity, the journal, the fence and the
// authority allowlist — and missed `ca/ca.key.new` and `ca/ca.crt.new`, which
// `billet ca rotate` REMOVES by name before it mints. An App key configured
// there would be issued, installed, and then deleted by an unrelated command,
// and GitHub does not reissue one.
//
// That miss is the argument against enumerating at all: the list has to be
// re-derived every time anything anywhere learns to write a new filename under
// this directory, and nothing makes that happen. The state directory is
// billet's; a credential billet does not manage does not belong inside it.
//
// The direction of failure decides it. Refusing too much costs an operator one
// `mv` and a sentence saying why; refusing too little costs the one credential
// that cannot be replaced.
func appKeyInsideStateDir(appKeyPath, stateDir string) (bool, error) {
	key, err := canonical(appKeyPath)
	if err != nil {
		return false, err
	}

	dir, err := canonical(stateDir)
	if err != nil {
		return false, err
	}

	// TWO ANSWERS, AND EITHER ONE REFUSES. Neither is sufficient alone.
	if lexicallyInside(key, dir) {
		return true, nil
	}

	return sharesAnAncestorWith(key, dir)
}

// lexicallyInside is the containment test on the cleaned absolute paths.
//
// filepath.Rel RATHER THAN A PREFIX COMPARISON. `strings.HasPrefix(key, dir +
// separator)` is wrong at the filesystem ROOT: with a state directory of "/"
// the prefix becomes "//", which nothing matches, so every path on the machine
// reads as OUTSIDE the state directory — including one a rotation deletes. Rel
// answers the question directly and has no such edge.
//
// THE ROOT IS THE ONLY INPUT THE TWO SPELLINGS DISAGREE ON, and the identity
// walk below independently covers it (every path walks up to "/", which always
// exists). So mutating either one alone leaves the root case passing, and
// mutating BOTH fails it — measured, rather than assumed. That redundancy is
// worth having and is not worth mistaking for two tests: what Rel carries on
// its own is the case where the state directory does not exist yet and the walk
// has nothing to compare against.
func lexicallyInside(key, dir string) bool {
	rel, err := filepath.Rel(dir, key)
	if err != nil {
		// Different volumes on Windows, or a pair Rel cannot relate. Not a
		// containment claim either way; the identity walk is the other half.
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sharesAnAncestorWith answers the same question by filesystem IDENTITY, for
// the cases a string comparison cannot reach.
//
// A CASE-INSENSITIVE FILESYSTEM IS THE ONE THAT MATTERS, and billet supports
// macOS, where APFS is case-insensitive by default. `/usr/local/VAR/billet/
// server/key.pem` and `/usr/local/var/billet/server` name the same directory to
// the kernel and different strings to Go — and EvalSymlinks preserves the
// caller's spelling of ordinary components, so canonicalising does not
// reconcile them. A hard-linked or bind-mounted directory is the same problem.
//
// The key's own path usually does not exist yet, so this walks UP from it and
// compares each ancestor that does exist. os.SameFile is the comparison because
// it asks the filesystem rather than the string.
func sharesAnAncestorWith(key, dir string) (bool, error) {
	want, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing to be inside of by identity — a restore creates the state
			// directory later, and the lexical answer above already covers the
			// paths it will create. Not an error: planning against a host that
			// has not been prepared yet is the ordinary case.
			return false, nil
		}

		return false, fmt.Errorf("deployarchive: inspect %s: %w", dir, err)
	}

	for at := key; ; {
		switch info, err := os.Stat(at); {
		case err == nil:
			if os.SameFile(want, info) {
				return true, nil
			}
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			// This component is not there, so neither is anything below it.
		default:
			return false, fmt.Errorf("deployarchive: inspect %s: %w", at, err)
		}

		parent := filepath.Dir(at)
		if parent == at {
			return false, nil
		}

		at = parent
	}
}

// canonical resolves a path far enough to compare it with another.
//
// filepath.Clean IS NOT ENOUGH: a relative spelling, or a symlinked ancestor,
// names the same file by a different string, and the comparison this feeds
// decides whether a credential is about to be written somewhere billet
// rewrites. EvalSymlinks needs the path to EXIST, and an App key destination
// usually does not yet — so this resolves the deepest ancestor that does and
// rejoins the rest.
func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("deployarchive: resolve %s: %w", path, err)
	}

	rest := ""
	head := filepath.Clean(abs)

	for {
		resolved, err := filepath.EvalSymlinks(head)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}

		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("deployarchive: resolve %s: %w", path, err)
		}

		parent := filepath.Dir(head)
		if parent == head {
			// Reached the root without finding anything that exists. Nothing to
			// resolve, so the cleaned absolute path is the best answer there is.
			return filepath.Clean(abs), nil
		}

		rest = filepath.Join(filepath.Base(head), rest)
		head = parent
	}
}

func (p *Plan) planAppKey() {
	if p.Target.AppKeyPath == "" {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s names no github.private_key_path, so billet has nowhere to put "+
				"this deployment's App key", p.Target.ConfigPath),
			Remedy: "set github.private_key_path in that config and run this again",
		})

		return
	}

	inside, err := appKeyInsideStateDir(p.Target.AppKeyPath, p.Target.StateDir)
	if err != nil {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("billet could not work out whether %s is inside the state "+
				"directory: %v", p.Target.AppKeyPath, err),
			Remedy: "resolve that path; billet will not install the App key somewhere it cannot " +
				"place",
		})

		return
	}

	if inside {
		p.refuse(lifeops.Refusal{
			What: fmt.Sprintf("%s names %s as github.private_key_path, and that is inside the "+
				"state directory %s", p.Target.ConfigPath, p.Target.AppKeyPath, p.Target.StateDir),
			Remedy: "point github.private_key_path outside the state directory. That directory is " +
				"billet's — it creates, renames and DELETES files there by name, including " +
				"staging names a rotation clears — and GitHub issues the App key exactly once",
		})

		return
	}

	want, _ := p.Archive.Entry(EntryAppKey)

	disposition, refusal := comparePublish(p.Target.AppKeyPath, want, "GitHub App private key")
	if refusal != nil {
		p.refuse(*refusal)

		return
	}

	p.add(Action{Entry: EntryAppKey, Path: p.Target.AppKeyPath, What: "GitHub App private key",
		Disposition: disposition})
}

// comparePublish is the no-clobber decision every credential in this package
// makes: absent installs, identical is already done, different is preserved and
// refused, and unreadable is refused rather than assumed absent.
//
// ONE IMPLEMENTATION, because getting it right once and wrong once is how a CA
// key gets replaced by a restore that was careful about the App key.
func comparePublish(path string, want []byte, what string) (Disposition, *lifeops.Refusal) {
	info, err := os.Lstat(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return Install, nil
	case err != nil:
		return Install, &lifeops.Refusal{
			What: fmt.Sprintf("billet could not inspect %s (%s): %v", path, what, err),
			Remedy: "resolve that first — \"billet could not tell\" is never permission to write " +
				"a credential over whatever is there",
		}
	case info.Mode()&os.ModeSymlink != 0:
		return Install, &lifeops.Refusal{
			What:   fmt.Sprintf("%s (%s) is a symlink", path, what),
			Remedy: "billet installs a credential only at the path it was given; move the link aside",
		}
	case !info.Mode().IsRegular():
		return Install, &lifeops.Refusal{
			What:   fmt.Sprintf("%s (%s) is not a regular file", path, what),
			Remedy: "move it aside",
		}
	}

	have, err := readSmall(path)
	if err != nil {
		return Install, &lifeops.Refusal{
			What: fmt.Sprintf("%s (%s) exists and billet could not read it, so it cannot tell "+
				"whether it already holds what this backup carries: %v", path, what, err),
			Remedy: "look at that file yourself; billet will neither overwrite it nor assume it " +
				"is the same",
		}
	}

	if bytes.Equal(have, want) {
		return AlreadyPresent, nil
	}

	return Install, &lifeops.Refusal{
		What: fmt.Sprintf("%s already holds a DIFFERENT %s", path, what),
		Remedy: "billet will not overwrite it, and it is left exactly as it is. Identify which " +
			"deployment that file belongs to and move it somewhere safe before restoring — " +
			"GitHub does not reissue an App key, and a replaced authority drops every node in " +
			"a fleet at once",
	}
}

func (p *Plan) add(a Action) { p.Actions = append(p.Actions, a) }

// dispositionOf reports what this plan decided about one entry, and answers
// Install for an entry nothing has decided about yet.
//
// THE ZERO VALUE IS THE SAFE ONE HERE: a caller asking whether something is
// AlreadyPresent gets "no" for an entry that was refused or never planned,
// rather than an answer that reads as proof.
func (p *Plan) dispositionOf(entry string) Disposition {
	for _, a := range p.Actions {
		if a.Entry == entry {
			return a.Disposition
		}
	}

	return Install
}

// LedgerPath is where a deployment's ledger lives.
//
// EXPORTED SO THE NAME HAS ONE HOME, for the reason state.DirectoryLockPath and
// wirecert.AuthorityLockPath are: three places compute it now — the planner, the
// executor's supersede, and an abandon putting one back — and a second literal
// among them is a ledger somebody moves aside and nobody restores.
func LedgerPath(stateDir string) string { return filepath.Join(stateDir, "billet.db") }

// supersededSuffix names where a replaced ledger goes.
//
// THE ARCHIVE'S OWN NAME rather than the clock, so a recovery that is
// interrupted and re-run computes the SAME one: the second pass must recognise
// the rename it already did rather than move a fresh ledger aside under a new
// name and lose the first one. That name carries the manifest digest as well as
// the timestamp, so two archives of one deployment taken in the same second do
// not land on each other.
func supersededSuffix(a *Archive) string { return ".superseded-" + a.Name() }

func (p *Plan) refuse(r lifeops.Refusal) { p.Refusals = append(p.Refusals, r) }
