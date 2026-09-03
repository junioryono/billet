package deployarchive

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state"
)

// externalBackupTo captures d as an IDENTITY-ONLY archive, the way the command
// does for a deployment whose ledger is in PostgreSQL.
//
// THE REAL Write, WITH THE REAL DEPLOYMENT BESIDE IT. The point of these tests
// is that an archive with no ledger is a whole thing rather than a truncated
// one, and a fixture assembled by hand could not tell the two apart — it would
// be exactly the truncated archive the refusals exist to catch.
func externalBackupTo(t *testing.T, d deployment, dest string) Manifest {
	t.Helper()

	m, err := Write(t.Context(), BackupRequest{
		Dest:         dest,
		StateDir:     d.stateDir,
		ConfigPath:   d.configPath,
		DeploymentID: d.id,
		GitHub:       d.github,
		AppKeyPEM:    d.appKey,
		ConfigBody:   []byte("server: {}\n"),
		ExternalLedger: &ExternalLedger{
			Backend: "postgres",
			DSNEnv:  "BILLET_STATE_DSN",
		},
		Now:      nowStub,
		Hostname: "test-host",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	return m
}

// AN IDENTITY-ONLY ARCHIVE IS A WHOLE ARCHIVE.
//
// `billet local backup` FAILED OUTRIGHT on a PostgreSQL deployment, because
// deployarchive called Snapshot unconditionally — so the half billet does own went
// uncaptured too. For a control plane built by control-plane-postgres that is the
// only recovery path there is: the module has no ledger volume by design, its root
// volume is delete_on_termination, and the App key is issued exactly once.
func TestABackupOfAnExternalLedgerCarriesEverythingElse(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	m := externalBackupTo(t, d, dest)

	if !m.Ledger.IsExternal() {
		t.Fatal("the manifest does not record the ledger as external")
	}

	if m.Schema != 2 {
		t.Errorf("an external archive is schema 2, got %d", m.Schema)
	}

	if m.Ledger.Backend != "postgres" || m.Ledger.DSNEnv != "BILLET_STATE_DSN" {
		t.Errorf("the manifest does not name the ledger: %+v", m.Ledger)
	}

	// EVERY OTHER PIECE IS THERE. This is the half of the failure that matters: the
	// command used to produce NOTHING, so an operator lost the identity, the
	// authority and the App key along with a ledger billet was never going to copy.
	for _, want := range []string{
		EntryIdentity, EntryAppKey,
		AuthorityEntry("ca.key"), AuthorityEntry("ca.crt"), AuthorityEntry("authority-created"),
	} {
		if _, ok := m.Record(want); !ok {
			t.Errorf("the manifest does not declare %s", want)
		}

		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("%s is not in the archive: %v", want, err)
		}
	}

	if _, ok := m.Record(EntryLedger); ok {
		t.Error("an external archive declares a ledger entry")
	}

	// NOT EVEN AN EMPTY ledger/ DIRECTORY, because refuseUndeclared walks the
	// tree and Open would then refuse the archive this package just wrote.
	if _, err := os.Stat(filepath.Join(dest, "ledger")); !os.IsNotExist(err) {
		t.Errorf("an external archive left a ledger directory behind: %v", err)
	}

	// AND IT READS BACK. Open is the whole integrity pass — every digest, the
	// closed entry set, the cross-checks — so this is what says the archive is
	// one rather than merely looking like one.
	if _, err := Open(t.Context(), dest); err != nil {
		t.Fatalf("Open refused the archive it just wrote: %v", err)
	}
}

// A BACKUP CARRIES ITS LEDGER OR DECLARES IT EXTERNAL, NEVER BOTH.
//
// The one combination that could LIE: it would write a snapshot into the archive
// and a manifest saying the file beside it is not there.
func TestABackupCannotBothSnapshotAndDeclareExternal(t *testing.T) {
	d := newDeployment(t)

	db, err := state.OpenAdmin(t.Context(), d.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	_, err = Write(t.Context(), BackupRequest{
		Dest:           filepath.Join(t.TempDir(), "archive"),
		StateDir:       d.stateDir,
		ConfigPath:     d.configPath,
		DeploymentID:   d.id,
		GitHub:         d.github,
		AppKeyPEM:      d.appKey,
		Snapshot:       db.SnapshotInto,
		ExternalLedger: &ExternalLedger{Backend: "postgres"},
		Now:            nowStub,
	})
	if err == nil {
		t.Fatal("a request naming both a snapshot and an external ledger was accepted")
	}

	if !strings.Contains(err.Error(), "never both") {
		t.Errorf("the refusal does not name the contradiction: %v", err)
	}
}

// AN EXTERNAL LEDGER HAS TO NAME THE ENGINE IT LIVES IN.
//
// "not here" with nothing attached tells a restore nothing to pair the identity
// with, and the refusal it would produce could not name what to fix.
func TestAnExternalLedgerMustNameItsEngine(t *testing.T) {
	d := newDeployment(t)

	_, err := Write(t.Context(), BackupRequest{
		Dest:           filepath.Join(t.TempDir(), "archive"),
		StateDir:       d.stateDir,
		ConfigPath:     d.configPath,
		DeploymentID:   d.id,
		GitHub:         d.github,
		AppKeyPEM:      d.appKey,
		ExternalLedger: &ExternalLedger{DSNEnv: "BILLET_STATE_DSN"},
		Now:            nowStub,
	})
	if err == nil {
		t.Fatal("an external ledger with no engine was accepted")
	}

	if !strings.Contains(err.Error(), "engine") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

// A TRUNCATED ARCHIVE AND A DELIBERATE ONE ARE NOT THE SAME THING.
//
// This is the reason External is a CLAIM rather than being derived from the
// absence of the entry. Deriving it would make an archive whose ledger was lost
// in transit restore an identity paired with nothing — the half-deployment this
// package exists to refuse — and it would arrive reported as a success.
func TestALedgerMissingWithoutTheClaimIsStillRefused(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	backupTo(t, d, dest)

	if err := os.Remove(filepath.Join(dest, EntryLedger)); err != nil {
		t.Fatalf("remove the ledger: %v", err)
	}

	rewriteManifest(t, dest, func(m *Manifest) {
		kept := m.Files[:0]

		for _, f := range m.Files {
			if f.Path != EntryLedger {
				kept = append(kept, f)
			}
		}

		m.Files = kept
	})

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("an archive whose ledger was removed opened successfully")
	}

	// THE REFUSAL HAS TO BE THE UNIT ONE, and asserting only err != nil was not
	// enough — MEASURED by mutation. With the ledger removed from requiredEntries
	// this still failed, but from checkManifestDescribesItsLedger trying to peek a
	// file that is not there: an lstat error naming a path inside the archive,
	// which tells an operator nothing about what is wrong with their backup. The
	// weak assertion passed and the diagnostic was gone.
	if !errors.Is(err, errNotWholeDeployment) {
		t.Errorf("the refusal is not the whole-deployment one, so the operator is told a "+
			"storage error instead of what is missing: %v", err)
	}
}

// AND THE REQUIRED SET IS ASSERTED DIRECTLY, BECAUSE ITERATING IT PROVES NOTHING.
//
// TestAnArchiveMissingPartOfTheUnitIsRefused loops over requiredEntries and
// removes each in turn, which is self-referential: an entry dropped from the
// list is an entry the loop stops testing, so the suite stays green exactly when
// the rule is weakened. Measured — removing the ledger from that list turns
// neither that test nor the one above red on its own.
//
// This is the assertion those two cannot make about themselves.
func TestTheRequiredEntrySetIsWhatItClaims(t *testing.T) {
	// COMPARED AGAINST LITERALS, NOT PROBED WITH Contains. A containment check
	// plus a length check is satisfied by a SUBSTITUTION — swap
	// authority/authority-created for the optional config entry and every
	// assertion still holds — which is the same shape of hole as iterating the
	// list under test. Sorted equality is the assertion that has no such gap.
	wantOrdinary := []string{
		AuthorityEntry("authority-created"), AuthorityEntry("ca.crt"), AuthorityEntry("ca.key"),
		EntryAppKey, EntryIdentity, EntryLedger,
	}

	// THE LEDGER IS THE ONLY DIFFERENCE. An external archive is identity-only,
	// which is not permission to omit the authority or the App key — those are
	// precisely what it exists to carry.
	wantExternal := []string{
		AuthorityEntry("authority-created"), AuthorityEntry("ca.crt"), AuthorityEntry("ca.key"),
		EntryAppKey, EntryIdentity,
	}

	ordinary := slices.Sorted(slices.Values(requiredEntries(Manifest{})))
	external := slices.Sorted(slices.Values(
		requiredEntries(Manifest{Ledger: LedgerFacts{External: true}})))

	slices.Sort(wantOrdinary)
	slices.Sort(wantExternal)

	if !slices.Equal(ordinary, wantOrdinary) {
		t.Errorf("an ordinary archive requires %v, and the unit is %v", ordinary, wantOrdinary)
	}

	if !slices.Equal(external, wantExternal) {
		t.Errorf("an external archive requires %v, and it must be %v", external, wantExternal)
	}

	// THE PREVIOUS AUTHORITY IS DELIBERATELY ABSENT FROM BOTH: it exists only
	// while a rotation is running, and requiring it would refuse every ordinary
	// backup. Stated separately because the literals above would hide the reason.
	for _, set := range [][]string{ordinary, external} {
		if slices.Contains(set, AuthorityEntry("ca-previous.key")) {
			t.Error("the previous authority is required, which refuses every backup taken " +
				"outside a rotation")
		}
	}
}

// AND A SCHEMA THAT CANNOT EXPRESS THE CLAIM MUST NOT BE READ AS MAKING IT.
//
// External is a schema-2 field. A schema-1 manifest carrying it was written by
// nothing billet ships, and honouring it would install an identity with no
// ledger while the archive declares the version whose entry set REQUIRES one.
func TestASchemaOneArchiveCannotClaimAnExternalLedger(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	backupTo(t, d, dest)

	if err := os.Remove(filepath.Join(dest, EntryLedger)); err != nil {
		t.Fatalf("remove the ledger: %v", err)
	}

	rewriteManifest(t, dest, func(m *Manifest) {
		m.Schema = 1
		m.Ledger.External = true
		m.Ledger.Backend = "postgres"

		kept := m.Files[:0]

		for _, f := range m.Files {
			if f.Path != EntryLedger {
				kept = append(kept, f)
			}
		}

		m.Files = kept
	})

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("a schema-1 manifest claiming an external ledger opened successfully")
	}

	if !strings.Contains(err.Error(), "no way to express") {
		t.Errorf("the refusal does not name the contradiction: %v", err)
	}
}

// AN EXTERNAL ARCHIVE MAY NOT DECLARE A LEDGER ENTRY EITHER.
//
// The same rule facing the other way: the manifest says the ledger is elsewhere,
// so an entry claiming to BE the ledger contradicts it. Accepting one would
// install a ledger the manifest says does not exist, onto a host whose config
// points the control plane at a database instead.
func TestAnExternalArchiveCannotAlsoDeclareALedger(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	backupTo(t, d, dest)

	rewriteManifest(t, dest, func(m *Manifest) {
		m.Schema = 2
		m.Ledger.External = true
		m.Ledger.Backend = "postgres"
	})

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("an external manifest declaring a ledger opened successfully")
	}

	if !strings.Contains(err.Error(), EntryLedger) {
		t.Errorf("the refusal does not name the entry: %v", err)
	}
}

// externalArchive is an identity-only archive of a fresh deployment, opened.
func externalArchive(t *testing.T) *Archive {
	t.Helper()

	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	externalBackupTo(t, d, dest)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return a
}

// externalTarget is a bare host configured for the ledger the archive names.
func externalTarget(t *testing.T, attached bool) Target {
	t.Helper()

	root := t.TempDir()

	return Target{
		ConfigPath:             filepath.Join(root, "billet.yaml"),
		StateDir:               filepath.Join(root, "state"),
		AppKeyPath:             filepath.Join(root, "app-private-key.pem"),
		GitHub:                 GitHubIdentity{Org: "acme", AppID: 42, InstallationID: 4242},
		LedgerBackend:          "postgres",
		ExternalLedgerAttached: attached,
	}
}

// refusalsMentioning collects the refusals whose text contains want.
func refusalsMentioning(p Plan, want string) []string {
	var out []string

	for _, r := range p.Refusals {
		if strings.Contains(r.What, want) || strings.Contains(r.Remedy, want) {
			out = append(out, r.What)
		}
	}

	return out
}

// AN IDENTITY-ONLY ARCHIVE IS REFUSED UNTIL THE OPERATOR SAYS THE LEDGER IS BACK.
//
// This is the invariant: pairing the two halves is the whole point, and an
// archive that silently restores half of it is worse than one that refuses.
// Billet cannot check this half — the database is on the other end of a DSN this
// process has not been given — so it asks.
//
// A ledger that is NOT back produces a control plane that starts against an
// empty database holding a restored identity: it advertises capacity for a fleet
// it has no record of, and reaps as orphans the compute the old one launched.
func TestAnExternalArchiveIsRefusedUntilTheLedgerIsAsserted(t *testing.T) {
	a := externalArchive(t)

	p, err := PlanRestore(t.Context(), a, externalTarget(t, false))
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(refusalsMentioning(p, "--external-ledger-attached")) == 0 {
		t.Fatalf("no refusal asks for the assertion: %+v", p.Refusals)
	}

	// AND IT IS THE ONLY THING IN THE WAY. A test that merely counted refusals
	// would pass while the identity or the authority was ALSO being refused for
	// an unrelated reason, and would then keep passing if this one were deleted.
	p, err = PlanRestore(t.Context(), a, externalTarget(t, true))
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(p.Refusals) > 0 {
		t.Fatalf("an asserted external restore is still refused: %+v", p.Refusals)
	}
}

// AND NO LEDGER IS INSTALLED, WHICH IS WHAT MAKES IT AN IDENTITY-ONLY RESTORE.
//
// Every later stage keys off the action list — the publication order, the
// journal's digests, the abandon's put-back — so an action with nothing behind
// it would have to be special-cased in all three.
func TestAnExternalRestorePlansNoLedgerAction(t *testing.T) {
	a := externalArchive(t)

	p, err := PlanRestore(t.Context(), a, externalTarget(t, true))
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	// THE WHOLE SET, NOT "SOME ACTIONS AND NO LEDGER ONE". The weaker pair passes
	// against a plan that installs the identity alone — which is the
	// half-deployment this package exists to refuse, and the identity is the ONE
	// piece whose absence a later start would not immediately expose.
	var got []string
	for _, act := range p.Actions {
		got = append(got, act.Entry)
	}

	want := []string{
		AuthorityEntry("authority-created"), AuthorityEntry("ca.crt"), AuthorityEntry("ca.key"),
		EntryAppKey, EntryIdentity,
	}

	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("an external restore plans %v, and it must plan exactly %v — no ledger, and "+
			"everything else", got, want)
	}
}

// A HOST CONFIGURED FOR A LOCAL LEDGER IS REFUSED, AND THAT HALF BILLET CAN PROVE.
//
// Restoring the identity beside a config naming a local ledger leaves the
// control plane to create an EMPTY billet.db of its own and start against it:
// every node's certificate valid, every lease gone. It is the same lost fleet as
// restoring an identity with no ledger at all, arriving through a config mistake
// instead of a missing file.
func TestAnExternalArchiveIsRefusedOntoALocalLedgerHost(t *testing.T) {
	a := externalArchive(t)

	for _, backend := range []string{"", "sqlite"} {
		t.Run("backend="+backend, func(t *testing.T) {
			target := externalTarget(t, true)
			target.LedgerBackend = backend

			p, err := PlanRestore(t.Context(), a, target)
			if err != nil {
				t.Fatalf("PlanRestore: %v", err)
			}

			if len(refusalsMentioning(p, "empty ledger of its own")) == 0 {
				t.Fatalf("no refusal names the mismatch: %+v", p.Refusals)
			}
		})
	}
}

// A RECOVER IS REFUSED OUTRIGHT, BECAUSE IT IS AN OPERATION ON THE LEDGER.
//
// `billet local recover` seals the deployment, waits for it to go quiet, renames
// the live billet.db aside and installs the archive's in its place. None of that
// exists for a database billet does not hold — and the seal itself goes through
// a SQLite handle, so the operation could not run even if the ledger half were
// skipped.
func TestARecoverIsRefusedForAnExternalLedger(t *testing.T) {
	a := externalArchive(t)

	p, err := PlanRecover(t.Context(), a, externalTarget(t, true))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "operation on the ledger")) == 0 {
		t.Fatalf("recover does not refuse an external archive: %+v", p.Refusals)
	}

	// THE REMEDY NAMES THE COMMAND THAT DOES WORK. A refusal that only says no
	// leaves an operator with an archive and no way to use it, on the day they
	// most need one.
	if len(refusalsMentioning(p, "billet local restore")) == 0 {
		t.Errorf("the refusal does not name what to run instead: %+v", p.Refusals)
	}
}

// THE BACKEND MATRIX, BOTH DIRECTIONS.
//
// The first version of this rule lived inside planExternalLedger and therefore
// covered ONE direction. The reverse is the same failure: restoring a SQLite
// archive onto a PostgreSQL-configured host installs billet.db and reports
// success, and the control plane then ignores that file and connects to the
// database its config names — advertising capacity against no leases and reaping
// the surviving compute as orphans.
//
// A TABLE RATHER THAN TWO TESTS, because what is being asserted is that the rule
// is total: every combination has an answer, and the ones that must be refused
// are refused for the stated reason rather than by accident.
func TestTheLedgerBackendMustAgreeInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		external bool
		target   string
		refused  bool
		// names is a phrase the refusal must carry, so a test cannot pass on a
		// refusal that happens to exist for an unrelated reason.
		names string
	}{
		{name: "sqlite archive onto an unset target", target: "", refused: false},
		{name: "sqlite archive onto a sqlite target", target: "sqlite", refused: false},
		{
			name: "sqlite archive onto a postgres target", target: "postgres", refused: true,
			names: "would not read the restored file at all",
		},
		{
			name: "external archive onto an unset target", external: true, target: "",
			refused: true, names: "empty ledger of its own",
		},
		{
			name: "external archive onto a sqlite target", external: true, target: "sqlite",
			refused: true, names: "empty ledger of its own",
		},
		{
			name: "external archive onto a postgres target", external: true, target: "postgres",
			refused: false,
		},
		{
			// AN ENGINE NEITHER SIDE RECOGNISES IS STILL A DISAGREEMENT. Answering
			// "not sqlite, so fine" would let a config typo through.
			name: "external archive onto some other engine", external: true, target: "mysql",
			refused: true, names: "empty ledger of its own",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeployment(t)
			dest := filepath.Join(t.TempDir(), "archive")

			if tc.external {
				externalBackupTo(t, d, dest)
			} else {
				backupTo(t, d, dest)
			}

			a, err := Open(t.Context(), dest)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			target := externalTarget(t, true)
			target.LedgerBackend = tc.target

			p, err := PlanRestore(t.Context(), a, target)
			if err != nil {
				t.Fatalf("PlanRestore: %v", err)
			}

			got := refusalsMentioning(p, "ledger is")
			if tc.names != "" {
				got = refusalsMentioning(p, tc.names)
			}

			switch {
			case tc.refused && len(got) == 0:
				t.Errorf("this pairing was accepted; refusals were %+v", p.Refusals)
			case !tc.refused && len(p.Refusals) > 0:
				t.Errorf("this pairing was refused: %+v", p.Refusals)
			}
		})
	}
}

// A MALFORMED EXTERNAL MANIFEST IS REFUSED WHERE THE ARCHIVE IS READ.
//
// Both halves of the closed set, and both used to reach the PLANNER instead. An
// archive naming no engine produced a refusal saying the control plane would
// "create an empty ledger of its own" — false on a PostgreSQL target, which
// would connect to the database its config names — and advised configuring
// `state: {backend: an unnamed engine}`, which is not a thing to type. The
// archive is malformed, which is a fact about the archive.
func TestAMalformedLedgerClaimIsRefusedByOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*Manifest)
		names string
	}{
		{
			name:  "external naming no engine",
			edit:  func(m *Manifest) { m.Ledger.External = true },
			names: "no engine at all",
		},
		{
			name: "external naming an engine billet cannot pair with",
			edit: func(m *Manifest) {
				m.Ledger.External = true
				m.Ledger.Backend = "mysql"
			},
			names: "mysql",
		},
		{
			// THE OTHER DIRECTION. The two fields exist to say where the ledger
			// is; setting them while shipping the file describes two deployments.
			name:  "carrying a ledger and describing another",
			edit:  func(m *Manifest) { m.Ledger.Backend = "postgres" },
			names: "also describes an external one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeployment(t)
			dest := filepath.Join(t.TempDir(), "archive")

			backupTo(t, d, dest)

			rewriteManifest(t, dest, func(m *Manifest) {
				m.Schema = 2
				tc.edit(m)
			})

			_, err := Open(t.Context(), dest)
			if err == nil {
				t.Fatal("a malformed ledger claim opened successfully")
			}

			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal does not name the problem: %v", err)
			}
		})
	}
}

// AND THE PLANNER'S RULE IS TOTAL ON ITS OWN.
//
// decodeManifest refuses an unknown engine, so the pairings below cannot arrive
// through Open — which is exactly why this drives the planner directly. The
// first version compared the two strings for equality, so an archive naming `X`
// was accepted by a target naming `X`: two identical typos agreeing with each
// other. This package is exported and internal/e2e reaches it, so "the CLI would
// have caught it" is a property of one caller rather than of the rule.
func TestThePlannerRefusesAnEngineItCannotPairWith(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	externalBackupTo(t, d, dest)

	whole, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, tc := range []struct{ archive, target string }{
		{archive: "mysql", target: "mysql"},
		{archive: "", target: ""},
		{archive: "sqlite", target: "sqlite"},
		{archive: "postgres", target: "mysql"},
	} {
		t.Run(tc.archive+"/"+tc.target, func(t *testing.T) {
			// THE REAL ARCHIVE WITH ITS CLAIM REWRITTEN, so everything else about
			// it — the identity, the authority, the App key — is genuine and the
			// only thing under test is the pairing.
			forged := *whole
			forged.Manifest.Ledger = LedgerFacts{External: true, Backend: tc.archive}

			target := externalTarget(t, true)
			target.LedgerBackend = tc.target

			p, err := PlanRestore(t.Context(), &forged, target)
			if err != nil {
				t.Fatalf("PlanRestore: %v", err)
			}

			if len(refusalsMentioning(p, "empty ledger of its own")) == 0 {
				t.Errorf("archive %q paired with target %q was accepted; refusals were %+v",
					tc.archive, tc.target, p.Refusals)
			}
		})
	}
}
