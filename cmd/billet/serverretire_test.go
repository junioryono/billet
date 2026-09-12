package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

const (
	retireTestID       = "0123456789abcdef0123456789abcdef"
	retireTestIdentity = "dddddddddddddddddddddddddddddddd"
)

// retireFixture is one host: a guard fixture (the upgrade root and the managed
// binary), the retirement root, a SQLite ledger with its identity minted, and
// the seams pinned so every answer is the same on every machine.
type retireFixture struct {
	guard    *guardFixture
	stateDir string
	cfg      string
	identity string
}

func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	useRetirementRoot(t)

	f := &retireFixture{guard: newGuardFixture(t), stateDir: t.TempDir()}
	f.cfg = writeCAConfig(t, f.stateDir)

	id, err := state.DeploymentID(f.stateDir)
	mustOK(t, err)

	f.identity = id

	// Bound, as a pair's ledger is once a controller has claimed it: the
	// dry run associates a row with this host through the binding.
	statusPlane(t, f.stateDir, func(db *state.DB) {
		_, err := db.ClaimController(t.Context(), "billet-control-01", id)
		mustOK(t, err)
	})

	savedNow, savedID, savedStdin := retireNow, retireTransitionID, retireStdin
	retireNow = func() time.Time { return time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC) }
	retireTransitionID = func() (string, error) { return retireTestID, nil }

	t.Cleanup(func() { retireNow, retireTransitionID, retireStdin = savedNow, savedID, savedStdin })

	return f
}

// ledger runs fn over the fixture's ledger through the control plane's open.
func (f *retireFixture) ledger(t *testing.T, fn func(db *state.DB)) {
	t.Helper()
	statusPlane(t, f.stateDir, fn)
}

// run runs `billet server retire --json` with the fixture's config and answers
// the normalised output and the exit code (0 for success).
func (f *retireFixture) run(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()

	retireStdin = strings.NewReader(stdin)

	var runErr error

	out := capture(t, func() {
		runErr = cmdServer(t.Context(), nil, append([]string{"retire", "--json", "--config", f.cfg}, args...))
	})

	code := 0

	if runErr != nil {
		var exit *exitError
		if !errors.As(runErr, &exit) {
			t.Fatalf("the command failed outside its answer: %v\n%s", runErr, out)
		}

		code = exit.code
	}

	out = strings.ReplaceAll(out, f.identity, retireTestIdentity)
	out = strings.ReplaceAll(out, f.cfg, "/etc/billet/billet.yaml")
	out = strings.ReplaceAll(out, retirement.Root, "/var/lib/billet")
	out = strings.ReplaceAll(out, f.guard.root, "/var/lib/billet/upgrades")
	out = strings.ReplaceAll(out, f.stateDir, "/var/lib/billet/server")

	return out, code
}

// answer decodes an answer's top-level members.
func retireAnswer(t *testing.T, out string) map[string]any {
	t.Helper()

	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, out)
	}

	return m
}

// expectRetire asserts the outcome, the reason and the exit code of an answer
// and compares it with its fixture.
func expectRetire(t *testing.T, out string, code int, name, outcome, reason string) map[string]any {
	t.Helper()

	m := retireAnswer(t, out)

	wantCode := 0

	switch outcome {
	case retireOutcomeRefused:
		wantCode = exitRefused
	case retireOutcomeUnknown:
		wantCode = exitUnknown
	}

	if m["outcome"] != outcome || code != wantCode || (reason != "" && m["reason"] != reason) {
		t.Fatalf("%s: outcome %v reason %v exit %d, want %s %s %d\n%s", name, m["outcome"], m["reason"], code,
			outcome, reason, wantCode, out)
	}

	compareFixture(t, "server-retire", name, out)

	return m
}

// reserveRow writes a reservation for this host's run into the ledger the way
// `--reserve` would on a PostgreSQL pair, so the modes after it run on SQLite.
func (f *retireFixture) reserveRow(t *testing.T, run string) state.Retirement {
	t.Helper()

	var row state.Retirement

	f.ledger(t, func(db *state.DB) {
		var err error

		row, _, err = db.ReserveRetirement(t.Context(), state.RetirementReservation{
			Deployment: f.identity, Retiring: "control-a", Survivor: "control-b", Run: run,
			TransitionID: retireTestID, At: retireNow(),
		})
		mustOK(t, err)
	})

	return row
}

// journalAt plants a journal for this fixture's deployment and transition.
func (f *retireFixture) journalAt(t *testing.T, phase retirement.Phase, owner string) {
	t.Helper()

	j := retirement.Journal{
		Schema: retirement.JournalSchema, Phase: phase, Variant: retirement.VariantServerOnly,
		Deployment: f.identity, Retiring: "control-a",
		Survivor: retirement.JournalSurvivor{Host: "control-b", Deployment: f.identity, CASHA256: strings.Repeat("c", 64)},
		Backend:  "postgres", Controllers: "active-passive",
		IdentityDir: f.stateDir, Archive: filepath.Join(retirement.Root, "retired", "identity-2026-09-11T08:00:00Z"),
		InstalledSHA256: strings.Repeat("a", 64), Config: "absent",
		Provenance: retirement.Provenance{
			ReservingHolder: owner, TransitionID: retireTestID, Reservation: "2026-09-11T08:00:00Z",
			Deployment: f.identity, Retiring: "control-a", Survivor: "control-b",
		},
		Ownership: retirement.Ownership{Owner: owner},
	}

	if phase == retirement.PhaseDone {
		j.DoneAt = "2026-09-11T09:00:00Z"
		f.archiveIdentity(t, j.Archive)
	}

	mustOK(t, j.Write(time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC)))
}

// archiveIdentity puts the deployment identity where a done journal says the
// directory was archived, as the transition's rename does.
func (f *retireFixture) archiveIdentity(t *testing.T, archive string) {
	t.Helper()

	if _, err := os.Lstat(archive); err == nil {
		return
	}

	mustOK(t, retirement.EnsureRetiredDir())
	mustOK(t, os.Mkdir(archive, 0o700))

	body, err := os.ReadFile(state.DeploymentIDPath(f.stateDir))
	mustOK(t, err)
	mustOK(t, os.WriteFile(state.DeploymentIDPath(archive), body, 0o600))
}

// THE COMMITTED FIXTURES ARE EXACTLY WHAT THE PRODUCERS BELOW WRITE.
func TestTheServerRetireFixturesAreTheCommandsOwn(t *testing.T) {
	fixtureSetIs(t, "server-retire", []string{
		"refused-combination", "refused-platform", "refused-backend", "refused-controllers",
		"refused-guard-none", "refused-guard-holder", "refused-guard-preparing", "refused-guard-pointer",
		"reserved", "adopted", "refused-reserved",
		"refused-reservation", "abandoned", "abandoned-marker-cleared", "unknown-marker", "refused-abandon-run",
		"refused-abandon-journal",
		"completed", "completed-already", "refused-self", "refused-deployment", "refused-conflict", "refused-input",
		"acknowledged", "acknowledged-already", "refused-acknowledge-journal", "refused-acknowledge-mismatch",
		"dry-run-request", "dry-run-unknown-journal", "dry-run-adopt", "dry-run-unknown-ledger",
	})
}

// THE FLAG TABLE IS REFUSED BEFORE ANYTHING IS OPENED: exactly one mode, its
// operands and no others; the upgrade root is never created and the ledger
// never touched.
func TestServerRetireRefusesEachCombinationBeforeOpeningAnything(t *testing.T) {
	f := newRetireFixture(t)

	cases := map[string][]string{
		"two modes":                {"--reserve", "--dry-run", "--run", "ci-1", "--retiring-host", "control-a"},
		"no mode":                  {"--run", "ci-1", "--retiring-host", "control-a"},
		"no run":                   {"--reserve", "--retiring-host", "control-a", "--survivor-host", "control-b"},
		"no retiring host":         {"--reserve", "--run", "ci-1", "--survivor-host", "control-b"},
		"reserve without survivor": {"--reserve", "--run", "ci-1", "--retiring-host", "control-a"},
		"survivor is the retiring host": {"--reserve", "--run", "ci-1", "--retiring-host", "control-a",
			"--survivor-host", "control-a"},
		"survivor outside reserve":     {"--dry-run", "--retiring-host", "control-a", "--survivor-host", "control-b"},
		"complete-row without as-host": {"--complete-row", "--run", "ci-1", "--completion", "-"},
		"complete-row from a file": {"--complete-row", "--run", "ci-1", "--as-host", "control-b", "--completion",
			"/tmp/c.json"},
		"as-host outside complete-row": {"--abandon-reservation", "--run", "ci-1", "--retiring-host", "control-a",
			"--as-host", "control-b"},
		"acknowledge without answer": {"--acknowledge-row", "--run", "ci-1", "--retiring-host", "control-a"},
		"answer outside acknowledge": {"--dry-run", "--retiring-host", "control-a", "--answer", "-"},
		"a run with a slash":         {"--dry-run", "--run", "ci/1", "--retiring-host", "control-a"},
		"a bad installed digest": {"--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host",
			"control-b", "--installed-sha256", "nope"},
	}

	for name, args := range cases {
		out, code := f.run(t, "", args...)

		m := retireAnswer(t, out)
		if m["outcome"] != retireOutcomeRefused || m["reason"] != retireReasonCombination || code != exitRefused {
			t.Errorf("%s: %s (exit %d)", name, out, code)
		}
	}

	out, code := f.run(t, "", "--reserve", "--dry-run", "--run", "ci-1", "--retiring-host", "control-a")
	expectRetire(t, out, code, "refused-combination", retireOutcomeRefused, retireReasonCombination)

	if _, err := os.Lstat(f.guard.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a combination refusal touched the upgrade root: %v", err)
	}

	// Without --json there is no answer at all.
	err := cmdServer(t.Context(), nil, []string{"retire", "--dry-run", "--retiring-host", "control-a", "--config", f.cfg})
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("a run without --json must be refused naming it, got %v", err)
	}
}

// A RETIREMENT IS DEFINED ON LINUX: the platform is refused before the
// configuration is read.
func TestServerRetireIsRefusedOffLinux(t *testing.T) {
	f := newRetireFixture(t)

	saved := hostOS
	hostOS = "darwin"

	t.Cleanup(func() { hostOS = saved })

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a", "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	expectRetire(t, out, code, "refused-platform", retireOutcomeRefused, retireReasonPlatform)
}

// A RESERVATION NEEDS A POSTGRESQL ACTIVE-PASSIVE PAIR, judged from the
// configuration before any guard or ledger is examined.
func TestServerRetireReserveNeedsAPostgresActivePassivePair(t *testing.T) {
	f := newRetireFixture(t)
	mustHold(t, "ci-1")

	// THE GUARD COMES FIRST: the configuration is observed under it, so the
	// eligibility a reservation rests on is the one no concurrent converge or
	// transaction can have moved.
	out, code := f.run(t, "", "--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", "control-b")
	expectRetire(t, out, code, "refused-backend", retireOutcomeRefused, retireReasonBackend)

	// A PostgreSQL configuration for one controller: refused on the controllers
	// before the connection string is ever needed.
	f.cfg = writePostgresConfig(t, f.stateDir)
	t.Setenv("BILLET_STATE_DSN", "postgres://u:NEVERDIALLED@127.0.0.1:1/db")

	out, code = f.run(t, "", "--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", "control-b")
	expectRetire(t, out, code, "refused-controllers", retireOutcomeRefused, retireReasonControllers)

	if strings.Contains(out, "NEVERDIALLED") {
		t.Fatal("the connection string reached the answer")
	}
}

// EVERY MUTATING MODE RUNS UNDER THIS CONVERGE'S GUARD: no guard, another
// holder's, one still preparing, or one carrying a transaction pointer each
// refuse before any record is read.
func TestServerRetireNeedsThisConvergesGuard(t *testing.T) {
	f := newRetireFixture(t)
	abandon := []string{"--abandon-reservation", "--run", "ci-1", "--retiring-host", "control-a"}

	out, code := f.run(t, "", abandon...)
	expectRetire(t, out, code, "refused-guard-none", retireOutcomeRefused, retireReasonGuard)

	mustHold(t, "ci-other")

	out, code = f.run(t, "", abandon...)
	expectRetire(t, out, code, "refused-guard-holder", retireOutcomeRefused, retireReasonGuard)

	mustOK(t, guardRun(t, "release", "--holder", "ci-other"))

	// A guard the acquirer has not settled: its cleanup window is still open.
	managedScript(t, f.guard, "v0.10.0")
	mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

	out, code = f.run(t, "", abandon...)
	expectRetire(t, out, code, "refused-guard-preparing", retireOutcomeRefused, retireReasonGuard)

	mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
	mustHold(t, "ci-1")

	// A binary transaction's pointer under the guard.
	recovery := filepath.Join(f.guard.root, "recovery-20260911T100000-0badcafe")
	mustOK(t, os.Mkdir(recovery, 0o700))
	mustOK(t, os.Symlink(recovery, filepath.Join(f.guard.active(), guardPointerName)))

	out, code = f.run(t, "", abandon...)
	expectRetire(t, out, code, "refused-guard-pointer", retireOutcomeRefused, retireReasonGuard)
}

// THE RESERVATION ROW ON A REAL PAIR: reserved, adopted by the same host's
// later run, refused for another host while it stands. Everything after the
// row runs on SQLite below; this is the one mode that requires PostgreSQL.
func TestServerRetireReservesOnAPostgresPair(t *testing.T) {
	dsn := requireBackupPostgres(t)
	f := newRetireFixture(t)

	identityDir := t.TempDir()
	f.cfg = writeRetirePostgresConfig(t, identityDir)
	f.stateDir = identityDir
	t.Setenv("BILLET_STATE_DSN", dsn)

	id, err := state.DeploymentID(identityDir)
	mustOK(t, err)

	f.identity = id

	db, err := state.OpenPostgres(t.Context(), identityDir, dsn)
	mustOK(t, err)
	mustOK(t, db.Close())

	mustHold(t, "ci-1")

	out, code := f.run(t, "", "--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", "control-b")
	m := expectRetire(t, out, code, "reserved", retireOutcomeReserved, "")

	if m["transition_id"] != retireTestID || m["reserved_at"] == "" || m["deployment"] != retireTestIdentity {
		t.Fatalf("the reservation answer: %s", out)
	}

	// The same host reserving again under the same run adopts its own row.
	out, code = f.run(t, "", "--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", "control-b")
	expectRetire(t, out, code, "adopted", retireOutcomeAdopted, "")

	// Another host, another run, under its own guard: refused while the
	// reservation stands, naming it.
	mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
	mustHold(t, "ci-2")

	out, code = f.run(t, "", "--reserve", "--run", "ci-2", "--retiring-host", "control-b", "--survivor-host", "control-a")
	expectRetire(t, out, code, "refused-reserved", retireOutcomeRefused, retireReasonReserved)

	if !strings.Contains(out, "control-a") || !strings.Contains(out, "ci-1") {
		t.Fatalf("the refusal must name the reservation's host and run: %s", out)
	}

	// A digest of the installed configuration that moved refuses before the
	// guard is examined.
	mustOK(t, guardRun(t, "release", "--holder", "ci-2"))
	mustHold(t, "ci-1")

	out, code = f.run(t, "", "--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", "control-b",
		"--installed-sha256", strings.Repeat("0", 64))

	if m := retireAnswer(t, out); m["reason"] != retireReasonConfig || code != exitRefused {
		t.Fatalf("a moved configuration must refuse on config: %s", out)
	}
}

// writeRetirePostgresConfig is writePostgresConfig for an active-passive pair.
func writeRetirePostgresConfig(t *testing.T, identityDir string) string {
	t.Helper()

	path := writePostgresConfig(t, identityDir)

	body, err := os.ReadFile(path)
	mustOK(t, err)

	body = []byte(strings.Replace(string(body), "  identity_dir: ", "  controllers: active-passive\n  identity_dir: ", 1))
	mustOK(t, os.WriteFile(path, body, 0o600))

	return path
}

// AN ABANDONMENT RELEASES A RESERVATION NOTHING HAS STARTED, the marker first
// when the guard carries one, and refuses everything else: another run's row,
// a mismatched marker (kept), a marker beside no row (kept), a journal.
func TestServerRetireAbandonsOnlyAReservationNothingStarted(t *testing.T) {
	f := newRetireFixture(t)
	mustHold(t, "ci-1")
	abandon := []string{"--abandon-reservation", "--run", "ci-1", "--retiring-host", "control-a"}

	out, code := f.run(t, "", abandon...)
	expectRetire(t, out, code, "refused-reservation", retireOutcomeRefused, retireReasonReservation)

	// Another run's reservation.
	f.reserveRow(t, "ci-0")

	out, code = f.run(t, "", abandon...)
	expectRetire(t, out, code, "refused-abandon-run", retireOutcomeRefused, retireReasonReserved)

	f.ledger(t, func(db *state.DB) {
		mustOK(t, db.ReleaseRetirement(t.Context(), f.identity, "control-a", "ci-0"))
	})

	// This run's reservation, no marker.
	f.reserveRow(t, "ci-1")

	out, code = f.run(t, "", abandon...)
	m := expectRetire(t, out, code, "abandoned", retireOutcomeAbandoned, "")

	if m["marker"] != "absent" {
		t.Fatalf("no marker to clear: %s", out)
	}

	f.ledger(t, func(db *state.DB) {
		if _, present, err := db.ReadRetirement(t.Context(), f.identity); err != nil || present {
			t.Fatalf("the row must be gone: %v %v", present, err)
		}
	})

	// This run's reservation with a matching marker: the marker is cleared,
	// then the row deleted.
	f.reserveRow(t, "ci-1")
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)

	out, code = f.run(t, "", abandon...)
	m = expectRetire(t, out, code, "abandoned-marker-cleared", retireOutcomeAbandoned, "")

	if m["marker"] != "cleared" || f.guard.record(t).Transition != nil {
		t.Fatalf("the marker must be cleared: %s", out)
	}

	// A marker naming another transition beside this run's row: could-not-tell,
	// the marker and the row kept.
	f.reserveRow(t, "ci-1")
	other := "fedcba9876543210fedcba9876543210"
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: other}, nil)

	out, code = f.run(t, "", abandon...)
	expectRetire(t, out, code, "unknown-marker", retireOutcomeUnknown, retireReasonMarker)

	if rec := f.guard.record(t); rec.Transition == nil || rec.Transition.ID != other {
		t.Fatalf("a mismatched marker must be kept, got %+v", rec.Transition)
	}

	f.ledger(t, func(db *state.DB) {
		if _, present, err := db.ReadRetirement(t.Context(), f.identity); err != nil || !present {
			t.Fatalf("the row must be kept: %v %v", present, err)
		}

		mustOK(t, db.ReleaseRetirement(t.Context(), f.identity, "control-a", "ci-1"))
	})

	// A marker beside no row is a state no step produces: kept, could-not-tell.
	out, code = f.run(t, "", abandon...)

	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeUnknown || m["reason"] != retireReasonMarker || code != exitUnknown {
		t.Fatalf("a marker beside no row must be could-not-tell: %s", out)
	}

	markGuard(t, f.guard, nil, nil)

	// A journal means the retirement began; an abandonment is not the way on.
	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	out, code = f.run(t, "", abandon...)
	expectRetire(t, out, code, "refused-abandon-journal", retireOutcomeRefused, retireReasonJournal)
}

// THE SURVIVOR COMPLETES THE ROW FROM THE COMPLETION DOCUMENT, proving its
// host from --as-host and its identity from its own directory; a document for
// another deployment, a host that is not the survivor, or a row that is
// somebody else's each refuse with nothing written.
func TestServerRetireCompletesTheRowOnTheSurvivor(t *testing.T) {
	f := newRetireFixture(t)
	mustHold(t, "ci-b")

	row := f.reserveRow(t, "ci-a")

	completion := func(mutate func(*retirement.Completion)) string {
		c := retirement.Completion{
			Schema: retirement.CompletionSchema, Deployment: f.identity, Retiring: "control-a", Survivor: "control-b",
			TransitionID: retireTestID, Reservation: row.ReservedAt, DoneAt: "2026-09-11T09:00:00Z",
		}
		mutate(&c)

		return string(mustMarshal(t, c))
	}

	complete := []string{"--complete-row", "--run", "ci-b", "--as-host", "control-b", "--completion", "-"}

	out, code := f.run(t, "not json", complete...)
	expectRetire(t, out, code, "refused-input", retireOutcomeRefused, retireReasonInput)

	out, code = f.run(t, completion(func(c *retirement.Completion) { c.Survivor = "control-c" }), complete...)
	expectRetire(t, out, code, "refused-self", retireOutcomeRefused, retireReasonSelf)

	out, code = f.run(t, completion(func(c *retirement.Completion) { c.Survivor, c.Retiring = "control-b", "control-b" }),
		complete...)

	// A document naming one host as both is refused by the decoder before any
	// host comparison; the retiring host never runs the helper over a document
	// that holds, because --as-host must equal the survivor.
	if m := retireAnswer(t, out); m["reason"] != retireReasonInput || code != exitRefused {
		t.Fatalf("one host as both retiring and survivor: %s", out)
	}

	out, code = f.run(t, completion(func(c *retirement.Completion) { c.Deployment = strings.Repeat("e", 32) }), complete...)
	expectRetire(t, out, code, "refused-deployment", retireOutcomeRefused, retireReasonDeployment)

	// The row is another retirement's (a different retiring host).
	out, code = f.run(t, completion(func(c *retirement.Completion) { c.Retiring = "control-x" }), complete...)
	expectRetire(t, out, code, "refused-conflict", retireOutcomeRefused, retireReasonConflict)

	f.ledger(t, func(db *state.DB) {
		if r, _, err := db.ReadRetirement(t.Context(), f.identity); err != nil || r.State != state.RetirementReserved {
			t.Fatalf("the refusals wrote to the row: %+v %v", r, err)
		}
	})

	out, code = f.run(t, completion(func(*retirement.Completion) {}), complete...)
	m := retireAnswer(t, out)

	if code != 0 || m["row"] != retirement.RowDone || m["completed_by"] != "control-b" {
		t.Fatalf("the completion: %s", out)
	}

	compareFixture(t, "server-retire", "completed", out)

	out, code = f.run(t, completion(func(*retirement.Completion) {}), complete...)
	m = retireAnswer(t, out)

	if code != 0 || m["row"] != retirement.RowAlready {
		t.Fatalf("a second completion answers already: %s", out)
	}

	compareFixture(t, "server-retire", "completed-already", out)

	f.ledger(t, func(db *state.DB) {
		r, _, err := db.ReadRetirement(t.Context(), f.identity)
		if err != nil || r.State != state.RetirementDone || r.CompletedBy != "control-b" {
			t.Fatalf("the row after completion: %+v %v", r, err)
		}
	})
}

// THE RETIRING HOST ACKNOWLEDGES THE SURVIVOR'S ANSWER against its own done
// journal, field by field, and writes row_done and completed_by; a second
// acknowledgement answers already, and an answer for another retirement or a
// journal not at done refuses.
func TestServerRetireAcknowledgesTheRowInTheJournal(t *testing.T) {
	f := newRetireFixture(t)
	mustHold(t, "ci-a")

	acknowledge := []string{"--acknowledge-row", "--run", "ci-a", "--retiring-host", "control-a", "--answer", "-"}

	answer := func(mutate func(*retirement.Answer)) string {
		a := retirement.Answer{
			Schema: retirement.AnswerSchema, Deployment: f.identity, Retiring: "control-a", Survivor: "control-b",
			TransitionID: retireTestID, Reservation: "2026-09-11T08:00:00Z", Row: retirement.RowDone,
			CompletedBy: "control-b", CompletedAt: "2026-09-11T09:00:05Z",
		}
		mutate(&a)

		return string(mustMarshal(t, a))
	}

	out, code := f.run(t, answer(func(*retirement.Answer) {}), acknowledge...)
	expectRetire(t, out, code, "refused-acknowledge-journal", retireOutcomeRefused, retireReasonJournal)

	f.journalAt(t, retirement.PhaseStopped, "ci-a")

	out, code = f.run(t, answer(func(*retirement.Answer) {}), acknowledge...)

	if m := retireAnswer(t, out); m["reason"] != retireReasonJournal || code != exitRefused {
		t.Fatalf("a journal before done must refuse: %s", out)
	}

	f.journalAt(t, retirement.PhaseDone, "ci-a")

	out, code = f.run(t, answer(func(a *retirement.Answer) { a.TransitionID = strings.Repeat("9", 32) }), acknowledge...)
	expectRetire(t, out, code, "refused-acknowledge-mismatch", retireOutcomeRefused, retireReasonMismatch)

	if j, _, err := retirement.ReadJournal(); err != nil || j.RowDone {
		t.Fatalf("a refused acknowledgement wrote the journal: %+v %v", j, err)
	}

	out, code = f.run(t, answer(func(*retirement.Answer) {}), acknowledge...)
	expectRetire(t, out, code, "acknowledged", retireOutcomeAcknowledged, "")

	j, _, err := retirement.ReadJournal()
	if err != nil || !j.RowDone || j.CompletedBy != "control-b" || j.Settled {
		t.Fatalf("the journal after the acknowledgement: %+v %v", j, err)
	}

	out, code = f.run(t, answer(func(*retirement.Answer) {}), acknowledge...)
	expectRetire(t, out, code, "acknowledged-already", retireOutcomeAlready, "")
}

// THE DRY RUN CLASSIFIES AND WRITES NOTHING: every record as it stands, the
// row through the read-only open, and the dispatch the table decides.
func TestServerRetireDryRunReportsWithoutALock(t *testing.T) {
	f := newRetireFixture(t)

	// No guard, no journal, no row: a request is the way on, and no upgrade
	// root is created by looking.
	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	m := expectRetire(t, out, code, "dry-run-request", retireOutcomeReported, "")

	if m["dispatch"] != string(retirement.DispatchRequest) || m["guard"] != string(claimNone) {
		t.Fatalf("the dry run: %s", out)
	}

	if _, err := os.Lstat(f.guard.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dry run created the upgrade root: %v", err)
	}

	// This host's reservation under a marked guard: adopt.
	mustHold(t, "ci-1")
	f.reserveRow(t, "ci-1")
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)

	out, code = f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	m = expectRetire(t, out, code, "dry-run-adopt", retireOutcomeReported, "")

	if m["dispatch"] != string(retirement.DispatchAdopt) || m["row_fact"] != string(retirement.RowReservedMine) {
		t.Fatalf("the dry run over a reservation: %s", out)
	}

	// A journal beside no row is a state the table cannot dispatch.
	f.ledger(t, func(db *state.DB) {
		mustOK(t, db.ReleaseRetirement(t.Context(), f.identity, "control-a", "ci-1"))
	})
	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	out, code = f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	m = expectRetire(t, out, code, "dry-run-unknown-journal", retireOutcomeReported, "")

	if m["dispatch"] != string(retirement.DispatchUnknownReservation) {
		t.Fatalf("a journal beside no row: %s", out)
	}

	// A LEDGER THAT CANNOT BE READ IS `unreadable`, NEVER ABSENCE: the dispatch
	// is unknown-ledger, and the report still answers.
	mustOK(t, os.WriteFile(state.LedgerPath(f.stateDir), []byte("not a database"), 0o600))

	out, code = f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	m = expectRetire(t, out, code, "dry-run-unknown-ledger", retireOutcomeReported, "")

	if m["row_fact"] != string(retirement.RowUnreadable) || m["dispatch"] != string(retirement.DispatchUnknownLedger) {
		t.Fatalf("a ledger that cannot be read: %s", out)
	}
}
