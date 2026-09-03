package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
)

// downConfig writes a service config whose state directory is somewhere a test
// can actually open a ledger, and returns both paths.
func downConfig(t *testing.T, withServer bool) (cfgPath, stateDir string) {
	t.Helper()

	dir := t.TempDir()
	stateDir = filepath.Join(dir, "server-state")
	cfgPath = filepath.Join(dir, "billet.yaml")

	var body strings.Builder

	if withServer {
		body.WriteString(`server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + filepath.Join(dir, "key.pem") + `
`)
	}

	{
		body.WriteString(`node:
  name: probe-node
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + filepath.Join(dir, "node-state") + `
  lock_dir: ` + filepath.Join(dir, "locks") + `
`)
	}

	body.WriteString(`tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`)

	if err := os.WriteFile(cfgPath, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return cfgPath, stateDir
}

// stageDown installs the fakes `down` reaches for. running says which units the
// host reports as active, and every one of them is reported as THIS build —
// the identity refusal has its own tests.
func stageDown(t *testing.T, f *fakeConverger, running ...string) *fakeConverger {
	t.Helper()

	realConverge, realInspect, realLock := converge, inspect, lifecycleLock
	t.Cleanup(func() { converge, inspect, lifecycleLock = realConverge, realInspect, realLock })

	converge = func(...lifeops.ConvergeOption) converger { return f }
	lifecycleLock = func() (*hostLock, error) {
		f.record("lock")

		return &hostLock{}, nil
	}

	facts := func(name string) lifeops.ServiceFacts {
		s := lifeops.ServiceFacts{Name: name, ActiveState: "inactive"}

		for _, r := range running {
			if r == name {
				s.ActiveState = "active"
				s.RunningIsThisBuild = lifeops.Yes
			}
		}

		return s
	}

	inspect = func(context.Context, string, string) (lifeops.Report, error) {
		return lifeops.Report{
			Server: facts(deploy.ServerUnitName),
			Node:   facts(deploy.NodeUnitName),
		}, nil
	}

	return f
}

func openLedger(t *testing.T, dir string) *state.DB {
	t.Helper()

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// THE WHOLE ORDER ON AN IDLE HOST. Each step is here because doing it in a
// different order is a defect: seal before waiting or the wait is for a target
// that keeps moving, wait before stopping or a running job is killed, node
// before server or the node reports its completions to nothing, and stop before
// disable so a failure to stop does not leave a host that is also not coming
// back at boot.
func TestDownSealsWaitsThenStopsTheNodeBeforeTheServer(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath}); err != nil {
			t.Errorf("an idle host was refused: %v", err)
		}
	})

	// THE LOCK IS IN THE TRACE, because every command test stubs it — without
	// this, deleting the production call from both `up` and `down` leaves the
	// whole suite green and two lifecycle commands can interleave.
	want := []string{
		"lock",
		"stop " + deploy.NodeUnitName,
		"stop " + deploy.ServerUnitName,
		"disable " + deploy.NodeUnitName,
		"disable " + deploy.ServerUnitName,
	}
	if strings.Join(f.trace, " → ") != strings.Join(want, " → ") {
		t.Errorf("down did:\n  %s\nwant:\n  %s",
			strings.Join(f.trace, " → "), strings.Join(want, " → "))
	}

	// AND THE SEAL IS A local-down ONE, which is what makes `billet local up`
	// able to clear it. An operator seal here would need a human to reopen the
	// deployment after an ordinary restart.
	got := admissionNow(t, db)

	if got.Mode != state.AdmissionSealed {
		t.Errorf("admission is %s after a down, want sealed", got.Mode)
	}
	if got.Provenance != state.ProvenanceLocalDown {
		t.Errorf("the seal's provenance is %q, want %q; `billet local up` clears only its "+
			"own", got.Provenance, state.ProvenanceLocalDown)
	}

	if !strings.Contains(out, "billet local up") {
		t.Errorf("the report does not say how to bring the host back:\n%s", out)
	}
}

// NOTHING IS STOPPED WHILE WORK IS RUNNING.
//
// This is the property the whole command exists for. A node stopped mid-job
// fails somebody's build — GitHub does not requeue a job whose runner vanished —
// so a `down` that cannot reach quiet must stop NOTHING rather than proceed.
func TestDownStopsNothingWhileAJobIsStillRunning(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)
	cfg := loadFixtureConfig(t, cfgPath)

	outstandingLease(t, db, cfg)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	err := runLocalDown(t.Context(), downOptions{configPath: cfgPath, timeout: time.Second})
	if err == nil {
		t.Fatal("a host with a job still running was taken down")
	}

	// AND THEREFORE NOTHING ELSE HAPPENED. An error says the command gave up; it
	// does not say the services are untouched.
	for _, step := range f.trace {
		if strings.HasPrefix(step, "stop ") || strings.HasPrefix(step, "disable ") {
			t.Errorf("a job was still running and down did %q; that fails somebody's build",
				step)
		}
	}

	// The seal STAYS, because the operator's intent has not changed and work is
	// still finishing. Reopening on the way out would invite new jobs onto a host
	// somebody is trying to take down.
	if got := admissionNow(t, db); got.Mode != state.AdmissionSealed {
		t.Errorf("admission is %s after an abandoned down, want it still sealed", got.Mode)
	}
}

// A NODE-ONLY HOST SAYS WHAT IT CANNOT DO.
//
// There is no ledger here to seal, so nothing on this machine can stop the
// control plane assigning it more work. Stopping the node still drains what is
// already on it — that is the node's own SIGTERM behaviour — but an operator who
// reads this as "the host is fenced" is wrong, and the remedy is a different
// command against a different machine.
func TestDownOnANodeOnlyHostSaysItCannotSealAndNamesTheRemedy(t *testing.T) {
	asLinux(t)

	cfgPath, _ := downConfig(t, false)
	f := stageDown(t, &fakeConverger{}, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath}); err != nil {
			t.Errorf("a node-only host was refused: %v", err)
		}
	})

	if !strings.Contains(out, "no admission ledger to seal") {
		t.Errorf("the report does not say why nothing was sealed:\n%s", out)
	}
	if !strings.Contains(out, "billet drain") {
		t.Errorf("the report does not name the command that fences the deployment:\n%s", out)
	}

	// IT STILL STOPS THE NODE, and only the node.
	want := []string{"lock", "stop " + deploy.NodeUnitName, "disable " + deploy.NodeUnitName}
	if strings.Join(f.trace, " → ") != strings.Join(want, " → ") {
		t.Errorf("down did:\n  %s\nwant:\n  %s",
			strings.Join(f.trace, " → "), strings.Join(want, " → "))
	}
}

// A UNIT RUNNING SOMEBODY ELSE'S BUILD IS NOT STOPPED BY ACCIDENT.
func TestDownRefusesAUnitRunningADifferentBuild(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	// BOTH ACTIVE, and the node is a build billet cannot vouch for.
	//
	// Staged through the BACKEND rather than through a systemd inspection: the
	// facts this refusal rests on are answered differently by every service
	// manager, and getting them from an inspector meant the refusal reported on
	// systemd units on a host that has none.
	f := &fakeConverger{running: map[string]lifeops.RunningFacts{
		deploy.ServerUnitName: {Active: lifeops.Yes, IsThisBuild: lifeops.Yes},
		deploy.NodeUnitName: {
			Active: lifeops.Yes, IsThisBuild: lifeops.No,
			Why: "the running process is /opt/other/billet",
		},
	}}
	stageDown(t, f)

	err := runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	if err == nil {
		t.Fatal("a unit running a different build was stopped without being asked twice")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal does not name the way through: %v", err)
	}

	// NOTHING WAS SEALED EITHER, which matters more than the stop: the seal goes
	// into a ledger the OTHER build is using, on the assumption that this one
	// will clear it.
	if got := admissionNow(t, db); got.Mode != state.AdmissionOpen {
		t.Errorf("a refused down still sealed the deployment (%s)", got.Mode)
	}
	if len(f.trace) != 0 {
		t.Errorf("a refused down still did %v", f.trace)
	}
}

// AND --force IS THE WAY THROUGH, so the refusal above is a question rather than
// a wall.
func TestDownForceStopsAUnitRunningADifferentBuild(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	openLedger(t, stateDir)

	f := &fakeConverger{}
	stageDown(t, f)

	inspect = func(context.Context, string, string) (lifeops.Report, error) {
		return lifeops.Report{
			Server: lifeops.ServiceFacts{Name: deploy.ServerUnitName, ActiveState: "inactive"},
			Node: lifeops.ServiceFacts{
				Name: deploy.NodeUnitName, ActiveState: "active",
				RunningIsThisBuild: lifeops.No, RunningWhy: "somebody else's",
			},
		}, nil
	}

	capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath, force: true}); err != nil {
			t.Errorf("--force was refused: %v", err)
		}
	})

	// THE UNIT IT REFUSED IS THE ONE IT STOPS. Asserting only that the trace is
	// non-empty passes when the stop is deleted and the disables still run —
	// which is the state that leaves a service running and not coming back.
	if !slices.Contains(f.trace, "stop "+deploy.NodeUnitName) {
		t.Errorf("--force did not stop the unit it was forced past: %v", f.trace)
	}
}

// THE FENCE BETWEEN DRAINING AND STOPPING.
//
// The barrier's answer is about the instant it was taken. Between the wait
// returning and the first unit stopping, somebody can resume admission and a
// listener can accept a job — so what was proved idle is running a build by the
// time the stop lands. Re-reading the generation is what turns "it was quiet"
// into "it is still the same quiet".
func TestDownStopsNothingIfAdmissionMovedWhileItWasGettingReady(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)
	cfg := loadFixtureConfig(t, cfgPath)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	sealed, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceLocalDown, Actor: "billet local down",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Somebody moves admission after the drain established its generation.
	if _, err := db.Resume(t.Context(), state.ResumeRequest{
		Expect: sealed.Generation, Clears: state.ProvenanceLocalDown, Actor: "someone else",
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	req := lifeops.UpRequest{ConfigPath: cfgPath, WantServer: true, WantNode: true}

	// THE STAGED FAKE, NOT A FRESH ONE. This passed a new &fakeConverger{} for a
	// while, so `f.trace` below was empty whatever the command did and the
	// assertion that nothing was stopped could not fail. Adversarial review found
	// it; it is the shape billet-testing calls "proving the mechanism is not
	// proving it is USED", one argument over.
	err = stopAndDisable(t.Context(), f, cfg, req, sealed.Generation, false)
	if err == nil {
		t.Fatal("a host whose admission moved underneath the drain was stopped anyway")
	}
	if !strings.Contains(err.Error(), "admission moved") {
		t.Errorf("the diagnostic does not say what changed: %v", err)
	}

	if len(f.trace) != 0 {
		t.Errorf("nothing should have been stopped, but down did %v", f.trace)
	}
}

// --dry-run CHANGES NOTHING, including the seal.
func TestDownDryRunSealsNothingAndStopsNothing(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath, dryRun: true}); err != nil {
			t.Errorf("dry run: %v", err)
		}
	})

	if got := admissionNow(t, db); got.Mode != state.AdmissionOpen {
		t.Errorf("a dry run sealed the deployment (%s)", got.Mode)
	}
	if len(f.trace) != 0 {
		t.Errorf("a dry run did %v", f.trace)
	}
	if !strings.Contains(out, "Nothing was changed") {
		t.Errorf("a dry run did not say so:\n%s", out)
	}
}

// AN OPERATOR'S SEAL IS NOT REPLACED BY A SHUTDOWN'S.
//
// Overwriting it would mean the next `billet local up` cleared a seal somebody
// took deliberately — reopening, silently, a deployment they had quiesced for
// their own reasons.
func TestDownLeavesAnOperatorSealAlone(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops", Reason: "replacing a disk",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath}); err != nil {
			t.Errorf("down: %v", err)
		}
	})

	got := admissionNow(t, db)

	if got.Provenance != state.ProvenanceOperator || got.Reason != "replacing a disk" {
		t.Errorf("the operator's seal was replaced: %+v", got)
	}
	if !strings.Contains(out, "already held by an operator") {
		t.Errorf("the report does not say the seal was left alone:\n%s", out)
	}
}

// UP CLEARS A SHUTDOWN'S SEAL, which is what closes the loop: a host taken down
// and brought back takes work again without anybody having to know that a seal
// was involved.
func TestUpClearsAShutdownSeal(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceLocalDown, Actor: "billet local down",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	out := capture(t, func() {
		if err := runLocalUp(t.Context(), upOptions{
			configPath: cfgPath, servicePath: cfgPath,
		}); err != nil {
			t.Errorf("up: %v", err)
		}
	})

	if got := admissionNow(t, db); got.Mode != state.AdmissionOpen {
		t.Errorf("admission is %s after `up`, want open; the host is running and taking "+
			"nothing", got.Mode)
	}
	if !strings.Contains(out, "taking work again") {
		t.Errorf("up does not report that admission was reopened:\n%s", out)
	}
}

// AND IT WILL NOT CLEAR AN OPERATOR'S.
//
// This is the whole reason provenance exists. Somebody quiesced this deployment
// deliberately; reopening it because a service was restarted would admit work
// into their maintenance window, silently, and their evidence would be a job
// running during it.
func TestUpLeavesAnOperatorSealAloneAndSaysSo(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops", Reason: "replacing a disk",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	out := capture(t, func() {
		if err := runLocalUp(t.Context(), upOptions{
			configPath: cfgPath, servicePath: cfgPath,
		}); err != nil {
			t.Errorf("up: %v", err)
		}
	})

	got := admissionNow(t, db)

	if got.Mode != state.AdmissionSealed || got.Provenance != state.ProvenanceOperator {
		t.Errorf("`up` cleared an operator's seal: %+v", got)
	}

	// AND IT IS REPORTED, because a host that came up and takes no work looks
	// broken to whoever ran the command unless they are told why.
	if !strings.Contains(out, "billet resume") {
		t.Errorf("up does not say how to reopen the deployment it left sealed:\n%s", out)
	}
}

// A FAILURE PART-WAY THROUGH SAYS WHERE IT LEFT THIS HOST.
//
// `down` is several systemd jobs, not one, so it can stop the node and then
// fail to stop the server. The operator's next decision depends entirely on
// which half happened, and the bare error names only the command that failed.
//
// The dangerous state is STOPPED BUT STILL ENABLED: not running now, and back
// at the next boot. That is the one somebody who has just been told their host
// is down would least expect.
func TestAFailedStopSaysWhatIsStoppedAndWhatComesBackAtBoot(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	openLedger(t, stateDir)

	// ENABLED, which is the state a host you are taking down is actually in —
	// and the state the "comes back at boot" warning is about.
	f := &fakeConverger{
		stopErr: map[string]error{
			deploy.ServerUnitName: errors.New("job for billet-server.service failed"),
		},
		enabled: map[string]string{
			deploy.NodeUnitName:   "enabled",
			deploy.ServerUnitName: "enabled",
		},
	}
	stageDown(t, f, deploy.ServerUnitName, deploy.NodeUnitName)

	var err error

	out := capture(t, func() {
		err = runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	})

	if err == nil {
		t.Fatal("a down whose server stop failed reported success")
	}

	// THE NODE STOPPED, and the report has to say so — it is not running, and
	// nothing else on this host will tell the operator that.
	if !strings.Contains(out, "stopped: "+deploy.NodeUnitName) {
		t.Errorf("the report does not say what was already stopped:\n%s", out)
	}

	// AND THAT IT COMES BACK. Nothing was disabled, because the disable pass is
	// after the stop pass.
	if !strings.Contains(out, "STILL ENABLED") {
		t.Errorf("the report does not warn that a stopped unit restarts at boot:\n%s", out)
	}

	// AND WHAT ADMISSION ACTUALLY SAYS, read at the time of the failure rather
	// than assumed from what this command did earlier: a resume elsewhere can
	// have cleared the seal while these stops were running, and the seal is the
	// one fact an operator would rely on before walking away from a half-stopped
	// host.
	if !strings.Contains(out, "state    admission is sealed") {
		t.Errorf("the report does not say what admission holds:\n%s", out)
	}
}

// A UNIT THAT WILL NOT STOP DISABLES NOTHING.
//
// The two passes are separate for this reason: disabling on the way past would
// leave a host that is still running AND not coming back at the next boot, which
// is the worst of both. Whether the stop itself succeeded is the converger's
// judgement — TestStopAndProveRefusesAUnitThatCameBack covers that where it
// lives, rather than here against a fake that would have to reimplement it.
func TestDownDisablesNothingWhenAUnitWillNotStop(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	openLedger(t, stateDir)

	f := &fakeConverger{
		stopErr: map[string]error{
			deploy.NodeUnitName: errors.New("billet-node.service is active after being " +
				"stopped; something is starting it again"),
		},
	}
	stageDown(t, f, deploy.ServerUnitName, deploy.NodeUnitName)

	err := runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	if err == nil {
		t.Fatal("a down whose unit would not stop reported success")
	}

	for _, step := range f.trace {
		if strings.HasPrefix(step, "disable ") {
			t.Errorf("a down that could not stop a unit still did %q; that leaves a host "+
				"running now and gone after the next reboot", step)
		}
	}
}

// AND IT REPORTS ADMISSION AS IT FINDS IT, not as this command left it.
//
// A resume elsewhere can clear the seal while the stops are running. Saying
// "still sealed" from memory would tell an operator the half-stopped host they
// are walking away from is fenced when it is taking work.
func TestAFailedStopReportsAdmissionAsItFindsIt(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	f := &fakeConverger{
		stopErr: map[string]error{
			deploy.ServerUnitName: errors.New("job for billet-server.service failed"),
		},
	}

	// The seal is cleared underneath, between this command sealing and its stop
	// failing — which is exactly when the report is written.
	f.onStop = func(unit string) {
		if unit != deploy.NodeUnitName {
			return
		}

		current, err := db.Admission(t.Context())
		if err != nil {
			t.Errorf("read admission: %v", err)

			return
		}

		if _, err := db.Resume(t.Context(), state.ResumeRequest{
			Expect: current.Generation, Clears: state.ProvenanceLocalDown, Actor: "someone else",
		}); err != nil {
			t.Errorf("Resume: %v", err)
		}
	}

	stageDown(t, f, deploy.ServerUnitName, deploy.NodeUnitName)

	var err error

	out := capture(t, func() {
		err = runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	})

	if err == nil {
		t.Fatal("the stop failure was not reported")
	}

	if !strings.Contains(out, "admission is OPEN") {
		t.Errorf("the report claims a seal that is no longer there:\n%s", out)
	}
	if !strings.Contains(out, "taking work again") {
		t.Errorf("the report does not say what an open admission means for a half-down "+
			"host:\n%s", out)
	}
}

// AN ADMISSION ROW BILLET CANNOT READ IS NOT SEALED OVER.
//
// Unknown is the fail-closed value on purpose. Writing a local-down seal over it
// would turn "I could not tell what this says" into a seal the next
// `billet local up` CLEARS — opening a deployment on the strength of a row
// nothing understood.
func TestDownRefusesToSealOverAnUnreadableAdmissionRow(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`PRAGMA ignore_check_constraints = ON`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			`UPDATE admission SET mode = 'quiescing' WHERE id = 1`)

		return err
	}); err != nil {
		t.Skipf("this SQLite build enforces the check regardless: %v", err)
	}

	before := admissionNow(t, db)
	if before.Mode != state.AdmissionUnknown {
		t.Skipf("the fixture did not reach the case: mode is %v", before.Mode)
	}

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	err := runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	if err == nil {
		t.Fatal("a down sealed over an admission row billet cannot read")
	}
	if !strings.Contains(err.Error(), "does not recognise") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// AND NOTHING WAS WRITTEN OR STOPPED.
	if got := admissionNow(t, db); got.Provenance == state.ProvenanceLocalDown {
		t.Error("the unreadable row was rewritten as a clearable shutdown seal")
	}

	for _, step := range f.trace {
		if strings.HasPrefix(step, "stop ") || strings.HasPrefix(step, "disable ") {
			t.Errorf("a refused down still did %q", step)
		}
	}
}

// AN `up` THAT CANNOT REOPEN ADMISSION IS NOT A SUCCESS.
//
// The services really are running, so this is not "the host did not come up" —
// but exiting 0 with admission still sealed is the exact "up and taking nothing"
// state provenance exists to prevent, and a script that brings a host up and
// moves on would move on.
func TestUpExitsNonZeroWhenItCannotReopenAdmission(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceLocalDown, Actor: "billet local down",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The ledger becomes unopenable while `up` is running its services, the way a
	// disk problem or a permissions mistake would present. A regular file where
	// the state directory belongs is the simplest thing OpenAdmin genuinely
	// cannot work with — removing the directory only makes it create a new one.
	f := &fakeConverger{plan: bothUnits()}
	f.onStart = func(string) {
		if err := os.RemoveAll(stateDir); err != nil {
			t.Errorf("remove the state dir: %v", err)

			return
		}
		if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
			t.Errorf("write over the state dir: %v", err)
		}
	}

	stageUp(t, f, githubVerified)

	var err error

	out := capture(t, func() {
		err = runLocalUp(t.Context(), upOptions{configPath: cfgPath, servicePath: cfgPath})
	})

	if err == nil {
		t.Fatal("up reported success while this deployment was still sealed and taking " +
			"nothing")
	}
	if code := exitStatus(err); code != 2 {
		t.Errorf("an up that could not reopen admission exits %d; 1 says the host did not "+
			"come up, which is not what happened", code)
	}

	// AND IT SAYS WHICH HALF HAPPENED, because the services ARE running and the
	// operator's next command depends on knowing that.
	if !strings.Contains(out, "services are up") {
		t.Errorf("the report does not say the services are running:\n%s", out)
	}
	if !strings.Contains(out, "billet resume") {
		t.Errorf("the report does not name the remedy:\n%s", out)
	}
}

// A STOP THAT DID NOT PROVE THE PROCESS GONE STOPS THE WHOLE COMMAND, even when
// the backend returned no error.
//
// This is the sequence that costs somebody their build. `down` stops the NODE
// first and the server second, because the node's SIGTERM is a drain while the
// server's teardown releases every lease its listener holds — including the
// running ones. So a node that is still draining and a `down` that believes it
// stopped is a server teardown on top of a live job.
//
// The systemd backend happens to return an error alongside every verdict that
// is not Yes, so acting on the error alone was indistinguishable from acting on
// the verdict — right up until a second backend exists. Relying on that is
// relying on a convention every future backend has to remember, rather than on
// the answer the type is there to carry.
func TestDownRefusesToGoOnWhenAStopIsNotProved(t *testing.T) {
	asLinux(t)

	for name, left := range map[string]lifeops.StopResult{
		"still running": {Gone: lifeops.No, How: "is deactivating"},
		"could not tell": {Gone: lifeops.Unknown,
			How: "was still stopping, with pid 4242 alive"},
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath, stateDir := downConfig(t, true)
			db := openLedger(t, stateDir)

			// The NODE reports it, which is the dangerous half: the server is
			// what this command would stop next.
			f := &fakeConverger{stopLeaves: map[string]lifeops.StopResult{
				deploy.NodeUnitName: left,
			}}
			stageDown(t, f, deploy.ServerUnitName, deploy.NodeUnitName)

			err := runLocalDown(t.Context(), downOptions{configPath: cfgPath})
			if err == nil {
				t.Fatal("a host whose node was not proved stopped was reported down")
			}

			if !strings.Contains(err.Error(), "not proved gone") {
				t.Errorf("the diagnostic does not say what was wrong: %v", err)
			}

			// AND THE MANAGER'S OWN ACCOUNT REACHES THE OPERATOR, because it is
			// the only thing that says WHICH of the two this was.
			if !strings.Contains(err.Error(), left.How) {
				t.Errorf("the manager's account of the stop was dropped: %v", err)
			}

			joined := strings.Join(f.trace, " → ")

			// THE SERVER WAS NEVER STOPPED. This is the assertion the whole test
			// exists for.
			if strings.Contains(joined, "stop "+deploy.ServerUnitName) {
				t.Errorf("the server was stopped after an unproved node stop, which tears down "+
					"every lease its listener holds: %s", joined)
			}

			// AND NOTHING WAS DISABLED, so a host left half-stopped still comes
			// back at the next boot rather than staying down silently.
			if strings.Contains(joined, "disable ") {
				t.Errorf("services were disabled after an unproved stop: %s", joined)
			}

			// The seal stays, which is correct and is what the report tells the
			// operator: nothing new is being admitted while this host is stuck.
			if got := admissionNow(t, db); got.Mode != state.AdmissionSealed {
				t.Errorf("admission is %s; a half-stopped host must stay sealed", got.Mode)
			}
		})
	}
}

// A SERVICE BILLET CANNOT TELL IS RUNNING IS REFUSED, not waved through.
//
// This was a bool, and "could not tell whether it is running" therefore became
// "not running" — which SKIPPED the identity refusal entirely, on the one host
// where it matters most. The refusal exists for the case where billet cannot
// vouch for the process it is about to stop, and an unanswered query is exactly
// that case rather than an exemption from it.
//
// The waved-through half is tested beside it, because a refusal that fires on
// everything is as useless as one that fires on nothing: a service PROVED
// inactive has no process whose identity could differ, and refusing on one would
// block a `down` on the half-stopped host that is when somebody reaches for this
// command.
func TestDownRefusesAServiceItCannotTellIsRunning(t *testing.T) {
	asLinux(t)

	for name, tc := range map[string]struct {
		node   lifeops.RunningFacts
		refuse bool
	}{
		"cannot tell whether it runs, or what": {
			node:   lifeops.RunningFacts{Active: lifeops.Unknown, IsThisBuild: lifeops.Unknown},
			refuse: true,
		},
		"cannot tell whether it runs, but it is this build": {
			node:   lifeops.RunningFacts{Active: lifeops.Unknown, IsThisBuild: lifeops.Yes},
			refuse: false,
		},
		"proved not running": {
			node:   lifeops.RunningFacts{Active: lifeops.No, IsThisBuild: lifeops.Unknown},
			refuse: false,
		},
		"running, and not this build": {
			node:   lifeops.RunningFacts{Active: lifeops.Yes, IsThisBuild: lifeops.No},
			refuse: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath, _ := downConfig(t, true)

			f := &fakeConverger{running: map[string]lifeops.RunningFacts{
				deploy.ServerUnitName: {Active: lifeops.Yes, IsThisBuild: lifeops.Yes},
				deploy.NodeUnitName:   tc.node,
			}}
			stageDown(t, f, deploy.ServerUnitName, deploy.NodeUnitName)

			err := runLocalDown(t.Context(), downOptions{configPath: cfgPath})

			switch {
			case tc.refuse && err == nil:
				t.Fatal("a service billet could not vouch for was stopped without being asked twice")

			case tc.refuse:
				if !strings.Contains(err.Error(), "--force") {
					t.Errorf("the refusal does not name the way through: %v", err)
				}

				// NOTHING WAS SEALED, which matters more than the stop: the seal
				// goes into a ledger the other build is using.
				if len(f.trace) != 0 {
					t.Errorf("a refused down still did %v", f.trace)
				}

			case err != nil:
				t.Fatalf("a host billet could vouch for was refused: %v", err)
			}
		})
	}
}

// STOPPING ONE UNIT MUST NOT TAKE THE OTHER WITH IT.
//
// A stop is a systemd TRANSACTION — `Conflicts=`, `PartOf=`, `BindsTo=` all
// reach other units, and the closure is systemd's to compute through units
// billet has never heard of. `up` settled this by observing rather than
// modelling, after four rounds of trying to predict it; `down` had the gap.
//
// It matters most on a SINGLE-ROLE host, which is the case that looks safest: a
// node-only `down` that reaches the server through a dependency takes the
// control plane down for the whole fleet, and the report would otherwise name
// only the node.
func TestDownRefusesWhenStoppingOneUnitStopsTheOther(t *testing.T) {
	asLinux(t)

	cfgPath, _ := downConfig(t, false)

	f := &fakeConverger{
		alsoStops: map[string]string{deploy.NodeUnitName: deploy.ServerUnitName},
	}
	stageDown(t, f, deploy.NodeUnitName)

	var err error

	out := capture(t, func() {
		err = runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	})

	if err == nil {
		t.Fatal("stopping the node took the server with it and the command reported success")
	}
	if !strings.Contains(err.Error(), "disturbed") {
		t.Errorf("the diagnostic does not name the collateral change: %v", err)
	}

	// AND IT STOPPED THERE. Continuing to disable after taking down a control
	// plane nobody asked about compounds it.
	for _, step := range f.trace {
		if strings.HasPrefix(step, "disable ") {
			t.Errorf("the run continued to %q after disturbing another unit", step)
		}
	}

	// AND THE REPORT SAYS WHERE THAT LEFT THE HOST.
	if !strings.Contains(out, "stopped: "+deploy.NodeUnitName) {
		t.Errorf("the report does not say what was stopped:\n%s", out)
	}
}

// DISABLING ONE UNIT MUST NOT DISABLE ANOTHER THIS RUN WAS NOT DISABLING.
//
// `systemctl disable` follows `[Install] Also=` — measured, and recorded in the
// billet-lifecycle skill. On a node-only host that means taking the node down
// can quietly stop the SERVER from starting at the next boot, and nothing about
// the node reports it.
func TestDownRefusesWhenDisablingOneUnitDisablesTheOther(t *testing.T) {
	asLinux(t)

	cfgPath, _ := downConfig(t, false)

	f := &fakeConverger{
		alsoEnables: map[string]string{deploy.NodeUnitName: deploy.ServerUnitName},
		enabled: map[string]string{
			deploy.NodeUnitName:   "enabled",
			deploy.ServerUnitName: "enabled",
		},
	}
	stageDown(t, f, deploy.NodeUnitName)

	err := runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	if err == nil {
		t.Fatal("disabling the node also disabled the server and the command reported success")
	}
	if !strings.Contains(err.Error(), "Also=") {
		t.Errorf("the diagnostic does not name the mechanism: %v", err)
	}
	if !strings.Contains(err.Error(), "systemctl enable") {
		t.Errorf("the diagnostic does not say how to restore it: %v", err)
	}
}

// AND A TWO-ROLE DOWN IS NOT REFUSED FOR DISABLING BOTH, which is the plan
// rather than collateral — without this the check above would refuse every
// ordinary host that carries an Also=.
func TestDownIsNotRefusedForDisablingUnitsItPlannedToDisable(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	openLedger(t, stateDir)

	f := &fakeConverger{
		alsoEnables: map[string]string{deploy.NodeUnitName: deploy.ServerUnitName},
		enabled: map[string]string{
			deploy.NodeUnitName:   "enabled",
			deploy.ServerUnitName: "enabled",
		},
	}
	stageDown(t, f, deploy.ServerUnitName, deploy.NodeUnitName)

	capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath}); err != nil {
			t.Errorf("a two-role down was refused for disabling both units: %v", err)
		}
	})
}

// A COMMAND THAT MUTATES AND THEN FAILS IS STILL OBSERVED.
//
// `systemctl` can move a transaction and then exit non-zero — an interrupted
// stop, a manager that stopped answering after doing the work. Returning on the
// error without looking is how `down` takes a control plane down through a
// dependency and then reports that nothing was stopped.
func TestAStopThatFailsAfterDisturbingTheOtherUnitStillReportsIt(t *testing.T) {
	asLinux(t)

	cfgPath, _ := downConfig(t, false)

	f := &fakeConverger{
		alsoStops: map[string]string{deploy.NodeUnitName: deploy.ServerUnitName},
		stopErr: map[string]error{
			deploy.NodeUnitName: errors.New("job for billet-node.service failed"),
		},
		enabled: map[string]string{deploy.NodeUnitName: "enabled"},
	}
	stageDown(t, f, deploy.NodeUnitName)

	var err error

	out := capture(t, func() {
		err = runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	})

	if err == nil {
		t.Fatal("a stop that failed was reported as success")
	}

	// THE UNIT IT TOOK WITH IT IS NAMED. On a node-only host that is the control
	// plane, and the report saying nothing about it is how somebody walks away
	// from a fleet that has lost its scheduler.
	if !strings.Contains(out, deploy.ServerUnitName) {
		t.Errorf("the report does not name the unit the failed stop disturbed:\n%s", out)
	}
}

// AND A DISABLE THAT REMOVES LINKS BEFORE FAILING IS REPORTED AS DISABLED.
//
// The state report exists to say what is true of the host, so deriving it from
// which commands returned zero gets it wrong in exactly the case somebody needs
// it: an interrupted disable that already removed the links.
func TestADisableThatFailsAfterRemovingLinksReportsWhatIsTrue(t *testing.T) {
	asLinux(t)

	cfgPath, _ := downConfig(t, false)

	f := &fakeConverger{
		disableEr: map[string]error{
			deploy.NodeUnitName: errors.New("interrupted"),
		},
		disablePartly: map[string]bool{deploy.NodeUnitName: true},
		enabled:       map[string]string{deploy.NodeUnitName: "enabled"},
	}
	stageDown(t, f, deploy.NodeUnitName)

	var err error

	out := capture(t, func() {
		err = runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	})

	if err == nil {
		t.Fatal("a disable that failed was reported as success")
	}

	// IT IS ACTUALLY DISABLED, so the report must not warn that it comes back at
	// the next boot.
	if strings.Contains(out, deploy.NodeUnitName+" is STILL ENABLED") {
		t.Errorf("the report claims a unit comes back at boot when its links were "+
			"already removed:\n%s", out)
	}
}

// THE "COMES BACK AT BOOT" WARNING IS READ, NOT INFERRED FROM WHAT THIS RUN DID.
//
// Deriving it from which commands succeeded is wrong in both directions, and the
// case that separates them is a unit that was ALREADY disabled before this run:
// no command here disabled it, so bookkeeping says nothing was done to it, while
// the host says it does not start at boot. Warning about that one sends an
// operator to fix something that is not broken — and, worse, teaches them the
// warning is noise, in a report whose whole purpose is the one line that matters.
func TestTheComesBackAtBootWarningNamesOnlyUnitsThatActuallyDo(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	openLedger(t, stateDir)

	f := &fakeConverger{
		stopErr: map[string]error{
			deploy.NodeUnitName: errors.New("job for billet-node.service failed"),
		},
		// The server was disabled by somebody earlier; the node is enabled. The
		// run fails on the first stop, so its disable pass never runs and NEITHER
		// unit is in this run's bookkeeping.
		enabled: map[string]string{
			deploy.NodeUnitName:   "enabled",
			deploy.ServerUnitName: "disabled",
		},
	}
	stageDown(t, f, deploy.ServerUnitName, deploy.NodeUnitName)

	var err error

	out := capture(t, func() {
		err = runLocalDown(t.Context(), downOptions{configPath: cfgPath})
	})

	if err == nil {
		t.Fatal("the failed stop was reported as success")
	}

	if !strings.Contains(out, deploy.NodeUnitName+" is STILL ENABLED") {
		t.Errorf("the report does not warn about the unit that really does come back:\n%s", out)
	}
	if strings.Contains(out, deploy.ServerUnitName+" is STILL ENABLED") {
		t.Errorf("the report warns that an already-disabled unit comes back at boot; that "+
			"is the line an operator is supposed to act on:\n%s", out)
	}
}
