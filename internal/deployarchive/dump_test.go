package deployarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
)

// fakeLedger stands in for the database a dump is restored into. It serves a
// scripted state, records every call in order, and models a restore as the
// database becoming whatever the restored file describes.
//
// IT DOES NOT JUDGE. Which dump was restored is decided by the bytes the
// restore was handed (their digest), and what the database holds afterwards is
// what the test scripted for those bytes, so a planner or executor that handed
// over the wrong file reads the wrong ledger back.
type fakeLedger struct {
	mu sync.Mutex

	now       state.PostgresLedger
	admission state.Admission
	calls     []string
	restored  []string
	paths     []string
	// versions and intos are the pg_dump major and the target each restore was
	// handed, in order.
	versions []int
	intos    []state.RestoreInto

	// byDigest is what the database holds after a restore of a file with that
	// digest.
	byDigest map[string]state.PostgresLedger
	// dumpBody is what Dump writes; dumpErr, restoreErr, peekErr and barrierErr
	// fail those calls.
	dumpBody   []byte
	dumpErr    error
	restoreErr error
	peekErr    error
	barrierErr error
}

// takenDumpVersion is the pg_dump major the fake's own dumps are taken with,
// distinct from the archive's so a restore handed the wrong one is seen.
const takenDumpVersion = 17

func (f *fakeLedger) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeLedger) Peek(context.Context) (state.PostgresLedger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("peek")

	if f.peekErr != nil {
		return state.PostgresLedger{}, f.peekErr
	}

	return f.now, nil
}

func (f *fakeLedger) Barrier(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("barrier")

	return f.barrierErr
}

func (f *fakeLedger) Dump(_ context.Context, dest string) (LedgerDump, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("dump")

	if f.dumpErr != nil {
		return LedgerDump{}, f.dumpErr
	}

	if err := os.WriteFile(dest, f.dumpBody, 0o644); err != nil {
		return LedgerDump{}, err
	}

	// WHAT THE DATABASE HOLDS NOW is what restoring this copy puts back.
	f.byDigest[digest(f.dumpBody)] = f.now

	return LedgerDump{
		Facts:      DumpFacts{Schema: f.now.Schema, DumpVersion: takenDumpVersion, Binding: f.now.Binding},
		Migrations: slices.Clone(f.now.Migrations),
		Database:   f.now.Database,
	}, nil
}

func (f *fakeLedger) Restore(_ context.Context, dump string, dumpVersion int, into state.RestoreInto) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.record("restore")
	f.versions = append(f.versions, dumpVersion)
	f.intos = append(f.intos, into)

	if f.restoreErr != nil {
		return f.restoreErr
	}

	body, err := os.ReadFile(dump)
	if err != nil {
		return err
	}

	sum := digest(body)
	f.restored = append(f.restored, sum)
	f.paths = append(f.paths, dump)

	after, ok := f.byDigest[sum]
	if !ok {
		return errors.New("fake: a dump nobody scripted was restored")
	}

	f.now = after

	return nil
}

func (f *fakeLedger) Admission(context.Context) (state.Admission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.admission, nil
}

func (f *fakeLedger) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

// stopBefore makes the next Execute stop just before it publishes entry, and
// puts the hook back when the test ends.
func stopBefore(t *testing.T, entry string, cause error) {
	t.Helper()

	prev := onPublish
	onPublish = func(a Action) error {
		if a.Entry == entry {
			return cause
		}

		return nil
	}

	t.Cleanup(func() { onPublish = prev })
}

// dumpSchema is the schema every dump in these tests was taken from.
const dumpSchema = "billet"

// pgDeployment is a deployment whose ledger is in PostgreSQL: the identity and
// the authority on disk, no billet.db beside them, and the migrations a real
// ledger of this binary has applied.
func pgDeployment(t *testing.T) (deployment, []state.AppliedMigration) {
	t.Helper()

	d := newDeployment(t)
	ledger := LedgerPath(d.stateDir)
	migrations := migrationsOf(t, t.Context(), ledger)

	for _, path := range append([]string{ledger}, LedgerSidecarPaths(ledger)...) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remove %s: %v", path, err)
		}
	}

	return d, migrations
}

// archiveDumpBody is the archive's dump in these tests: content only a restore
// of the archive's dump hands over.
var archiveDumpBody = []byte("PGDMP the archive's ledger")

// dumpBackupTo writes an archive carrying a dump, through the real Write, bound
// to binding.
func dumpBackupTo(t *testing.T, d deployment, dest string, migrations []state.AppliedMigration,
	binding string,
) Manifest {
	t.Helper()

	m, err := Write(t.Context(), BackupRequest{
		Dest:         dest,
		StateDir:     d.stateDir,
		ConfigPath:   d.configPath,
		DeploymentID: d.id,
		GitHub:       d.github,
		AppKeyPEM:    d.appKey,
		ConfigBody:   []byte("server: {}\n"),
		Dump: func(_ context.Context, path string) (LedgerDump, error) {
			if err := os.WriteFile(path, archiveDumpBody, 0o644); err != nil {
				return LedgerDump{}, err
			}

			return LedgerDump{
				Facts: DumpFacts{
					Schema: dumpSchema, ServerVersion: 160015, DumpVersion: 16, Binding: binding,
				},
				Migrations: migrations,
				DSNEnv:     "BILLET_STATE_DSN",
			}, nil
		},
		Now:      nowStub,
		Hostname: "test-host",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	return m
}

// dumpFixture is an archive carrying a dump, and the deployment it was taken of.
type dumpFixture struct {
	d          deployment
	archive    *Archive
	migrations []state.AppliedMigration
}

func newDumpFixture(t *testing.T, binding func(id string) string) dumpFixture {
	t.Helper()

	d, migrations := pgDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	dumpBackupTo(t, d, dest, migrations, binding(d.id))

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return dumpFixture{d: d, archive: a, migrations: migrations}
}

func bound(id string) string { return id }
func unbound(string) string  { return "" }

// restoredAs is what the database holds once the archive's dump is in it.
func (f dumpFixture) restoredAs() state.PostgresLedger {
	return state.PostgresLedger{
		Database: dumpDatabase, Schema: dumpSchema, ServerVersion: 160015,
		Tables:    []string{"admission", "schema_migrations"},
		Populated: true, Migrations: f.migrations, Binding: f.archive.Manifest.Ledger.Dump.Binding,
	}
}

// dumpDatabase is the database every ledger in these tests is in.
const dumpDatabase = "billet"

// ledgerFor is a fake database holding now, whose restore of the archive's dump
// produces the archive's ledger.
func (f dumpFixture) ledgerFor(now state.PostgresLedger) *fakeLedger {
	return &fakeLedger{
		now:      now,
		byDigest: map[string]state.PostgresLedger{digest(archiveDumpBody): f.restoredAs()},
		dumpBody: []byte("PGDMP the ledger that was there"),
	}
}

// pgTarget is a fresh host configured for PostgreSQL, reaching ledger.
func pgTarget(t *testing.T, d deployment, ledger PostgresLedger) Target {
	t.Helper()

	tgt := newTarget(t, d.github)
	tgt.LedgerBackend = "postgres"
	tgt.Postgres = ledger

	return tgt
}

// emptySchema is a database whose schema holds nothing.
func emptySchema() state.PostgresLedger {
	return state.PostgresLedger{Database: dumpDatabase, Schema: dumpSchema, ServerVersion: 160015}
}

// AN ARCHIVE CARRYING A DUMP IS SCHEMA 4 AND SAYS WHAT IT CARRIES: the dump
// entry, digested like every other, the engine, the schema, the pg_dump version
// and the migrations read in the dump's own snapshot. Every other archive stays
// at 3, which the billets that read 3 can restore.
func TestAnArchiveCarryingADumpIsSchemaFourAndDescribesIt(t *testing.T) {
	f := newDumpFixture(t, bound)
	m := f.archive.Manifest

	if m.Schema != 4 || !m.Ledger.IsDump() || m.Ledger.IsExternal() || m.Ledger.Backend != "postgres" {
		t.Fatalf("the manifest is schema %d with ledger %+v", m.Schema, m.Ledger)
	}

	rec, ok := m.Record(EntryLedgerDump)
	if !ok || rec.SHA256 != digest(archiveDumpBody) || rec.Size != int64(len(archiveDumpBody)) {
		t.Errorf("the dump's record is %+v (found %v)", rec, ok)
	}

	if _, ok := m.Record(EntryLedger); ok {
		t.Error("an archive carrying a dump also declares a SQLite ledger")
	}

	if d := m.Ledger.Dump; d.Schema != dumpSchema || d.DumpVersion != 16 || d.Format != DumpFormat ||
		d.Binding != f.d.id || m.Ledger.DSNEnv != "BILLET_STATE_DSN" {
		t.Errorf("the dump is described as %+v (dsn_env %q)", d, m.Ledger.DSNEnv)
	}

	if !slices.Equal(m.Ledger.Migrations, f.migrations) {
		t.Errorf("the manifest records %d migrations, want the dump's %d",
			len(m.Ledger.Migrations), len(f.migrations))
	}

	info, err := os.Stat(f.archive.DumpPath())
	if err != nil || info.Mode().Perm() != entryMode {
		t.Errorf("the archived dump is %v (%v), want mode %v", info, err, entryMode)
	}
}

// A DUMP OF ROWS BOUND TO ANOTHER DEPLOYMENT IS REFUSED AT BACKUP: an archive is
// where pairing two deployments would travel furthest before anybody noticed.
func TestADumpBoundToAnotherDeploymentIsRefusedAtBackup(t *testing.T) {
	d, migrations := pgDeployment(t)

	_, err := Write(t.Context(), BackupRequest{
		Dest: filepath.Join(t.TempDir(), "archive"), StateDir: d.stateDir,
		ConfigPath: d.configPath, DeploymentID: d.id, GitHub: d.github, AppKeyPEM: d.appKey,
		Dump: func(_ context.Context, path string) (LedgerDump, error) {
			return LedgerDump{
				Facts: DumpFacts{Schema: dumpSchema, DumpVersion: 16,
					Binding: "ffffffffffffffffffffffffffffffff"},
				Migrations: migrations,
			}, os.WriteFile(path, archiveDumpBody, 0o600)
		},
		Now: nowStub,
	})
	if err == nil || !strings.Contains(err.Error(), "ffffffffffffffffffffffffffffffff") {
		t.Fatalf("a dump of another deployment's rows = %v, want a refusal naming it", err)
	}
}

// A MANIFEST CANNOT CLAIM A DUMP IT CANNOT EXPRESS OR CONTRADICTS, and an
// archive of one kind may not carry the other kind's ledger entry.
func TestAManifestsDumpClaimIsHeldToItsSchema(t *testing.T) {
	f := newDumpFixture(t, bound)

	raw := read(t, filepath.Join(f.archive.Dir, EntryManifest))

	mutate := func(edit func(m map[string]any)) error {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}

		edit(m)

		body, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		_, err = decodeManifest(body)

		return err
	}

	object := func(m map[string]any, key string) map[string]any {
		inner, ok := m[key].(map[string]any)
		if !ok {
			t.Fatalf("the manifest's %s is not an object: %v", key, m[key])
		}

		return inner
	}
	ledger := func(m map[string]any) map[string]any { return object(m, "ledger") }
	dump := func(m map[string]any) map[string]any { return object(ledger(m), "dump") }

	for name, edit := range map[string]func(map[string]any){
		"schema 3":           func(m map[string]any) { m["schema"] = 3 },
		"external too":       func(m map[string]any) { ledger(m)["external"] = true },
		"another format":     func(m map[string]any) { dump(m)["format"] = "plain" },
		"no schema":          func(m map[string]any) { dump(m)["schema"] = "" },
		"no pg_dump version": func(m map[string]any) { dump(m)["dump_version"] = 0 },
		"another deployment": func(m map[string]any) { dump(m)["binding"] = "ffffffffffffffffffffffffffffffff" },
		"no engine":          func(m map[string]any) { ledger(m)["backend"] = "" },
	} {
		if err := mutate(edit); !errors.Is(err, errNotAnArchive) {
			t.Errorf("%s: decode = %v, want errNotAnArchive", name, err)
		}
	}

	if err := mutate(func(map[string]any) {}); err != nil {
		t.Fatalf("the unchanged manifest does not decode: %v", err)
	}

	if !knownEntries(f.archive.Manifest)[EntryLedgerDump] || knownEntries(f.archive.Manifest)[EntryLedger] {
		t.Error("a dump archive's entry set is not exactly the dump")
	}
}

// WHAT IS IN THE DATABASE DECIDES A RESTORE'S DISPOSITION, and "could not tell"
// is a refusal. The replacement-controller case is the one that matters: a
// database still holding this deployment's ledger keeps its rows, which are
// newer than the archive's.
func TestADumpRestorePlansByWhatTheDatabaseHolds(t *testing.T) {
	f := newDumpFixture(t, bound)
	newer := append(slices.Clone(f.migrations), state.AppliedMigration{Version: 9999, Name: "future"})

	// older is this deployment's own ledger at a migration before the archive's:
	// restored from something taken before this backup.
	older := f.restoredAs()
	older.Migrations = older.Migrations[:len(older.Migrations)-1]

	// unboundLedger is a populated billet ledger bound to no deployment.
	unboundLedger := f.restoredAs()
	unboundLedger.Binding = ""

	// notALedger is tables bound to this deployment with no migration
	// bookkeeping: some other program's.
	notALedger := f.restoredAs()
	notALedger.Tables = []string{"admission"}

	cases := []struct {
		name     string
		now      state.PostgresLedger
		attached bool
		peekErr  error
		want     Disposition
		refusal  string
	}{
		{name: "an empty schema", now: emptySchema(), want: Install},
		{name: "a preflight's untouched ledger",
			now: state.PostgresLedger{
				Schema: dumpSchema, Tables: []string{"admission", "schema_migrations"}, Migrations: f.migrations,
			},
			want: ReplaceEmptyLedger},
		{name: "empty tables with no billet migrations",
			now:     state.PostgresLedger{Schema: dumpSchema, Tables: []string{"reports"}},
			refusal: "no billet migrations"},
		{name: "this deployment's own ledger", now: f.restoredAs(), want: KeepLiveLedger},
		{name: "this deployment's own ledger, newer than the backup",
			now: func() state.PostgresLedger {
				l := f.restoredAs()
				l.Migrations = newer

				return l
			}(), want: KeepLiveLedger},
		{name: "this deployment's ledger, older than the backup", now: older,
			refusal: "the database is older than the backup"},
		{name: "this deployment's tables with no schema_migrations", now: notALedger,
			refusal: "it is not a billet ledger"},
		{name: "another deployment's ledger",
			now:     state.PostgresLedger{Schema: dumpSchema, Tables: []string{"x"}, Populated: true, Binding: "ff"},
			refusal: "bound to deployment ff"},
		{name: "a populated ledger bound to nobody", now: unboundLedger,
			refusal: "--external-ledger-attached"},
		{name: "a populated ledger bound to nobody, asserted", now: unboundLedger,
			attached: true, want: KeepLiveLedger},
		{name: "a populated ledger bound to nobody, asserted, with no schema_migrations",
			now:      state.PostgresLedger{Schema: dumpSchema, Tables: []string{"x"}, Populated: true},
			attached: true, refusal: "it is not a billet ledger"},
		{name: "a database that could not be read", now: emptySchema(),
			peekErr: errors.New("connection refused"), refusal: "could not read the PostgreSQL ledger"},
		{name: "a schema a newer billet prepared",
			now: state.PostgresLedger{
				Schema: dumpSchema, Tables: []string{"admission", "schema_migrations"}, Migrations: newer,
			},
			refusal: "newer than the one that took this backup"},
		{name: "another schema",
			now: state.PostgresLedger{Schema: "public"}, refusal: "resolves to schema public"},
		{name: "no schema at all", now: state.PostgresLedger{}, refusal: "resolves to no schema"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := f.ledgerFor(tc.now)
			ledger.peekErr = tc.peekErr

			tgt := pgTarget(t, f.d, ledger)
			tgt.ExternalLedgerAttached = tc.attached

			p, err := PlanRestore(t.Context(), f.archive, tgt)
			if err != nil {
				t.Fatalf("PlanRestore: %v", err)
			}

			if tc.refusal != "" {
				if len(refusalsMentioning(p, tc.refusal)) == 0 {
					t.Fatalf("no refusal says %q: %+v", tc.refusal, p.Refusals)
				}

				return
			}

			if len(p.Refusals) > 0 {
				t.Fatalf("refused: %+v", p.Refusals)
			}

			if got := p.dispositionOf(EntryLedgerDump); got != tc.want {
				t.Errorf("the ledger is %q, want %q", got, tc.want)
			}
		})
	}
}

// AND A DUMP GOES ONLY INTO A HOST CONFIGURED FOR ITS ENGINE: onto a SQLite
// host it would be restored nowhere and the control plane would start empty.
func TestADumpIsRefusedOntoASQLiteHost(t *testing.T) {
	f := newDumpFixture(t, bound)

	tgt := newTarget(t, f.d.github)

	p, err := PlanRestore(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(refusalsMentioning(p, "empty ledger of its own")) == 0 {
		t.Fatalf("a dump onto a SQLite host was not refused for that: %+v", p.Refusals)
	}
}

// executeOn plans and executes a restore or recovery with the fake.
func executeOn(t *testing.T, a *Archive, tgt Target, intent Intent) (Result, error) {
	t.Helper()

	plan, err := planFor(t.Context(), a, tgt, intent)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("refused: %+v", plan.Refusals)
	}

	return Execute(t.Context(), RestoreRequest{
		Plan: plan, InstallAppKey: testInstallAppKey, Actor: "test",
		Now: func() time.Time { return time.Unix(1_700_000_100, 0).UTC() },
	})
}

// A RESTORE HANDS THE DATABASE A COPY IT VERIFIED, after the barrier, and reads
// the archive's ledger back before it lifts anything.
func TestARestoreOfADumpRestoresAVerifiedCopyAndReadsItBack(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(emptySchema())
	tgt := pgTarget(t, f.d, ledger)

	res, err := executeOn(t, f.archive, tgt, RestoreFresh)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !slices.Equal(ledger.restored, []string{digest(archiveDumpBody)}) {
		t.Fatalf("the database was handed %v, want the archive's dump once", ledger.restored)
	}

	// WITH THE ARCHIVE'S OWN pg_dump MAJOR AND SCHEMA, which decide which
	// pg_restore may read it and where its tables go.
	// AND INTO THE DATABASE IT JOURNALED, WITH NO LEDGER THERE: the writer
	// re-proves both on its own connection before it drops anything.
	wantInto := []state.RestoreInto{{Database: dumpDatabase, Schema: dumpSchema}}
	if !slices.Equal(ledger.versions, []int{16}) || !slices.Equal(ledger.intos, wantInto) {
		t.Errorf("the restore was handed versions %v and targets %+v, want [16] and %+v",
			ledger.versions, ledger.intos, wantInto)
	}

	// A COPY THIS RUN VERIFIED, never the archive's own pathname, whose bytes
	// were checked once by Open and could have changed since.
	rel, err := filepath.Rel(f.archive.Dir, ledger.paths[0])
	if err != nil {
		t.Fatalf("relate %s to the archive: %v", ledger.paths[0], err)
	}

	if !strings.HasPrefix(rel, "..") {
		t.Errorf("pg_restore was handed the archive's own file %s", ledger.paths[0])
	}

	if _, err := os.Lstat(ledger.paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the verified copy %s was left behind (%v)", ledger.paths[0], err)
	}

	calls := ledger.callsSnapshot()
	if b, r := slices.Index(calls, "barrier"), slices.Index(calls, "restore"); b < 0 || b > r {
		t.Errorf("the barrier did not precede the restore: %v", calls)
	}

	if last := calls[len(calls)-1]; last != "peek" {
		t.Errorf("the restore was not read back: calls end %v", calls)
	}

	if !slices.ContainsFunc(res.Installed, func(a Action) bool { return a.Entry == EntryLedgerDump }) {
		t.Errorf("the result does not report the ledger installed: %+v", res.Installed)
	}

	assertOpen(t, tgt.StateDir)

	if read(t, filepath.Join(tgt.StateDir, "deployment-id")) == nil {
		t.Error("the identity was not installed beside the ledger")
	}
}

// assertOpen proves a directory is no longer fenced and holds no journal.
func assertOpen(t *testing.T, stateDir string) {
	t.Helper()

	if _, fenced, err := state.MaintenanceFenceReason(stateDir); err != nil || fenced {
		t.Errorf("%s is still fenced (%v)", stateDir, err)
	}

	if _, err := os.Lstat(JournalPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the journal is still there (%v)", err)
	}
}

// assertFenced proves a directory is still closed, with its journal.
func assertFenced(t *testing.T, stateDir string) {
	t.Helper()

	if _, fenced, err := state.MaintenanceFenceReason(stateDir); err != nil || !fenced {
		t.Errorf("%s is not fenced (%v)", stateDir, err)
	}

	if _, err := os.Lstat(JournalPath(stateDir)); err != nil {
		t.Errorf("the journal is gone (%v)", err)
	}
}

// A RESTORE WHOSE READ-BACK DISAGREES KEEPS THE FENCE: billet cannot vouch that
// pg_restore put this backup's ledger where billet reads it.
func TestARestoreWhoseReadBackDisagreesKeepsTheFence(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(emptySchema())

	wrong := f.restoredAs()
	wrong.Migrations = wrong.Migrations[:len(wrong.Migrations)-1]
	ledger.byDigest[digest(archiveDumpBody)] = wrong

	tgt := pgTarget(t, f.d, ledger)

	if _, err := executeOn(t, f.archive, tgt, RestoreFresh); err == nil ||
		!strings.Contains(err.Error(), "does not record the migrations") {
		t.Fatalf("Execute = %v, want the read-back's refusal", err)
	}

	assertFenced(t, tgt.StateDir)
}

// A RESUME AFTER THE RESTORE COMMITTED FINDS ITS OWN WORK through the journal:
// a ledger never bound to a deployment cannot say whose it is, and without the
// record a re-plan would refuse the run's own restore as somebody's populated
// schema.
func TestAResumeAfterTheDumpRestoredFindsItsOwnWork(t *testing.T) {
	f := newDumpFixture(t, unbound)
	ledger := f.ledgerFor(emptySchema())
	tgt := pgTarget(t, f.d, ledger)

	interrupted := errors.New("interrupted after the ledger")

	stopBefore(t, AuthorityEntry("ca.key"), interrupted)

	if _, err := executeOn(t, f.archive, tgt, RestoreFresh); !errors.Is(err, interrupted) {
		t.Fatalf("the first run = %v, want the interruption", err)
	}

	prog, err := InProgress(tgt.StateDir)
	if err != nil || !prog.LedgerRestored {
		t.Fatalf("the journal does not record the restore (%+v, %v)", prog, err)
	}

	onPublish = nil

	if _, err := executeOn(t, f.archive, tgt, RestoreFresh); err != nil {
		t.Fatalf("the resume: %v", err)
	}

	if len(ledger.restored) != 1 {
		t.Errorf("the dump was restored %d times; the resume should have found it done", len(ledger.restored))
	}

	assertOpen(t, tgt.StateDir)
}

// recoverTarget is the deployment's own host, its database holding its ledger.
func recoverTarget(t *testing.T, f dumpFixture, ledger PostgresLedger) Target {
	t.Helper()

	return Target{
		ConfigPath: f.d.configPath, StateDir: f.d.stateDir, AppKeyPath: f.d.appKeyPath,
		GitHub: f.d.github, LedgerBackend: "postgres", Postgres: ledger,
	}
}

// liveLedger is the database's ledger as the recovery finds it: this
// deployment's, and newer than the archive, by rows and by a migration, so a
// put-back of it reads back differently from the archive's ledger.
func (f dumpFixture) liveLedger() state.PostgresLedger {
	l := f.restoredAs()
	l.NonEmpty = []string{"leases (3)"}
	l.Migrations = append(slices.Clone(f.migrations), state.AppliedMigration{Version: 9998, Name: "later"})

	return l
}

// A RECOVERY KEEPS A DUMP OF WHAT IT REPLACES BEFORE IT REPLACES IT, journals
// its digest before it publishes it, and leaves the directory fenced for the
// seal; Finish then lifts the fence only over an operator's seal read through
// the database.
func TestARecoverKeepsADumpOfWhatItReplacesFirst(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(f.liveLedger())
	tgt := recoverTarget(t, f, ledger)

	res, err := executeOn(t, f.archive, tgt, ReplaceLedger)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := ledger.callsSnapshot()
	if d, r := slices.Index(calls, "dump"), slices.Index(calls, "restore"); d < 0 || r < d {
		t.Fatalf("the copy was not taken before the restore: %v", calls)
	}

	copyPath := supersededDumpPath(f.archive, tgt.StateDir)

	if !slices.Equal(res.Superseded, []string{copyPath}) {
		t.Errorf("Superseded = %v, want %s", res.Superseded, copyPath)
	}

	if got := read(t, copyPath); !bytes.Equal(got, ledger.dumpBody) {
		t.Errorf("the kept copy holds %q, want what the database held", got)
	}

	if info, err := os.Stat(copyPath); err != nil || info.Mode().Perm() != entryMode {
		t.Errorf("the kept copy is %v (%v), want mode %v", info, err, entryMode)
	}

	raw := read(t, JournalPath(tgt.StateDir))

	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatalf("decode the journal: %v", err)
	}

	if j.SupersededDigests[filepath.Base(copyPath)] != digest(ledger.dumpBody) || !j.LedgerRestored {
		t.Errorf("the journal records %+v", j)
	}

	if !res.Unfinished {
		t.Fatal("a recovery reported itself finished before its seal")
	}

	plan, err := PlanRecover(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	ledger.admission = state.Admission{Mode: state.AdmissionOpen}

	if err := Finish(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "rather than sealed") {
		t.Fatalf("Finish over an open admission = %v, want a refusal", err)
	}

	assertFenced(t, tgt.StateDir)

	ledger.admission = state.Admission{Mode: state.AdmissionSealed, Provenance: state.ProvenanceOperator}

	if err := Finish(t.Context(), plan); err != nil {
		t.Fatalf("Finish over an operator's seal: %v", err)
	}

	assertOpen(t, tgt.StateDir)
}

// AN ABANDONED RECOVERY PUTS THE LEDGER BACK FROM ITS COPY, and keeps the copy
// under a name a later recovery will not take for its own.
func TestAnAbandonedRecoverPutsTheLedgerBackFromItsCopy(t *testing.T) {
	f := newDumpFixture(t, bound)
	live := f.liveLedger()
	ledger := f.ledgerFor(live)
	ledger.restoreErr = errors.New("the archive's restore was interrupted")
	tgt := recoverTarget(t, f, ledger)

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); !errors.Is(err, ledger.restoreErr) {
		t.Fatalf("Execute = %v, want the interrupted restore", err)
	}

	assertFenced(t, tgt.StateDir)

	ledger.restoreErr = nil
	before := len(ledger.callsSnapshot())

	res, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if !slices.Equal(ledger.restored, []string{digest(ledger.dumpBody)}) {
		t.Errorf("the abandon restored %v, want the copy it kept", ledger.restored)
	}

	// WITH THE MAJOR OF THE pg_dump THAT TOOK THE COPY, not the archive's: the
	// first restore was the archive's, interrupted, and the second is the copy.
	if !slices.Equal(ledger.versions, []int{16, takenDumpVersion}) {
		t.Errorf("the restores were handed pg_dump majors %v, want [16 %d]", ledger.versions, takenDumpVersion)
	}

	// BOTH OVER THIS DEPLOYMENT'S LEDGER, in the database the recovery journaled.
	ours := state.RestoreInto{Database: dumpDatabase, Schema: dumpSchema, Binding: f.d.id}
	if !slices.Equal(ledger.intos, []state.RestoreInto{ours, ours}) {
		t.Errorf("the restores were handed targets %+v, want %+v twice", ledger.intos, ours)
	}

	// AND ONLY ONCE NO BILLET WRITER ON ANY HOST IS MID-COMMIT.
	abandon := ledger.callsSnapshot()[before:]
	if b, r := slices.Index(abandon, "barrier"), slices.Index(abandon, "restore"); b < 0 || r < b {
		t.Errorf("the abandon's barrier did not precede its restore: %v", abandon)
	}

	copyPath := supersededDumpPath(f.archive, tgt.StateDir)

	if _, err := os.Lstat(copyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy is still at its recovery name (%v)", err)
	}

	if _, err := os.Lstat(keptCopyPath(copyPath, digest(ledger.dumpBody))); err != nil {
		t.Errorf("the copy was not kept aside: %v", err)
	}

	if len(res.Restored) != 1 {
		t.Errorf("Restored = %v", res.Restored)
	}

	assertOpen(t, tgt.StateDir)
}

// AN ABANDON REFUSES A COPY IT CANNOT VOUCH FOR, fence and journal standing,
// and restores nothing.
func TestAnAbandonRefusesACopyItCannotVouchFor(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(f.liveLedger())
	ledger.restoreErr = errors.New("interrupted")
	tgt := recoverTarget(t, f, ledger)

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); err == nil {
		t.Fatal("Execute succeeded through an interrupted restore")
	}

	copyPath := supersededDumpPath(f.archive, tgt.StateDir)
	if err := os.WriteFile(copyPath, []byte("PGDMP somebody else's"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	ledger.restoreErr = nil

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "not the copy this recovery took") {
		t.Fatalf("Abandon over a tampered copy = %v, want a refusal", err)
	}

	if len(ledger.restored) != 0 {
		t.Errorf("the abandon restored %v from a copy it could not vouch for", ledger.restored)
	}

	assertFenced(t, tgt.StateDir)
}

// AN ABANDON BEFORE THE COPY EXISTS TOUCHES NO DATABASE: the archive's restore
// runs only after the copy is durable, so no copy means nothing was replaced.
func TestAnAbandonBeforeTheCopyTouchesNoDatabase(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(f.liveLedger())
	ledger.dumpErr = errors.New("pg_dump failed")
	tgt := recoverTarget(t, f, ledger)

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); !errors.Is(err, ledger.dumpErr) {
		t.Fatalf("Execute = %v, want the failed dump", err)
	}

	if slices.Contains(ledger.callsSnapshot(), "restore") {
		t.Fatal("the archive's dump was restored although no copy of the ledger was kept")
	}

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if slices.Contains(ledger.callsSnapshot(), "restore") {
		t.Error("the abandon restored something although nothing was replaced")
	}

	assertOpen(t, tgt.StateDir)
}

// A RESTORE'S ABANDON SAYS WHAT IT LEFT IN THE DATABASE rather than dropping a
// schema.
func TestARestoresAbandonSaysWhatItLeftInTheDatabase(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(emptySchema())
	tgt := pgTarget(t, f.d, ledger)

	stopBefore(t, AuthorityEntry("ca.key"), errStopHere)

	if _, err := executeOn(t, f.archive, tgt, RestoreFresh); !errors.Is(err, errStopHere) {
		t.Fatalf("Execute = %v, want the interruption", err)
	}

	res, err := Abandon(t.Context(), f.archive, tgt, RestoreFresh)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if res.LedgerLeftInDatabase != dumpSchema {
		t.Errorf("LedgerLeftInDatabase = %q, want %q", res.LedgerLeftInDatabase, dumpSchema)
	}

	if n := len(ledger.restored); n != 1 {
		t.Errorf("the abandon touched the database: %d restores", n)
	}

	assertOpen(t, tgt.StateDir)
}

// interruptedRecovery is a recovery that published its copy of the ledger and
// was interrupted in the archive's restore: the state every abandon test below
// starts from.
func interruptedRecovery(t *testing.T) (dumpFixture, *fakeLedger, Target) {
	t.Helper()

	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(f.liveLedger())
	ledger.restoreErr = errors.New("the archive's restore was interrupted")
	tgt := recoverTarget(t, f, ledger)

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); !errors.Is(err, ledger.restoreErr) {
		t.Fatalf("Execute = %v, want the interrupted restore", err)
	}

	ledger.restoreErr = nil
	ledger.restored = nil

	return f, ledger, tgt
}

// AN ABANDON WHOSE COPY IS GONE AFTER IT WAS PUBLISHED REFUSES: the journal says
// the archive's restore was allowed to run, so the database may hold the
// archive's ledger or the operator's, and only that copy could say which.
func TestAnAbandonWhoseCopyIsGoneRefuses(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	if err := os.Remove(supersededDumpPath(f.archive, tgt.StateDir)); err != nil {
		t.Fatalf("remove the copy: %v", err)
	}

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "it is gone") {
		t.Fatalf("Abandon with the copy gone = %v, want a refusal", err)
	}

	if len(ledger.restored) != 0 {
		t.Errorf("the abandon restored %v with no copy to restore", ledger.restored)
	}

	assertFenced(t, tgt.StateDir)
}

// AN ABANDON PUTS THE LEDGER BACK ONLY INTO THE DATABASE IT CAME FROM, at the
// archive's schema and bound to this deployment or nobody; a connection pointed
// elsewhere since is refused before anything is restored.
func TestAnAbandonRefusesADatabaseTheCopyDidNotComeFrom(t *testing.T) {
	cases := map[string]struct {
		edit    func(*state.PostgresLedger)
		refusal string
	}{
		"another database": {
			edit:    func(l *state.PostgresLedger) { l.Database = "elsewhere" },
			refusal: "the connection now reaches elsewhere",
		},
		"another schema": {
			edit:    func(l *state.PostgresLedger) { l.Schema = "public" },
			refusal: `now resolves to "public"`,
		},
		"another deployment's ledger": {
			edit:    func(l *state.PostgresLedger) { l.Binding = "ffffffffffffffffffffffffffffffff" },
			refusal: `bound to "ffffffffffffffffffffffffffffffff", not to this deployment`,
		},
		// AN UNBOUND DATABASE IS NOT THIS DEPLOYMENT'S: the recovery replaced only a
		// ledger bound to it, and every state since holds that binding, so a
		// database of the same name on another server is refused.
		"a ledger bound to nobody": {
			edit:    func(l *state.PostgresLedger) { l.Binding = "" },
			refusal: `bound to "", not to this deployment`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, ledger, tgt := interruptedRecovery(t)

			tc.edit(&ledger.now)

			if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
				!strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("Abandon = %v, want a refusal saying %q", err, tc.refusal)
			}

			if len(ledger.restored) != 0 {
				t.Errorf("the abandon restored %v into a database it could not vouch for", ledger.restored)
			}

			assertFenced(t, tgt.StateDir)
		})
	}
}

// AN ABANDON READS THE LEDGER BACK AFTER THE PUT-BACK and keeps the fence over
// one bound to another deployment: the copy was this deployment's, so a foreign
// binding afterwards means the restore did not put it where billet reads.
func TestAnAbandonKeepsTheFenceOverAForeignLedgerAfterThePutBack(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	foreign := f.liveLedger()
	foreign.Binding = "ffffffffffffffffffffffffffffffff"
	ledger.byDigest[digest(ledger.dumpBody)] = foreign

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "after the put-back") {
		t.Fatalf("Abandon = %v, want the read-back's refusal", err)
	}

	assertFenced(t, tgt.StateDir)
}

// A RESUMED RECOVERY REFUSES A CONNECTION THAT NOW REACHES ANOTHER DATABASE than
// the one its copy was taken from, before it restores anything there.
func TestAResumedRecoveryRefusesAnotherDatabase(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	ledger.now.Database = "elsewhere"

	// REFUSED AT THE PLAN, before any disposition is chosen.
	p, err := PlanRecover(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "now reaches elsewhere")) == 0 {
		t.Fatalf("the resume onto another database planned without refusing: %+v", p.Refusals)
	}

	// AND AT THE WRITE, for a plan made before the connection moved: the
	// database the journal holds is re-read on the connection that writes.
	j := journal{LedgerDatabase: dumpDatabase}

	if err := bindLedgerDatabase(t.TempDir(), &j, state.PostgresLedger{Database: "elsewhere"}); err == nil ||
		!strings.Contains(err.Error(), "the connection now reaches elsewhere") {
		t.Fatalf("binding a run that began in %s to elsewhere = %v, want a refusal", dumpDatabase, err)
	}

	if err := bindLedgerDatabase(t.TempDir(), &j, state.PostgresLedger{Database: dumpDatabase}); err != nil {
		t.Fatalf("binding a run to the database it began in: %v", err)
	}

	if len(ledger.restored) != 0 {
		t.Errorf("the resume restored %v into another database", ledger.restored)
	}

	assertFenced(t, tgt.StateDir)
}

// A RECOVERY REPLACES A LEDGER NEWER THAN THE ARCHIVE, because the restore
// drops what the archive's dump does not name; and it refuses another
// deployment's ledger and a host that is not this deployment.
func TestARecoverPlansByWhoseLedgerItIs(t *testing.T) {
	f := newDumpFixture(t, bound)

	ahead := f.liveLedger()
	ahead.Migrations = append(slices.Clone(f.migrations), state.AppliedMigration{Version: 9999, Name: "future"})

	p, err := PlanRecover(t.Context(), f.archive, recoverTarget(t, f, f.ledgerFor(ahead)))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(p.Refusals) > 0 || p.dispositionOf(EntryLedgerDump) != SupersedeLedger {
		t.Errorf("a ledger ahead of the archive plans as %q (%+v), want %q",
			p.dispositionOf(EntryLedgerDump), p.Refusals, SupersedeLedger)
	}

	foreign := f.liveLedger()
	foreign.Binding = "ffffffffffffffffffffffffffffffff"

	p, err = PlanRecover(t.Context(), f.archive, recoverTarget(t, f, f.ledgerFor(foreign)))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "bound to deployment ffffffffffffffffffffffffffffffff")) == 0 {
		t.Errorf("a recovery onto another deployment's ledger was not refused for it: %+v", p.Refusals)
	}

	p, err = PlanRecover(t.Context(), f.archive, pgTarget(t, f.d, f.ledgerFor(f.liveLedger())))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "back over ITSELF")) == 0 {
		t.Errorf("a recovery onto a host that is not this deployment was not refused: %+v", p.Refusals)
	}

	// A LEDGER NO CONTROLLER CLAIMED, OR AN ARCHIVE OF ONE, IS NOT RECOVERED: the
	// binding is what every later pass holds the database to.
	unclaimed := f.liveLedger()
	unclaimed.Binding = ""

	p, err = PlanRecover(t.Context(), f.archive, recoverTarget(t, f, f.ledgerFor(unclaimed)))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "bound to no deployment")) == 0 {
		t.Errorf("a recovery onto an unbound ledger was not refused for it: %+v", p.Refusals)
	}

	u := newDumpFixture(t, unbound)
	claimedSince := u.liveLedger()
	claimedSince.Binding = u.d.id

	p, err = PlanRecover(t.Context(), u.archive, recoverTarget(t, u, u.ledgerFor(claimedSince)))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "this backup's ledger is bound to")) == 0 {
		t.Errorf("a recovery from an unbound archive was not refused for it: %+v", p.Refusals)
	}
}

// A BARRIER THAT CANNOT BE HAD STOPS THE RESTORE BEFORE ANYTHING IS PUBLISHED,
// and lifts the fence it raised over a directory nothing touched.
func TestARestoreWhoseDatabaseBarrierFailsPublishesNothing(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(emptySchema())
	ledger.barrierErr = errors.New("a writer held the lock past the limit")
	tgt := pgTarget(t, f.d, ledger)

	if _, err := executeOn(t, f.archive, tgt, RestoreFresh); !errors.Is(err, ledger.barrierErr) {
		t.Fatalf("Execute = %v, want the barrier's failure", err)
	}

	if slices.Contains(ledger.callsSnapshot(), "restore") {
		t.Error("the dump was restored although the barrier was never had")
	}

	if _, err := os.Lstat(filepath.Join(tgt.StateDir, "deployment-id")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the identity was published although the barrier was never had (%v)", err)
	}

	assertOpen(t, tgt.StateDir)
}

// A RESUMED RECOVERY WHOSE PUBLISHED COPY IS GONE STOPS, and takes no second
// copy: the archive's restore may have begun, so a copy now could hold a ledger
// part-way replaced, recorded as the operator's.
func TestAResumedRecoveryWhoseCopyIsGoneTakesNoSecondCopy(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	if err := os.Remove(supersededDumpPath(f.archive, tgt.StateDir)); err != nil {
		t.Fatalf("remove the copy: %v", err)
	}

	before := len(ledger.callsSnapshot())

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "is gone") {
		t.Fatalf("the resume = %v, want a refusal", err)
	}

	resumed := ledger.callsSnapshot()[before:]
	if slices.Contains(resumed, "dump") || slices.Contains(resumed, "restore") {
		t.Errorf("the resume went on without its copy: %v", resumed)
	}

	assertFenced(t, tgt.StateDir)
}

// AN ABANDON THAT STOPPED AFTER IT PUT THE LEDGER BACK FINISHES: its copy is at
// the kept name with this recovery's digest, and the ledger reads back as the
// copy's. A kept copy that is not this recovery's is no such proof.
func TestAnAbandonThatStoppedAfterItsPutBackFinishes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keptBody []byte
		wantErr  string
	}{
		{name: "the copy at the kept name"},
		{name: "somebody else's file at the kept name", keptBody: []byte("PGDMP not this one"),
			wantErr: "it is gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ledger, tgt := interruptedRecovery(t)

			copyPath := supersededDumpPath(f.archive, tgt.StateDir)
			kept := keptCopyPath(copyPath, digest(ledger.dumpBody))

			if tc.keptBody == nil {
				if err := os.Link(copyPath, kept); err != nil {
					t.Fatalf("keep the copy: %v", err)
				}
			} else if err := os.WriteFile(kept, tc.keptBody, 0o600); err != nil {
				t.Fatalf("write the kept name: %v", err)
			}

			if err := os.Remove(copyPath); err != nil {
				t.Fatalf("remove the copy: %v", err)
			}

			// THE PUT-BACK HAD COMMITTED: the database holds what the copy held.
			ledger.now = ledger.byDigest[digest(ledger.dumpBody)]

			_, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Abandon = %v, want a refusal saying %q", err, tc.wantErr)
				}

				assertFenced(t, tgt.StateDir)

				return
			}

			if err != nil {
				t.Fatalf("Abandon: %v", err)
			}

			if len(ledger.restored) != 0 {
				t.Errorf("the finishing abandon restored %v again", ledger.restored)
			}

			if sum, _, err := digestFile(kept); err != nil || sum != digest(ledger.dumpBody) {
				t.Errorf("the kept copy is not this recovery's (%s, %v)", sum, err)
			}

			assertOpen(t, tgt.StateDir)
		})
	}
}

// AND THE FINISHING ABANDON READS THE LEDGER BACK WHERE IT PUT IT: a sibling
// schema holding the same deployment's ledger at the same migrations is not the
// put-back.
func TestAFinishingAbandonReadsBackItsOwnSchema(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	copyPath := supersededDumpPath(f.archive, tgt.StateDir)
	if err := os.Link(copyPath, keptCopyPath(copyPath, digest(ledger.dumpBody))); err != nil {
		t.Fatalf("keep the copy: %v", err)
	}

	if err := os.Remove(copyPath); err != nil {
		t.Fatalf("remove the copy: %v", err)
	}

	ledger.now = ledger.byDigest[digest(ledger.dumpBody)]
	ledger.now.Schema = "billet_sibling"

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), `read from schema "billet_sibling"`) {
		t.Fatalf("Abandon over a sibling schema = %v, want a refusal", err)
	}

	assertFenced(t, tgt.StateDir)
}

// A RESTORE THAT BEGAN WRITING IN ONE DATABASE IS HELD TO IT BEFORE ANY
// DISPOSITION: resumed after its pg_restore committed and before its journal
// said so, against another database holding this deployment's ledger, it would
// otherwise plan that ledger as one to keep and lift the fence over it.
func TestAResumedRestoreIsHeldToItsDatabaseBeforeAnyDisposition(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(emptySchema())
	ledger.restoreErr = errors.New("killed after pg_restore committed")
	tgt := pgTarget(t, f.d, ledger)

	if _, err := executeOn(t, f.archive, tgt, RestoreFresh); !errors.Is(err, ledger.restoreErr) {
		t.Fatalf("Execute = %v, want the interruption", err)
	}

	elsewhere := f.restoredAs()
	elsewhere.Database = "elsewhere"
	ledger.now = elsewhere
	ledger.restoreErr = nil

	p, err := PlanRestore(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(refusalsMentioning(p, "began writing the ledger in database billet")) == 0 {
		t.Fatalf("a resume onto another database planned %+v with refusals %+v",
			p.dispositionOf(EntryLedgerDump), p.Refusals)
	}
}

// THE PUT-BACK IS READ BACK AGAINST THE COPY'S MIGRATIONS, which the archive's
// ledger, bound to the same deployment, does not share: a restore that left
// the archive's ledger in place is not a put-back.
func TestAnAbandonReadsThePutBackAgainstTheCopysMigrations(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	ledger.byDigest[digest(ledger.dumpBody)] = f.restoredAs()

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "does not record the migrations the copy did") {
		t.Fatalf("Abandon = %v, want the read-back's refusal", err)
	}

	assertFenced(t, tgt.StateDir)
}

// A RESUME AFTER THE RESTORE COMMITTED, A FINISH AND THE SEAL'S PROOF ALL HOLD
// THE CONNECTION TO THE DATABASE THE LEDGER WENT INTO: nothing after the
// restore restores anything, so each would otherwise seal or open whatever the
// connection reaches by then.
func TestEveryStepAfterTheRestoreProvesItsDatabase(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(f.liveLedger())
	tgt := recoverTarget(t, f, ledger)

	// THE LAST ITEM IS THE LEDGER on the deployment's own host, so the run is
	// stopped after publication and before its journal says so.
	prev := onPublished
	onPublished = func() error { return errStopHere }

	t.Cleanup(func() { onPublished = prev })

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); !errors.Is(err, errStopHere) {
		t.Fatalf("Execute = %v, want the interruption", err)
	}

	onPublished = nil

	if prog, err := InProgress(tgt.StateDir); err != nil || !prog.LedgerRestored {
		t.Fatalf("the interrupted run did not record its restore (%+v, %v)", prog, err)
	}

	plan, err := PlanRecover(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if err := ProveRestoredLedger(t.Context(), plan); err != nil {
		t.Fatalf("ProveRestoredLedger over the database restored into: %v", err)
	}

	ledger.now.Database = "elsewhere"

	if err := ProveRestoredLedger(t.Context(), plan); err == nil ||
		!strings.Contains(err.Error(), "the connection now reaches elsewhere") {
		t.Errorf("ProveRestoredLedger over another database = %v, want a refusal", err)
	}

	replanned, err := PlanRecover(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(replanned, "now reaches elsewhere")) == 0 {
		t.Errorf("a resume onto another database was not refused: %+v", replanned.Refusals)
	}

	// THE RESUME'S OWN PROOF, past the database: the same database holding a
	// ledger bound otherwise is not the one this run restored.
	ledger.now.Database = dumpDatabase
	ledger.now.Binding = "ffffffffffffffffffffffffffffffff"

	rebound, err := PlanRecover(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(rebound, "not proved to be the one it went into")) == 0 {
		t.Errorf("a resume onto a ledger bound otherwise was not refused: %+v", rebound.Refusals)
	}

	ledger.now.Binding = f.d.id
	ledger.now.Database = dumpDatabase

	// THE PUBLISHED COPY GONE: the only record of the ledger replaced, so the
	// resume, the seal's proof and the finish all refuse to go on without it.
	copyPath := supersededDumpPath(f.archive, tgt.StateDir)
	moved := copyPath + ".moved"

	if err := os.Rename(copyPath, moved); err != nil {
		t.Fatalf("move the copy: %v", err)
	}

	if err := ProveRestoredLedger(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Errorf("ProveRestoredLedger without the copy = %v, want a refusal", err)
	}

	withoutCopy, err := PlanRecover(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(withoutCopy, "is gone")) == 0 {
		t.Errorf("a resume without the copy was not refused: %+v", withoutCopy.Refusals)
	}

	if err := os.Rename(moved, copyPath); err != nil {
		t.Fatalf("put the copy back: %v", err)
	}

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); err != nil {
		t.Fatalf("the resume: %v", err)
	}

	ledger.admission = state.Admission{Mode: state.AdmissionSealed, Provenance: state.ProvenanceOperator}
	ledger.now.Database = "elsewhere"

	if err := Finish(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "now reaches elsewhere") {
		t.Fatalf("Finish over another database = %v, want a refusal", err)
	}

	assertFenced(t, tgt.StateDir)

	ledger.now.Database = dumpDatabase

	if err := os.Rename(copyPath, moved); err != nil {
		t.Fatalf("move the copy: %v", err)
	}

	if err := Finish(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("Finish without the copy = %v, want a refusal", err)
	}

	assertFenced(t, tgt.StateDir)

	if err := os.Rename(moved, copyPath); err != nil {
		t.Fatalf("put the copy back: %v", err)
	}

	if err := Finish(t.Context(), plan); err != nil {
		t.Fatalf("Finish over the database restored into: %v", err)
	}

	assertOpen(t, tgt.StateDir)
}

// A JOURNAL THAT RECORDS WORK AND NOT THE DATABASE COULD NOT TELL where it went,
// and neither a resume nor an abandon reads that as "anywhere".
func TestAJournalWithoutItsDatabaseIsCouldNotTell(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	path := JournalPath(tgt.StateDir)

	var raw map[string]any
	if err := json.Unmarshal(read(t, path), &raw); err != nil {
		t.Fatalf("decode the journal: %v", err)
	}

	if raw["ledger_database"] != dumpDatabase {
		t.Fatalf("the journal records ledger_database %v, want %s", raw["ledger_database"], dumpDatabase)
	}

	delete(raw, "ledger_database")

	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode the journal: %v", err)
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the journal: %v", err)
	}

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "without saying which") {
		t.Errorf("a resume over a journal with no database = %v, want a refusal", err)
	}

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "does not say which database") {
		t.Errorf("an abandon over a journal with no database = %v, want a refusal", err)
	}

	if len(ledger.restored) != 0 {
		t.Errorf("a journal that could not tell led to restores %v", ledger.restored)
	}

	assertFenced(t, tgt.StateDir)
}

// THE KEPT NAME IS NEVER WRITTEN OVER: an abandon that finds other bytes there
// refuses, fenced, with its copy still at the recovery's name.
func TestAnAbandonWritesOverNoKeptCopy(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	copyPath := supersededDumpPath(f.archive, tgt.StateDir)
	kept := keptCopyPath(copyPath, digest(ledger.dumpBody))

	if err := os.WriteFile(kept, []byte("PGDMP somebody's"), 0o600); err != nil {
		t.Fatalf("write the kept name: %v", err)
	}

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "holds other bytes") {
		t.Fatalf("Abandon over another file at the kept name = %v, want a refusal", err)
	}

	if got := read(t, kept); string(got) != "PGDMP somebody's" {
		t.Errorf("the kept name was written over: %q", got)
	}

	if _, err := os.Lstat(copyPath); err != nil {
		t.Errorf("the copy is gone from the recovery's name (%v)", err)
	}

	assertFenced(t, tgt.StateDir)
}

// A RECOVERY RESUMED AFTER ITS ABANDON PUT THE LEDGER BACK says to finish the
// abandon: putting the copy back for the recovery would run it forward over the
// ledger the abandon restored.
func TestARecoveryResumedAfterItsAbandonSaysToFinishTheAbandon(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	copyPath := supersededDumpPath(f.archive, tgt.StateDir)
	if err := os.Link(copyPath, keptCopyPath(copyPath, digest(ledger.dumpBody))); err != nil {
		t.Fatalf("keep the copy: %v", err)
	}

	if err := os.Remove(copyPath); err != nil {
		t.Fatalf("remove the copy: %v", err)
	}

	ledger.now = ledger.byDigest[digest(ledger.dumpBody)]

	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); err == nil ||
		!strings.Contains(err.Error(), "run `billet local recover --abandon` again") {
		t.Fatalf("the resumed recovery = %v, want it to name the abandon", err)
	}

	if len(ledger.restored) != 0 {
		t.Errorf("the resumed recovery restored %v", ledger.restored)
	}

	assertFenced(t, tgt.StateDir)
}

// AN ABANDON THAT HAS BEGUN IS THE ONLY WAY ON. Interrupted after its put-back
// committed and before it kept its copy, it leaves a ledger a forward run would
// re-prove as plausible (same database, binding and leading migrations) and
// finish the recovery over; the journal's mark refuses that run, and Finish,
// and a second abandon completes.
func TestAForwardRunAfterAnAbandonBeganIsRefused(t *testing.T) {
	f := newDumpFixture(t, bound)
	ledger := f.ledgerFor(f.liveLedger())
	tgt := recoverTarget(t, f, ledger)

	// A RECOVERY THAT PUBLISHED AND WAITS FOR ITS SEAL: the state a forward run
	// would finish, so Finish's refusal is its own and not the publication's.
	if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ledger.restored = nil

	copyPath := supersededDumpPath(f.archive, tgt.StateDir)
	kept := keptCopyPath(copyPath, digest(ledger.dumpBody))

	// THE ABANDON STOPS AFTER ITS PUT-BACK: something it cannot read where it
	// keeps the copy.
	if err := os.Mkdir(kept, 0o700); err != nil {
		t.Fatalf("block the kept name: %v", err)
	}

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err == nil {
		t.Fatal("the abandon finished although it could not keep its copy")
	}

	if !slices.Equal(ledger.restored, []string{digest(ledger.dumpBody)}) {
		t.Fatalf("the abandon restored %v, want its copy, before it stopped", ledger.restored)
	}

	prog, err := InProgress(tgt.StateDir)
	if err != nil || !prog.Abandoning {
		t.Fatalf("the journal does not record the abandon that began (%+v, %v)", prog, err)
	}

	p, err := PlanRecover(t.Context(), f.archive, tgt)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "has already begun undoing it")) == 0 {
		t.Errorf("a forward recovery after an abandon began was planned: %+v", p.Refusals)
	}

	ledger.admission = state.Admission{Mode: state.AdmissionSealed, Provenance: state.ProvenanceOperator}

	// THE SEAL'S PROOF REFUSES IT TOO, for a run planned before the abandon
	// began and taking the published shortcut past Execute.
	if err := ProveRestoredLedger(t.Context(), p); err == nil ||
		!strings.Contains(err.Error(), "has already begun undoing it") {
		t.Errorf("ProveRestoredLedger after an abandon began = %v, want a refusal", err)
	}

	if err := Finish(t.Context(), p); err == nil || !strings.Contains(err.Error(), "has already begun undoing it") {
		t.Errorf("Finish after an abandon began = %v, want a refusal", err)
	}

	assertFenced(t, tgt.StateDir)

	if err := os.Remove(kept); err != nil {
		t.Fatalf("unblock the kept name: %v", err)
	}

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); err != nil {
		t.Fatalf("the second abandon: %v", err)
	}

	assertOpen(t, tgt.StateDir)
}

// A RECOVERY OVER A LEDGER SCHEMA HOLDING A TABLE WITH WHITESPACE IN ITS NAME IS
// REFUSED AT THE PLAN, before any quiesce or fence: the copy it would take could
// not be restored, so an abandon could never put it back.
func TestARecoveryOverATableItsCopyCouldNotNameIsRefusedAtThePlan(t *testing.T) {
	f := newDumpFixture(t, bound)

	spaced := f.liveLedger()
	spaced.Tables = append(slices.Clone(spaced.Tables), "operator notes")

	p, err := PlanRecover(t.Context(), f.archive, recoverTarget(t, f, f.ledgerFor(spaced)))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "whitespace in its name")) == 0 {
		t.Errorf("a recovery over a table with a space in its name was not refused: %+v", p.Refusals)
	}
}

// THE COPY A RECOVERY KEEPS IS HELD TO THE LEDGER IT REPLACES, read in the
// dump's own snapshot, before its digest is journaled: a copy of another
// database, schema or deployment would become the ledger an abandon puts back.
func TestARecoveryKeepsNoCopyOfAnotherLedger(t *testing.T) {
	for name, edit := range map[string]func(*state.PostgresLedger){
		"another database":   func(l *state.PostgresLedger) { l.Database = "elsewhere" },
		"another schema":     func(l *state.PostgresLedger) { l.Schema = "public" },
		"another deployment": func(l *state.PostgresLedger) { l.Binding = "ffffffffffffffffffffffffffffffff" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDumpFixture(t, bound)
			ledger := &dumpOfAnother{fakeLedger: f.ledgerFor(f.liveLedger()), edit: edit}
			tgt := recoverTarget(t, f, ledger)

			if _, err := executeOn(t, f.archive, tgt, ReplaceLedger); err == nil ||
				!strings.Contains(err.Error(), "will not keep it as the record of that ledger") {
				t.Fatalf("Execute = %v, want a refusal", err)
			}

			if prog, err := InProgress(tgt.StateDir); err != nil || prog.SupersedePublished ||
				len(prog.SupersededDigests) != 0 {
				t.Errorf("the copy was journaled or published (%+v, %v)", prog, err)
			}

			if slices.Contains(ledger.callsSnapshot(), "restore") {
				t.Error("the archive was restored over a ledger whose copy was refused")
			}
		})
	}
}

// dumpOfAnother is a database whose pg_dump reaches another ledger than its
// peek: the copy's snapshot says what it is of.
type dumpOfAnother struct {
	*fakeLedger
	edit func(*state.PostgresLedger)
}

func (d *dumpOfAnother) Dump(ctx context.Context, dest string) (LedgerDump, error) {
	out, err := d.fakeLedger.Dump(ctx, dest)

	other := state.PostgresLedger{Database: out.Database, Schema: out.Facts.Schema, Binding: out.Facts.Binding}
	d.edit(&other)
	out.Database, out.Facts.Schema, out.Facts.Binding = other.Database, other.Schema, other.Binding

	return out, err
}

// A SCHEMA HOLDING A TABLE HIERARCHY IS REFUSED AT THE PLAN, for a restore and a
// recovery alike: the cleanup and an abandon's put-back would both refuse it,
// and a recovery would be left fenced over a database billet never changed.
func TestASchemaHoldingAHierarchyIsRefusedAtThePlan(t *testing.T) {
	f := newDumpFixture(t, bound)

	pristine := state.PostgresLedger{
		Database: dumpDatabase, Schema: dumpSchema, Tables: []string{"admission", "reports", "schema_migrations"},
		Migrations: f.migrations, Hierarchy: []string{"reports"},
	}

	p, err := PlanRestore(t.Context(), f.archive, pgTarget(t, f.d, f.ledgerFor(pristine)))
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(refusalsMentioning(p, "partitioned, a partition or an inheriting")) == 0 {
		t.Errorf("a restore into a schema holding a hierarchy was planned: %+v", p.Refusals)
	}

	// A HIERARCHY THE ROLE CANNOT SEE: no table listed, the catalogue's
	// hierarchy still reported, and an install refused for it.
	hidden := emptySchema()
	hidden.Hierarchy = []string{"reports"}

	p, err = PlanRestore(t.Context(), f.archive, pgTarget(t, f.d, f.ledgerFor(hidden)))
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(refusalsMentioning(p, "partitioned, a partition or an inheriting")) == 0 {
		t.Errorf("an install into a schema hiding a hierarchy was planned: %+v", p.Refusals)
	}

	// KEEPING THE DATABASE'S OWN LEDGER WRITES NOTHING THERE, so it is not
	// refused for one.
	own := f.restoredAs()
	own.Hierarchy = []string{"reports"}

	p, err = PlanRestore(t.Context(), f.archive, pgTarget(t, f.d, f.ledgerFor(own)))
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(p.Refusals) > 0 || p.dispositionOf(EntryLedgerDump) != KeepLiveLedger {
		t.Errorf("keeping a ledger beside a hierarchy planned %q with refusals %+v",
			p.dispositionOf(EntryLedgerDump), p.Refusals)
	}

	live := f.liveLedger()
	live.Hierarchy = []string{"reports"}

	p, err = PlanRecover(t.Context(), f.archive, recoverTarget(t, f, f.ledgerFor(live)))
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(refusalsMentioning(p, "partitioned, a partition or an inheriting")) == 0 {
		t.Errorf("a recovery over a schema holding a hierarchy was planned: %+v", p.Refusals)
	}
}

// THE ABANDON RECORDS ITSELF BEFORE ITS FIRST ACTION. Held in the source, because
// no fake reaches a crash between the mark and the first removal: in
// abandonLocked the journal's Abandoning mark is saved before the first
// abandonOne and before the PostgreSQL put-back.
func TestAnAbandonIsJournaledBeforeItsFirstAction(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "restore.go", nil, 0)
	if err != nil {
		t.Fatalf("parse restore.go: %v", err)
	}

	var mark, save, firstRemove, putBack token.Pos

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "abandonLocked" {
			continue
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if sel, ok := n.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Abandoning" && mark == token.NoPos {
					mark = n.Pos()
				}
			case *ast.CallExpr:
				// THE SAVE THAT MAKES THE MARK DURABLE, the first after it.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "save" &&
					mark != token.NoPos && save == token.NoPos {
					save = n.Pos()
				}

				if id, ok := n.Fun.(*ast.Ident); ok {
					switch {
					case id.Name == "abandonOne" && firstRemove == token.NoPos:
						firstRemove = n.Pos()
					case id.Name == "putSupersededDumpBack" && putBack == token.NoPos:
						putBack = n.Pos()
					}
				}
			}

			return true
		})
	}

	switch {
	case mark == token.NoPos || save == token.NoPos || firstRemove == token.NoPos || putBack == token.NoPos:
		t.Fatalf("abandonLocked no longer has the mark (%v), its save (%v), abandonOne (%v) and the "+
			"put-back (%v) this test holds in order", mark, save, firstRemove, putBack)
	case save > firstRemove || save > putBack:
		t.Fatal("abandonLocked acts before the journal durably says an abandon has begun")
	}
}

// THE MARK IS SAVED BEFORE THE ABANDON ACTS, ON EVERY RUN: an abandon that cannot
// save its journal removes nothing and puts nothing back, even when the journal
// it loaded already carries the mark, because the run that first set it may
// have died before that save was durable.
func TestAnAbandonThatCannotSaveItsMarkDoesNothing(t *testing.T) {
	f, ledger, tgt := interruptedRecovery(t)

	path := JournalPath(tgt.StateDir)

	var raw map[string]any
	if err := json.Unmarshal(read(t, path), &raw); err != nil {
		t.Fatalf("decode the journal: %v", err)
	}

	raw["abandoning"] = true

	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode the journal: %v", err)
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the journal: %v", err)
	}

	// EVERY SAVE FAILS from here, the abandon's own first one included.
	prev := saveJournalHook
	saveJournalHook = func() error { return errStopHere }

	t.Cleanup(func() { saveJournalHook = prev })

	if _, err := Abandon(t.Context(), f.archive, tgt, ReplaceLedger); !errors.Is(err, errStopHere) {
		t.Fatalf("Abandon with a journal it cannot save = %v, want that failure", err)
	}

	if len(ledger.restored) != 0 {
		t.Errorf("the abandon put %v back without saving its mark", ledger.restored)
	}

	if _, err := os.Lstat(supersededDumpPath(f.archive, tgt.StateDir)); err != nil {
		t.Errorf("the copy moved although the mark was never saved (%v)", err)
	}

	assertFenced(t, tgt.StateDir)
}
