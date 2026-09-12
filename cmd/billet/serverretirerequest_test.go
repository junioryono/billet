package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// requestFixture is a retiring controller as the request meets it: a
// PostgreSQL active-passive configuration, an identity and an authority, a
// bound ledger, this converge's guard, a fake systemd whose backup service is
// not installed, a mountinfo the rename's proof reads, and the addresses the
// address rule compares against.
type requestFixture struct {
	*retireFixture
	unitsDir string
	caPEM    string
	// now is this host's clock: the reservation is made at it, the round
	// starts after it, and the request runs later still, as a converge's
	// own order puts them.
	now time.Time
}

const (
	requestRun       = "ci-1"
	requestRetiring  = "control-a"
	requestSurvivor  = "control-b"
	requestStarted   = "2026-09-11T10:00:05Z"
	requestCollected = "2026-09-11T10:00:10Z"
)

func newRequestFixture(t *testing.T) *requestFixture {
	t.Helper()

	dsn := requireBackupPostgres(t)
	f := &requestFixture{retireFixture: newRetireFixture(t)}

	// The identity directory is this host's own, on PostgreSQL and
	// active-passive, with an authority the survivor must share.
	identityDir := t.TempDir()
	f.cfg = writeRetirePostgresConfig(t, identityDir)
	f.stateDir = identityDir
	t.Setenv("BILLET_STATE_DSN", dsn)

	id, err := state.DeploymentID(identityDir)
	mustOK(t, err)

	f.identity = id

	ca, err := wirecert.LoadOrCreateCA(identityDir, id)
	mustOK(t, err)

	f.caPEM = string(ca.CertPEM())

	// The ledger exists and is BOUND: a row is associated with this host
	// through the binding.
	db, err := state.OpenPostgres(t.Context(), identityDir, dsn)
	mustOK(t, err)

	_, err = db.ClaimController(t.Context(), "billet-control-01", id)
	mustOK(t, err)
	mustOK(t, db.Close())

	// A fake systemd: the backup service is not installed, which is a
	// positive answer and not an unread one.
	f.unitsDir = filepath.Join(t.TempDir(), "units")
	mustOK(t, os.MkdirAll(f.unitsDir, 0o755))
	writeFile(t, filepath.Join(f.unitsDir, backupServiceUnit),
		"LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nKillMode=control-group\nMainPID=0\n"+
			"InvocationID=\nStateChangeTimestamp=\n", 0o644)

	bin := filepath.Join(t.TempDir(), "systemctl")
	writeFile(t, bin, "#!/bin/sh\nunit=\"\"\nnames=\"\"\nfor a in \"$@\"; do case \"$a\" in --property=*) "+
		"names=\"$names ${a#--property=}\";; --|show) ;; *) unit=$a;; esac; done\n"+
		"for n in $names; do grep \"^$n=\" \"$BILLET_FAKE_UNITS/$unit\" || true; done\nexit 0\n", 0o755)
	t.Setenv("BILLET_FAKE_UNITS", f.unitsDir)

	savedSystemctl := systemctlBinary
	systemctlBinary = bin

	t.Cleanup(func() { systemctlBinary = savedSystemctl })

	// ONE MOUNT holds everything, so the rename is one rename on one mount.
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	writeFile(t, mountinfo, "1 0 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n", 0o644)

	savedMountinfo := mountinfoPath
	mountinfoPath = mountinfo

	t.Cleanup(func() { mountinfoPath = savedMountinfo })

	pinHostAddresses(t)
	mustHold(t, requestRun)

	// THE CLOCK IS THIS HOST'S: the reservation is made at it, and the
	// request runs after the round the role collected.
	f.now = time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	retireNow = func() time.Time { return f.now }

	return f
}

// reserve writes this host's reservation the way `--reserve` does and answers
// the row.
func (f *requestFixture) reserve(t *testing.T) state.Retirement {
	t.Helper()

	out, code := f.run(t, "", "--reserve", "--run", requestRun, "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor)

	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeReserved || code != 0 {
		t.Fatalf("the reservation: %s", out)
	}

	var row state.Retirement

	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present {
			t.Fatal("the reservation wrote no row")
		}

		row = r
	})

	// The round the role then collects starts after the reservation, and the
	// request runs after the round.
	f.now = time.Date(2026, 9, 11, 10, 0, 30, 0, time.UTC)

	return row
}

// pgLedger runs fn over this fixture's PostgreSQL ledger.
func (f *requestFixture) pgLedger(t *testing.T, fn func(db *state.DB)) {
	t.Helper()

	db, err := state.OpenPostgres(t.Context(), f.stateDir, os.Getenv("BILLET_STATE_DSN"))
	mustOK(t, err)

	fn(db)
	mustOK(t, db.Close())
}

// fixtureDoc reads a committed producer fixture, so every member the request
// reads is spelled as the command that writes it spells it.
func fixtureDoc(t *testing.T, command, name string) map[string]any {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(fixturesRoot, command, name+".json"))
	mustOK(t, err)

	var doc map[string]any
	mustOK(t, json.Unmarshal(body, &doc))

	return doc
}

// setPath sets a member of a decoded document by path, creating nothing: a
// path the producer does not write is a test's own mistake.
func setPath(t *testing.T, doc map[string]any, value any, path ...string) {
	t.Helper()

	cur := doc

	for i, key := range path {
		if i == len(path)-1 {
			if _, ok := cur[key]; !ok {
				t.Fatalf("the fixture carries no %s", strings.Join(path, "."))
			}

			cur[key] = value

			return
		}

		next, ok := cur[key].(map[string]any)
		if !ok {
			t.Fatalf("the fixture carries no %s", strings.Join(path, "."))
		}

		cur = next
	}
}

// survivorReport is the survivor's envelope: the committed controller report
// with this deployment's identity, an active-passive pair and the authority
// this host holds.
func (f *requestFixture) survivorReport(t *testing.T) map[string]any {
	t.Helper()

	inspect := fixtureDoc(t, "release-inspect", "postgres-controller-guarded")
	setPath(t, inspect, f.identity, "host", "deployment_id")
	setPath(t, inspect, "active-passive", "installed_config", "controllers")
	setPath(t, inspect, f.caPEM, "host", "authority", "current", "pem")
	setPath(t, inspect, []any{}, "host", "addresses")

	status := fixtureDoc(t, "rollout-status", "no-rollout")
	setPath(t, status, f.identity, "deployment", "id")
	setPath(t, status, []any{}, "registrations")

	return map[string]any{"host": requestSurvivor, "collected_at": requestCollected,
		"inspect": inspect, "status": status}
}

// selfReport is this host's own envelope: a server-only controller.
func (f *requestFixture) selfReport(t *testing.T) map[string]any {
	t.Helper()

	inspect := fixtureDoc(t, "release-inspect", "postgres-controller-guarded")
	setPath(t, inspect, f.identity, "host", "deployment_id")
	setPath(t, inspect, "active-passive", "installed_config", "controllers")

	return map[string]any{"host": requestRetiring, "collected_at": requestCollected,
		"inspect": inspect, "status": nil}
}

// input is the request's document, with overrides applied to its top level.
func (f *requestFixture) input(t *testing.T, overrides map[string]any) string {
	t.Helper()

	doc := map[string]any{
		"schema": retireInputSchema,
		"round":  map[string]any{"id": requestRun + "-" + requestStarted, "started_at": requestStarted},
		"self":   f.selfReport(t), "survivor": f.survivorReport(t),
		"nodes": map[string]any{}, "desired": nil, "desired_nodes": map[string]any{},
	}

	for k, v := range overrides {
		if v == nil {
			delete(doc, k)
		} else {
			doc[k] = v
		}
	}

	return string(mustMarshal(t, doc))
}

// request runs `--input -` with this fixture's operands.
func (f *requestFixture) request(t *testing.T, stdin string, extra ...string) (string, int) {
	t.Helper()

	args := []string{"--input", "-", "--run", requestRun, "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor, "--server-only", "--installed-sha256", f.installedSHA(t)}

	return f.run(t, stdin, append(args, extra...)...)
}

// installedSHA is the digest of the installed configuration, as the role
// reads it before the request.
func (f *requestFixture) installedSHA(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile(f.cfg)
	mustOK(t, err)

	return retirement.Digest(body)
}

// A SERVER-ONLY REQUEST RECORDS ITS INTENT AND NOTHING ELSE: the guard's
// marker, the journal at intent with the decision it judged, the row at
// intent, the status published, and an answer that says the phases after
// intent are not this binary's.
func TestServerRetireRequestRecordsItsIntent(t *testing.T) {
	f := newRequestFixture(t)
	row := f.reserve(t)

	// The dry run judges the same request and writes nothing.
	out, code := f.request(t, f.input(t, nil), "--dry-run")

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeReported || m["would"] != "request" ||
		m["variant"] != string(retirement.VariantServerOnly) {
		t.Fatalf("the dry run over a request: %s", out)
	}

	if _, presence, err := retirement.ReadJournal(); presence != retirement.JournalAbsent || err != nil {
		t.Fatalf("the dry run wrote a journal (%d %v)", presence, err)
	}

	if f.guard.record(t).Transition != nil {
		t.Fatal("the dry run marked the guard")
	}

	out, code = f.request(t, f.input(t, nil))

	m = retireAnswer(t, out)
	if m["outcome"] != retireOutcomeUnknown || m["reason"] != retireReasonPhase || code != exitUnknown ||
		m["state"] != "intent" {
		t.Fatalf("the request: %s", out)
	}

	// THE MARKER names the row's transition.
	rec := f.guard.record(t)
	if rec.Transition == nil || rec.Transition.ID != row.TransitionID || rec.Transition.Kind != transitionRetirement {
		t.Fatalf("the guard's marker: %+v", rec.Transition)
	}

	// THE JOURNAL carries the decision, and no stage on a server-only host.
	j, presence, err := retirement.ReadJournal()
	if err != nil || presence != retirement.JournalPresent {
		t.Fatalf("the journal: %d %v", presence, err)
	}

	switch {
	case j.Phase != retirement.PhaseIntent || j.Variant != retirement.VariantServerOnly:
		t.Fatalf("the journal's phase or variant: %+v", j)
	case j.Deployment != f.identity || j.Retiring != requestRetiring || j.Survivor.Host != requestSurvivor:
		t.Fatalf("the journal's hosts: %+v", j)
	case j.Provenance.TransitionID != row.TransitionID || j.Provenance.Reservation != row.ReservedAt:
		t.Fatalf("the journal's provenance: %+v", j.Provenance)
	case j.Ownership.Owner != requestRun:
		t.Fatalf("the journal's owner: %+v", j.Ownership)
	case j.Config != "absent" || j.StagedSHA256 != "":
		t.Fatalf("a server-only journal stages nothing: %+v", j)
	case j.InstalledSHA256 != f.installedSHA(t):
		t.Fatalf("the journal's installed digest: %+v", j)
	case j.Survivor.CASHA256 == "" || j.Survivor.Deployment != f.identity:
		t.Fatalf("the journal's survivor: %+v", j.Survivor)
	case j.Locator.DSNEnv != "BILLET_STATE_DSN" || j.Locator.Backend != "postgres":
		t.Fatalf("the journal's locator: %+v", j.Locator)
	}

	if _, presence, err := retirement.ReadStage(); presence != retirement.StageFileAbsent {
		t.Fatalf("a server-only request staged a configuration: %d %v", presence, err)
	}

	// THE ROW is at intent under this run, its id kept.
	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementIntent || r.TransitionID != row.TransitionID || r.Run != requestRun {
			t.Fatalf("the row: %+v (present %v)", r, present)
		}
	})

	// THE STATUS is published at intent.
	st, statusPresence, err := retirement.ReadStatus()
	if err != nil || statusPresence != retirement.StatusPresent || st.Phase != retirement.PhaseIntent {
		t.Fatalf("the status: %+v %d %v", st, statusPresence, err)
	}

	// AND THE GUARD IS RELEASED BY NOBODY from here on.
	if err := guardRun(t, "release", "--holder", requestRun); !errors.Is(err, errGuardTransition) {
		t.Fatalf("a marked guard was released: %v", err)
	}
}

// THE REQUEST IS HELD TO ITS RESERVATION AND ITS ROUND: no reservation, a
// round that predates it, a round too old, a self report from another host,
// and a survivor report the collector did not carry.
func TestServerRetireRequestIsHeldToItsReservationAndRound(t *testing.T) {
	f := newRequestFixture(t)

	out, code := f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["reason"] != retireReasonReservation || code != exitRefused {
		t.Fatalf("a request with no reservation: %s", out)
	}

	f.reserve(t)

	cases := map[string]struct {
		overrides map[string]any
		reason    string
	}{
		"a round that predates the reservation": {map[string]any{
			"round": map[string]any{"id": requestRun + "-2026-09-11T09:59:00Z", "started_at": "2026-09-11T09:59:00Z"},
		}, retireReasonReportStale},
		"a round another run minted": {map[string]any{
			"round": map[string]any{"id": "ci-9-" + requestStarted, "started_at": requestStarted},
		}, retireReasonInput},
		"no survivor report": {map[string]any{"survivor": nil}, retireReasonSurvivor},
	}

	for name, c := range cases {
		out, code := f.request(t, f.input(t, c.overrides))

		if m := retireAnswer(t, out); m["reason"] != c.reason || code != exitRefused {
			t.Errorf("%s: %s", name, out)
		}
	}

	// A self report from another host.
	self := f.selfReport(t)
	self["host"] = "control-x"

	out, code = f.request(t, f.input(t, map[string]any{"self": self}))
	if m := retireAnswer(t, out); m["reason"] != retireReasonReportHost || code != exitRefused {
		t.Fatalf("a self report from another host: %s", out)
	}

	// AND EVERY REFUSAL LEAVES THE RESERVATION as it was, since this
	// converge did not insert it.
	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementReserved {
			t.Fatalf("the row: %+v (present %v)", r, present)
		}
	})

	// WITH --reservation-fresh a refusal RELEASES the row this converge
	// inserted, so a failed request leaves no reservation behind.
	out, _ = f.request(t, f.input(t, map[string]any{"survivor": nil}), "--reservation-fresh")
	if m := retireAnswer(t, out); m["reservation"] != "released" {
		t.Fatalf("a fresh reservation must be released on a refusal: %s", out)
	}

	f.pgLedger(t, func(db *state.DB) {
		if _, present, err := db.ReadRetirement(t.Context(), f.identity); err != nil || present {
			t.Fatalf("the row must be gone: %v %v", present, err)
		}
	})
}

// THE SURVIVOR MUST BE ONE: another deployment's, a flagged one, one whose
// server is not running its executable, one mid-rotation, one carrying a
// retirement of its own, and one serving another authority.
func TestServerRetireRequestRequiresARunningSurvivor(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	cases := map[string]struct {
		// mutate carries the setter the case needs, bound to the test's own
		// t by the loop below, so no case body is a helper of its own.
		mutate func(doc map[string]any, set func(doc map[string]any, value any, path ...string))
		reason string
		extra  []string
	}{
		"another deployment's": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, strings.Repeat("e", 32), "host", "deployment_id")
		}, retireReasonSurvivorBinding, nil},
		"a single-controller deployment": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, "single", "installed_config", "controllers")
		}, retireReasonSurvivor, nil},
		"a server that is not active": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, "inactive", "services", "server", "active_state")
		}, retireReasonSurvivor, nil},
		"a server that is not its executable": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, false, "services", "server", "same_as_executable")
		}, retireReasonSurvivor, nil},
		"a rotation in progress": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, true, "host", "authority", "rotation_in_progress")
		}, retireReasonAuthority, nil},
		"a retirement of its own": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, map[string]any{"phase": "intent"}, "host", "retirement")
		}, retireReasonSurvivorRetiring, nil},
		"another authority": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, otherAuthorityPEM(t), "host", "authority", "current", "pem")
		}, retireReasonAuthority, nil},
		"a binding it could not observe": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, map[string]any{"unknown": "the identity could not be read"}, "host", "deployment_id")
		}, retireReasonSurvivorBinding, nil},
		// AN UNKNOWN AT AN INTERMEDIATE STEP of a path is could-not-tell for
		// everything under it: the rotation is read through host.authority,
		// which the survivor's own inspector could not observe.
		"an authority it could not observe": {func(doc map[string]any, set func(map[string]any, any, ...string)) {
			set(doc, map[string]any{"unknown": "the authority is changing"}, "host", "authority")
		}, retireReasonAuthority, nil},
	}

	set := func(doc map[string]any, value any, path ...string) { setPath(t, doc, value, path...) }

	for name, c := range cases {
		env := f.survivorReport(t)
		c.mutate(asMap(env["inspect"]), set)

		out, code := f.request(t, f.input(t, map[string]any{"survivor": env}), c.extra...)

		want := exitRefused
		if strings.HasSuffix(name, "it could not observe") {
			want = exitUnknown
		}

		if m := retireAnswer(t, out); m["reason"] != c.reason || code != want {
			t.Errorf("%s: %s", name, out)
		}
	}

	// The inventory's own flag is the cheaper refusal.
	out, code := f.request(t, f.input(t, nil), "--survivor-flagged")
	if m := retireAnswer(t, out); m["reason"] != retireReasonSurvivorFlagged || code != exitRefused {
		t.Fatalf("a flagged survivor: %s", out)
	}
}

// otherAuthorityPEM is a certificate of another deployment's authority.
func otherAuthorityPEM(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	ca, err := wirecert.LoadOrCreateCA(dir, strings.Repeat("f", 32))
	mustOK(t, err)

	return string(ca.CertPEM())
}

// THE HOST'S OWN PRECONDITIONS: the rename must be one rename on one mount,
// and a backup that is still running is awaited rather than raced.
func TestServerRetireRequestProvesTheRenameAndTheBackup(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// Two mounts: the identity's parent on one, the retirement directory on
	// another.
	writeFile(t, mountinfoPath, fmt.Sprintf("1 0 8:1 / / rw - ext4 /dev/sda1 rw\n2 1 8:2 / %s rw - ext4 /dev/sdb1 rw\n",
		mustEval(t, retirement.Root)), 0o644)

	out, code := f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["reason"] != retireReasonMount || code != exitRefused {
		t.Fatalf("two mounts: %s", out)
	}

	writeFile(t, mountinfoPath, "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n", 0o644)

	// A backup that is still running.
	writeFile(t, filepath.Join(f.unitsDir, backupServiceUnit),
		"LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nKillMode=control-group\nMainPID=4242\n"+
			"InvocationID=\nStateChangeTimestamp=\n", 0o644)

	out, code = f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["reason"] != retireReasonBackup || code != exitRefused {
		t.Fatalf("a running backup: %s", out)
	}

	if _, presence, err := retirement.ReadJournal(); presence != retirement.JournalAbsent || err != nil {
		t.Fatalf("a refused request wrote a journal (%d %v)", presence, err)
	}
}

// mustEval resolves a path's symlinks, as the preconditions do.
func mustEval(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	mustOK(t, err)

	return resolved
}
