package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/lifeops"
)

// fakeConverger records what `up` did to the host, in order.
//
// THE TRACE IS THE ASSERTION. Every safety property this command has is an
// ORDERING — ownership before and after the check, the check before any start,
// the server before the node, readiness before enablement, and unwinding only
// what this run committed. None of them is observable from a return value, and
// none can be exercised against a real host without root and a service manager.
type fakeConverger struct {
	plan lifeops.UpPlan

	trace []string

	// serverName and nodeName are what THIS backend calls its two services.
	// Empty means the packaged systemd units, which is what almost every test
	// here wants; a test that sets them is checking that the command layer asks
	// the backend rather than reaching for deploy's constants.
	serverName, nodeName string

	startErr  map[string]error
	proveErr  map[string]error
	enableErr map[string]error
	disableEr map[string]error

	// enabled is what systemd would answer for each unit, and it moves when this
	// fake is asked to enable one — so a test can stage a unit somebody else
	// enabled while the check was running.
	enabled map[string]string
	// enablePartly makes an enable MUTATE and then fail, which is what an
	// interrupted `systemctl enable` does: some links written, non-zero status.
	// enableLeaves says what state systemd reports afterwards — "enabled" for a
	// partial success, anything else for a unit that changed underneath.
	enablePartly map[string]bool
	enableLeaves map[string]string
	// alsoEnables models `[Install] Also=`: enabling one unit enables another,
	// which no property of the first one reports.
	alsoEnables map[string]string
	// cancelOnEnable ends the caller's context from inside Enable, the way an
	// operator's interrupt arrives — while the mutation is in flight, not
	// before the call.
	cancelOnEnable context.CancelFunc

	// stopErr and stopLeaves model a stop that fails, and one that "succeeds"
	// while the unit comes back — a Restart= that applies, or somebody starting
	// it again. The second is the one a command must not report as down.
	// active is what systemd would answer for each unit's ActiveState.
	active map[string]string

	// alsoStops models a stop TRANSACTION reaching another unit — Conflicts=,
	// PartOf=, BindsTo= — which no property of the stopped unit reports.
	// Stopping the key changes the value unit's snapshot.
	alsoStops map[string]string

	// onStop runs inside StopAndProve, so a test can change the world between
	// one unit stopping and the next — which is when a report is written.
	// onStart runs inside StartAndProve, so a test can change the world while the
	// services are coming up.
	onStart func(unit string)

	onStop func(unit string)

	// disablePartly makes a failing Disable remove the links first.
	disablePartly map[string]bool

	stopErr    map[string]error
	stopLeaves map[string]lifeops.StopResult

	repaired         []string
	repairedPaths    []string
	repairPathsErr   error
	ownershipChanges []lifeops.OwnershipChange
	ownershipErr     error
	identity         func() (int, int, error)
	revalidateErr    map[string]error

	// snapshots is what each unit looks like, and snapshotAfter replaces those
	// answers once a start has happened — so a test can stage a service that
	// was disturbed by starting a different one.
	snapshots     map[string]string
	snapshotAfter map[string]string
	// snapshotErrAfter and enabledErrAfter fail only the Nth call onwards, so a
	// test can let the BEFORE reading succeed and fail the AFTER one — which is
	// the call the check under test actually makes.
	snapshotErr      error
	snapshotErrAfter int
	snapshotCalls    int
	enabledNowErr    error
	enabledErrAfter  int
	enabledCalls     int
	started          bool

	// disabledCtxErr is what the rollback's context looked like when a disable
	// ran: a live context is the property, and it is not visible from the trace.
	disabledCtxErr error

	// startProof is what this backend says it proved by starting a service.
	startProof string

	// running is what each service is executing, for `down`'s identity refusal.
	running    map[string]lifeops.RunningFacts
	runningErr error

	// asked records the service NAMES the queries were given. The trace records
	// mutations, deliberately — but which names the command asks ABOUT is the
	// thing that goes wrong when shared code reaches for one backend's constants,
	// and a query that names a service this host does not have finds nothing and
	// reports that nothing changed.
	asked []string
}

func (f *fakeConverger) record(step string) { f.trace = append(f.trace, step) }

func (f *fakeConverger) Plan(context.Context, lifeops.UpRequest) (lifeops.UpPlan, error) {
	f.record("plan")

	return f.plan, nil
}

// Services and EnablementCmd are what keep the command layer free of systemd's
// vocabulary. The fake answers as the systemd backend does, because these tests
// are about the ORDER the command imposes rather than about which manager is
// underneath — a second backend proves the same order under different names.
func (f *fakeConverger) Services() (string, string) {
	if f.serverName != "" || f.nodeName != "" {
		return f.serverName, f.nodeName
	}

	return deploy.ServerUnitName, deploy.NodeUnitName
}

func (f *fakeConverger) DisableCmd(unit string) string {
	if f.serverName != "" || f.nodeName != "" {
		return "some-other-manager disable " + unit
	}

	return "systemctl disable " + unit
}

func (f *fakeConverger) ManagerName() string {
	if f.serverName != "" || f.nodeName != "" {
		return "some-other-manager"
	}

	return "systemd"
}

func (f *fakeConverger) CollateralNote() string {
	if f.serverName != "" || f.nodeName != "" {
		return "this manager can commit a second service when one is enabled"
	}

	return "an `[Install] Also=` commits a unit to every future boot that nothing here has " +
		"checked a credential for"
}

// Running is what `down`'s identity refusal reads, and it comes from the BACKEND
// so a host with no systemd is not reported on in systemd's terms.
func (f *fakeConverger) Running(_ context.Context,
	req lifeops.UpRequest,
) ([]lifeops.RunningFacts, error) {
	if f.runningErr != nil {
		return nil, f.runningErr
	}

	server, node := f.Services()

	var facts []lifeops.RunningFacts

	for _, s := range []struct {
		want bool
		name string
	}{{req.WantServer, server}, {req.WantNode, node}} {
		if !s.want {
			continue
		}

		fact, ok := f.running[s.name]
		if !ok {
			// The ordinary host: running this build.
			fact = lifeops.RunningFacts{Active: lifeops.Yes, IsThisBuild: lifeops.Yes}
		}

		fact.Name = s.name
		facts = append(facts, fact)
	}

	return facts, nil
}

func (f *fakeConverger) EnablementCmd(units ...string) string {
	if f.serverName != "" || f.nodeName != "" {
		return "some-other-manager is-enabled " + strings.Join(units, " ")
	}

	return "systemctl is-enabled " + strings.Join(units, " ")
}

func (f *fakeConverger) Identity(lifeops.UpRequest) (int, int, error) {
	if f.identity != nil {
		return f.identity()
	}

	return 990, 991, nil
}

func (f *fakeConverger) ApplyOwnership(changes []lifeops.OwnershipChange, _, _ int) error {
	// THE RECORD STRING STAYS "ownership" because `up`'s ordering assertions read
	// it; what each change NAMES is captured beside it, because a restore has to
	// hand over the App key and nothing in the ordering can say whether it did.
	f.ownershipChanges = append(f.ownershipChanges, changes...)

	f.record("ownership")

	return f.ownershipErr
}

func (f *fakeConverger) RepairServerState(dir string, _, _ int) ([]string, error) {
	f.record("repair " + dir)

	return f.repaired, nil
}

// RepairPaths records the NAMES it was given, because what a restore has to get
// right is which entries it hands over — a set that misses the authority leaves
// a control plane that cannot start.
func (f *fakeConverger) RepairPaths(
	dir string, targets []lifeops.RepairTarget, _, _ int,
) ([]string, error) {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Dir {
			names = append(names, t.Name+"/")

			continue
		}

		names = append(names, t.Name)
	}

	f.record("repair-paths " + dir + " " + strings.Join(names, ","))

	if f.repairPathsErr != nil {
		return nil, f.repairPathsErr
	}

	return f.repairedPaths, nil
}

func (f *fakeConverger) Revalidate(_ context.Context, _ lifeops.UpRequest,
	want lifeops.UnitPlan,
) error {
	f.record("revalidate " + want.Name)

	return f.revalidateErr[want.Name]
}

func (f *fakeConverger) StartAndProve(ctx context.Context, unit string) (string, error) {
	if f.onStart != nil {
		f.onStart(unit)
	}

	f.record("start " + unit)
	f.started = true

	if err := f.startErr[unit]; err != nil {
		return "", err
	}

	// WHAT THE BACKEND PROVED, in its own words. The command prints this
	// verbatim, so a fake that returned a fixed sentence would let a test about
	// what `up` reports pass against a backend that proved something else.
	if f.startProof != "" {
		return f.startProof, nil
	}

	return "ready, and still running after the settle window", nil
}

func (f *fakeConverger) Snapshot(_ context.Context, unit string) (string, error) {
	f.asked = append(f.asked, "snapshot "+unit)
	f.snapshotCalls++
	if f.snapshotErr != nil && f.snapshotCalls > f.snapshotErrAfter {
		return "", f.snapshotErr
	}
	if f.started {
		if s, ok := f.snapshotAfter[unit]; ok {
			return s, nil
		}
	}
	if s, ok := f.snapshots[unit]; ok {
		return s, nil
	}

	return "active/running (result success, pid 1643, 0 restarts)", nil
}

func (f *fakeConverger) ProveStable(_ context.Context, unit string) error {
	f.record("prove " + unit)

	return f.proveErr[unit]
}

func (f *fakeConverger) EnabledNow(ctx context.Context, unit string) (lifeops.Enablement, error) {
	f.asked = append(f.asked, "enabled "+unit)
	f.enabledCalls++
	if f.enabledNowErr != nil && f.enabledCalls > f.enabledErrAfter {
		return lifeops.Enablement{}, f.enabledNowErr
	}
	// HONOURING ctx IS THE POINT HERE. `systemctl show` on a cancelled context
	// fails, so a fake that answered anyway would hide a run that asks what
	// happened using the very context whose cancellation caused the failure.
	if err := ctx.Err(); err != nil {
		return lifeops.Enablement{}, err
	}

	if s, ok := f.enabled[unit]; ok {
		return enablementOf(s), nil
	}

	return enablementOf("disabled"), nil
}

// enablementOf maps a manager's word to a verdict the way the real backends do:
// exactly two words are classified and everything else is Unknown.
//
// THE FAKE MUST NOT BE MORE GENEROUS THAN THE REAL ONE. Classifying `static` or
// `masked` as anything but Unknown here would let a test pass against a command
// that acts on states billet has no rule for -- which is the bug the mapping
// exists to prevent.
func enablementOf(word string) lifeops.Enablement {
	switch word {
	case "enabled":
		return lifeops.Enablement{Enabled: lifeops.Yes, How: word}
	case "disabled":
		return lifeops.Enablement{Enabled: lifeops.No, How: word}
	default:
		return lifeops.Enablement{Enabled: lifeops.Unknown, How: word}
	}
}

func (f *fakeConverger) Enable(_ context.Context, unit string) error {
	f.record("enable " + unit)

	err := f.enableErr[unit]
	if err != nil && f.cancelOnEnable != nil {
		f.cancelOnEnable()
	}

	if err == nil || f.enablePartly[unit] {
		if f.enabled == nil {
			f.enabled = map[string]string{}
		}

		left := "enabled"
		if s, ok := f.enableLeaves[unit]; ok {
			left = s
		}
		f.enabled[unit] = left

		if also, ok := f.alsoEnables[unit]; ok {
			f.enabled[also] = "enabled"
		}
	}

	return err
}

func (f *fakeConverger) StopAndProve(_ context.Context, unit string) (lifeops.StopResult, error) {
	f.record("stop " + unit)

	if f.onStop != nil {
		f.onStop(unit)
	}

	// MUTATE-THEN-FAIL, which is what an interrupted systemctl does: the
	// transaction moved and the command still exited non-zero. A fake that
	// returned before applying alsoStops could not model it, and the production
	// path that skips the after-observation on error would be untestable.
	if err := f.stopErr[unit]; err != nil {
		if also, ok := f.alsoStops[unit]; ok {
			if f.snapshots == nil {
				f.snapshots = map[string]string{}
			}
			f.snapshots[also] = "inactive/dead (result success, pid 0, 0 restarts)"
		}

		return lifeops.StopResult{}, err
	}

	// IT RETURNS WHAT IT IS TOLD AND JUDGES NOTHING. An earlier version copied
	// the real converger's "came back after stopping" refusal into here, so the
	// command test that named that behaviour was asserting the FAKE — deleting
	// the guard from internal/lifeops left it green. The guard is tested where
	// it lives, in converge_test.go; a caller staging a failure here uses
	// stopErr.
	if left, ok := f.stopLeaves[unit]; ok {
		return left, nil
	}

	if f.active == nil {
		f.active = map[string]string{}
	}
	f.active[unit] = "inactive"

	if also, ok := f.alsoStops[unit]; ok {
		if f.snapshots == nil {
			f.snapshots = map[string]string{}
		}
		f.snapshots[also] = "inactive/dead (result success, pid 0, 0 restarts)"
	}

	return lifeops.StopResult{Gone: lifeops.Yes, How: "is inactive"}, nil
}

func (f *fakeConverger) Disable(_ context.Context, unit string) error {
	f.record("disable " + unit)
	if err := f.disableEr[unit]; err != nil {
		// The same mutate-then-fail shape: `systemctl disable` can remove links
		// and still exit non-zero.
		if f.disablePartly[unit] {
			if f.enabled == nil {
				f.enabled = map[string]string{}
			}
			f.enabled[unit] = "disabled"
		}

		return err
	}

	if f.enabled == nil {
		f.enabled = map[string]string{}
	}
	f.enabled[unit] = "disabled"

	// `systemctl disable` FOLLOWS `[Install] Also=` as well, so undoing one
	// unit's enablement removes the other's. A fake that changed only the unit
	// it was given would be doing the safety-relevant part of systemd's job for
	// the test.
	if also, ok := f.alsoEnables[unit]; ok {
		f.enabled[also] = "disabled"
	}

	return nil
}

// stageUp installs the fakes and returns the recorder. The real seams are
// restored by t.Cleanup, so a failure cannot leave a later test talking to the
// host's own systemd.
func stageUp(t *testing.T, f *fakeConverger, verdict githubVerdict) *fakeConverger {
	t.Helper()

	realConverge, realCheck, realLock := converge, check, lifecycleLock
	t.Cleanup(func() { converge, check, lifecycleLock = realConverge, realCheck, realLock })

	// THE LOCK IS STUBBED, NOT EXERCISED HERE. It lives in a directory only root
	// can create, and these tests are about the ORDER `up` does things in, which
	// is entirely on the far side of taking it. hostlock_test.go covers the lock
	// against a real directory.
	lifecycleLock = func() (*hostLock, error) {
		f.record("lock")

		return &hostLock{}, nil
	}

	converge = func(...lifeops.ConvergeOption) converger { return f }
	check = func(context.Context, checkOptions) (checkReport, error) {
		f.record("check")

		return checkReport{github: verdict}, nil
	}

	return f
}

// bothUnits is a plan that starts and enables the server and then the node.
func bothUnits() lifeops.UpPlan {
	return lifeops.UpPlan{
		ServerState: "/var/lib/billet/server",
		Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: true, Enable: true},
			{Name: deploy.NodeUnitName, Start: true, Enable: true},
		},
	}
}

// serviceConfig writes a full server+node config and returns its path, along
// with the path the units are pretended to read — the same file, so a test can
// get past the shape refusal and reach the orchestration.
func serviceConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")
	key := filepath.Join(dir, "app-private-key.pem")

	// A REAL DIRECTORY, because `up` now opens the ledger at the end to clear a
	// shutdown seal — and a failure to do that is reported rather than swallowed.
	// Pointing this at /var/lib made every one of these tests fail on a machine
	// where that is not writable, which is every machine running the suite.
	body := `server:
  listen: 127.0.0.1:7717
  state_dir: ` + filepath.Join(dir, "server-state") + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + key + `
node:
  name: probe-node
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return path
}

// nodeOnlyConfig has no server section, so no GitHub credential is involved.
func nodeOnlyConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")
	body := `node:
  name: probe-node
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return path
}

// THE WHOLE ORDER, ON A HOST THAT IS READY. Each step in this sequence exists
// because doing it later would be a defect: ownership before the check so the
// check can read the config, ownership AND the state repair after it because
// the check opens the ledger as this user, and every start before any enable.
func TestUpFollowsTheOrderItsSafetyDependsOn(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("a prepared host was refused: %v", err)
	}

	want := []string{
		"plan",
		// Taken before the first mutation and released on the way out, so a
		// concurrent `down` cannot stop the services this is starting.
		"lock",
		"ownership",
		"check",
		"ownership",
		"repair /var/lib/billet/server",
		// Asked again immediately before acting: the plan above is as old as the
		// GitHub probe took, and a unit can be daemon-reloaded into something
		// else in that time.
		"revalidate " + deploy.ServerUnitName,
		"start " + deploy.ServerUnitName,
		"enable " + deploy.ServerUnitName,
		"revalidate " + deploy.NodeUnitName,
		"start " + deploy.NodeUnitName,
		"enable " + deploy.NodeUnitName,
	}
	if strings.Join(f.trace, " → ") != strings.Join(want, " → ") {
		t.Errorf("up did:\n  %s\nwant:\n  %s",
			strings.Join(f.trace, " → "), strings.Join(want, " → "))
	}
}

// GITHUB MUST BE PROVED, NOT MERELY UNREFUTED. `billet check` exits 0 when the
// probe was skipped or could not complete, which is right for a diagnostic and
// is not consent to start a control plane on somebody's organization.
func TestUpStartsNothingWhenGitHubWasNotProved(t *testing.T) {
	asLinux(t)

	for _, verdict := range []githubVerdict{
		githubSkipped, githubUnverifiable, githubFailed, githubNotConfigured,
	} {
		t.Run(verdict.String(), func(t *testing.T) {
			cfg := serviceConfig(t)
			f := stageUp(t, &fakeConverger{plan: bothUnits()}, verdict)

			err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
			if err == nil {
				t.Fatal("a control plane was started on a credential nothing proved")
			}
			if !strings.Contains(err.Error(), "was not verified") {
				t.Errorf("the refusal does not say what was missing: %v", err)
			}

			// AND THEREFORE NOTHING ELSE HAPPENED. Asserting only the error would
			// pass against a command that started both services and then complained.
			for _, step := range f.trace {
				if strings.HasPrefix(step, "start") || strings.HasPrefix(step, "enable") {
					t.Errorf("%q ran anyway; the whole trace was %v", step, f.trace)
				}
			}
		})
	}
}

// A NODE-ONLY HOST NEEDS NO GITHUB PROOF, because it starts no control plane.
// Requiring one would make a compute host unbringable-up without a credential
// it does not use.
func TestUpDoesNotRequireGitHubForANodeOnlyHost(t *testing.T) {
	asLinux(t)

	cfg := nodeOnlyConfig(t)
	f := stageUp(t, &fakeConverger{plan: lifeops.UpPlan{
		Units: []lifeops.UnitPlan{{Name: deploy.NodeUnitName, Start: true, Enable: true}},
	}}, githubNotConfigured)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("a node-only host was refused: %v", err)
	}
	if !strings.Contains(strings.Join(f.trace, " "), "start "+deploy.NodeUnitName) {
		t.Errorf("the node was never started: %v", f.trace)
	}
}

// A FAILURE UNWINDS WHAT THIS RUN COMMITTED, AND ONLY THAT. The node has no
// dependency on the server, so a unit left enabled by a failed run starts at
// every future boot with nothing having established it can run.
func TestUpUndoesTheEnablementItPerformedWhenALaterUnitFails(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan:     bothUnits(),
		startErr: map[string]error{deploy.NodeUnitName: errors.New("the node never came up")},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a failed node was reported as success")
	}
	if !strings.Contains(err.Error(), "never came up") {
		t.Errorf("the cause was lost: %v", err)
	}

	joined := strings.Join(f.trace, " → ")
	if !strings.Contains(joined, "disable "+deploy.ServerUnitName) {
		t.Errorf("the enablement this run performed was not undone: %s", joined)
	}
	if strings.Contains(joined, "disable "+deploy.NodeUnitName) {
		t.Errorf("a unit that was never enabled was disabled anyway: %s", joined)
	}
}

// AND IT DOES NOT UNDO SOMEBODY ELSE'S. `systemctl enable` is idempotent, so a
// successful enable is not evidence this run performed the transition — another
// operator may have done it while `billet check` was talking to GitHub, and
// disabling it on the way out would take down what they just committed.
func TestUpNeverDisablesAnEnablementItDidNotPerform(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: bothUnits(),
		// The server was enabled by somebody else between the plan and here.
		enabled:  map[string]string{deploy.ServerUnitName: "enabled"},
		startErr: map[string]error{deploy.NodeUnitName: errors.New("the node never came up")},
	}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err == nil {
		t.Fatal("a run that found the server already enabled was reported as success")
	}

	if strings.Contains(strings.Join(f.trace, " → "), "disable ") {
		t.Errorf("an enablement this run did not perform was undone: %v", f.trace)
	}
}

// ONLY "disabled" IS PERMISSION TO ENABLE. systemd has a dozen enablement
// states and folding them to a boolean made every one of them — masked, static,
// linked, an answer billet could not read — mean what disabled means.
func TestUpEnablesOnlyAUnitItFindsDisabled(t *testing.T) {
	asLinux(t)

	for _, state := range []string{
		"masked", "masked-runtime", "static", "indirect", "generated",
		"transient", "alias", "linked", "linked-runtime", "enabled-runtime",
		"bad", "", "a-state-systemd-adds-later",
	} {
		t.Run(state, func(t *testing.T) {
			cfg := serviceConfig(t)
			f := stageUp(t, &fakeConverger{
				plan:    bothUnits(),
				enabled: map[string]string{deploy.ServerUnitName: state},
			}, githubVerified)

			err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
			if err == nil {
				t.Fatalf("a unit that was %q was enabled anyway", state)
			}
			if !strings.Contains(err.Error(), "only enables a service it finds disabled") {
				t.Errorf("the refusal does not say what it required: %v", err)
			}

			joined := strings.Join(f.trace, " → ")
			if strings.Contains(joined, "enable ") {
				t.Errorf("enable was called on a %q unit: %s", state, joined)
			}
			// The node is never reached, so nothing was committed for it either.
			if strings.Contains(joined, deploy.NodeUnitName) {
				t.Errorf("the run continued past a unit it could not account for: %s", joined)
			}
		})
	}
}

// AN INTERRUPTED ENABLE THAT WROTE SOME LINKS IS STILL THIS RUN'S TO UNDO, and
// finding that out takes a live context: the query runs after the failure, and
// the likeliest cause of the failure is the operator's own interrupt.
func TestUpUndoesAnEnableThatFailedAfterWritingLinks(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The interrupt lands INSIDE the enable, after links were written: that is
	// what makes the question "did my enable land?" one that cannot be asked on
	// the caller's own context.
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: true, Enable: true},
		}},
		enableErr:      map[string]error{deploy.ServerUnitName: context.Canceled},
		enablePartly:   map[string]bool{deploy.ServerUnitName: true},
		cancelOnEnable: cancel,
	}, githubVerified)

	if err := runLocalUp(ctx, upOptions{configPath: cfg, servicePath: cfg}); err == nil {
		t.Fatal("an interrupted enable was reported as success")
	}

	if !strings.Contains(strings.Join(f.trace, " → "), "disable "+deploy.ServerUnitName) {
		t.Errorf("an enable that wrote links before failing was left in place: %v", f.trace)
	}
}

// THE UNWINDING OUTLIVES THE CANCELLATION THAT CAUSED IT. An operator's Ctrl-C
// is the likeliest way into rollback, and a disable issued on the cancelled
// context would fail — leaving the host committed to booting a service nothing
// proved.
func TestUpRollsBackEvenWhenTheContextIsCancelled(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)

	ctx, cancel := context.WithCancel(t.Context())

	// The node fails the way an interrupted start does, and the cancellation
	// lands where an operator's Ctrl-C lands: after the server is up and
	// enabled, while the node is what they are waiting on.
	f := &fakeConverger{
		plan:     bothUnits(),
		startErr: map[string]error{deploy.NodeUnitName: context.Canceled},
	}
	stageUp(t, f, githubVerified)
	converge = func(...lifeops.ConvergeOption) converger {
		return &cancellingConverger{fakeConverger: f, cancel: cancel}
	}

	if err := runLocalUp(ctx, upOptions{configPath: cfg, servicePath: cfg}); err == nil {
		t.Fatal("a cancelled run was reported as success")
	}

	if !strings.Contains(strings.Join(f.trace, " → "), "disable "+deploy.ServerUnitName) {
		t.Errorf("nothing was unwound after cancellation: %v", f.trace)
	}
	// The disable really did run on a live context.
	if f.disabledCtxErr != nil {
		t.Errorf("the rollback ran on the cancelled context: %v", f.disabledCtxErr)
	}
}

// cancellingConverger cancels the run at the moment the node is started, which
// is where an interrupt lands on a real host: the server is up and enabled, and
// the node is what the operator is waiting on.
type cancellingConverger struct {
	*fakeConverger
	cancel context.CancelFunc
}

func (c *cancellingConverger) StartAndProve(ctx context.Context, unit string) (string, error) {
	if unit == deploy.NodeUnitName {
		c.cancel()
	}

	return c.fakeConverger.StartAndProve(ctx, unit)
}

func (c *cancellingConverger) Disable(ctx context.Context, unit string) error {
	c.disabledCtxErr = ctx.Err()

	return c.fakeConverger.Disable(ctx, unit)
}

// AN ALREADY-RUNNING SERVICE IS PROVED BEFORE IT IS ENABLED, and never started.
// The plan's sample is minutes old by the time enablement happens — `billet
// check` talks to GitHub in between — so enabling on it could commit a service
// that has since begun crash-looping to every future boot.
func TestUpProvesARunningServiceBeforeEnablingIt(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{plan: lifeops.UpPlan{
		Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: false, Enable: true},
			{Name: deploy.NodeUnitName, Start: false, Enable: false},
		},
	}}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("a running host was refused: %v", err)
	}

	joined := strings.Join(f.trace, " → ")
	if !strings.Contains(joined, "prove "+deploy.ServerUnitName+" → enable "+deploy.ServerUnitName) {
		t.Errorf("a running service was enabled without being proved first: %s", joined)
	}
	// AND NOTHING WAS RESTARTED. A restart is a drain, and a drain destroys the
	// jobs the host is holding.
	if strings.Contains(joined, "start ") {
		t.Errorf("a running service was started: %s", joined)
	}
	// The already-enabled node is not proved: nothing is being committed for it.
	if strings.Contains(joined, "prove "+deploy.NodeUnitName) {
		t.Errorf("a service nothing was being committed for was probed anyway: %s", joined)
	}
}

// AND A RUNNING SERVICE THAT IS NOT STABLE IS NOT ENABLED.
func TestUpDoesNotEnableARunningServiceThatCannotBeProved(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: false, Enable: true},
		}},
		proveErr: map[string]error{deploy.ServerUnitName: errors.New("crash loop")},
	}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err == nil {
		t.Fatal("a crash-looping service was enabled")
	}
	if strings.Contains(strings.Join(f.trace, " "), "enable ") {
		t.Errorf("it was enabled anyway: %v", f.trace)
	}
}

// --dry-run CHANGES NOTHING. It is the same decision as the real run, reported
// rather than applied, which is the only thing that makes it worth having.
func TestUpDryRunMutatesNothing(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg, dryRun: true}); err != nil {
		t.Fatalf("--dry-run failed: %v", err)
	}

	if strings.Join(f.trace, " ") != "plan" {
		t.Errorf("--dry-run did more than plan: %v", f.trace)
	}
}

// A CONFIG THE UNITS CANNOT READ IS REFUSED BEFORE ANYTHING IS TOUCHED.
func TestUpRefusesAConfigThePackagedUnitsCannotUse(t *testing.T) {
	asLinux(t)

	err := cmdLocalUp(t.Context(), []string{"--config", nodeOnlyConfig(t)})
	if err == nil {
		t.Fatal("a config at the wrong path was accepted")
	}

	// It names the path the units actually read, and how to get one there.
	if !strings.Contains(err.Error(), initconfig.ServiceConfigPathFor(hostOS)) {
		t.Errorf("the refusal does not name the path the units read: %v", err)
	}
	if !strings.Contains(err.Error(), "--profile local-service") {
		t.Errorf("the refusal does not say how to generate one: %v", err)
	}
}

// AN APP KEY THE SERVICE CANNOT READ IS ITS OWN REFUSAL. Both units set
// ProtectHome=true, so a key under a home directory is readable by the operator
// running `billet check` and invisible to the process that needs it — which
// fails at startup rather than here.
func TestUpRefusesAnAppKeyTheServiceCannotReach(t *testing.T) {
	asLinux(t)

	dir := t.TempDir()
	cfg := filepath.Join(dir, "billet.yaml")
	body := `server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /home/ci/app-private-key.pem
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("an App key outside the service's reach was accepted")
	}
	if !strings.Contains(err.Error(), "ProtectHome") {
		t.Errorf("the refusal does not say why the service cannot read it: %v", err)
	}
	for _, step := range f.trace {
		if strings.HasPrefix(step, "start") || strings.HasPrefix(step, "ownership") {
			t.Errorf("%q ran before the refusal: %v", step, f.trace)
		}
	}
}

// EVERY REASON AT ONCE, and every one with its remedy.
func TestUpReportsEveryRefusalWithItsRemedy(t *testing.T) {
	asLinux(t)

	f := stageUp(t, &fakeConverger{plan: lifeops.UpPlan{Refusals: []lifeops.Refusal{
		{What: "the node unit is masked", Remedy: "systemctl unmask billet-node.service"},
	}}}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: nodeOnlyConfig(t)})
	if err == nil {
		t.Fatal("an unprepared host was accepted")
	}

	text := err.Error()
	if !strings.HasPrefix(text, "this host is not ready to bring billet up:") {
		t.Errorf("the refusal does not lead with what it is: %v", err)
	}
	// The config-shape problem AND the host's own, each on its own bullet.
	if strings.Count(text, "\n  - ") < 2 {
		t.Errorf("the refusals were folded into one: %v", err)
	}
	if !strings.Contains(text, "systemctl unmask") {
		t.Errorf("the host's own refusal was dropped: %v", err)
	}
	if len(f.trace) > 1 {
		t.Errorf("a refused host was mutated anyway: %v", f.trace)
	}
}

// THE GUIDED PATH WORKS WITH NO FLAGS. billet's ordinary config default is
// per-user, which for the `local` family named a file in root's home directory
// that nothing ever creates — so on a host the package had just prepared,
// `billet local up` failed with an error about the wrong file entirely. The
// units read one config, and these commands are about those units.
func TestLocalCommandsDefaultToTheConfigTheUnitsRead(t *testing.T) {
	asLinux(t)

	if _, err := os.Stat(initconfig.ServiceConfigPathFor(hostOS)); err == nil {
		t.Skipf("this machine has a real %s, so the error below would not be about "+
			"the path being defaulted", initconfig.ServiceConfigPathFor(hostOS))
	}

	t.Run("up", func(t *testing.T) {
		err := cmdLocalUp(t.Context(), nil)
		if err == nil {
			t.Fatal("up succeeded without a config")
		}
		if !strings.Contains(err.Error(), initconfig.ServiceConfigPathFor(hostOS)) {
			t.Errorf("up with no --config looked somewhere else: %v", err)
		}
	})

	t.Run("status", func(t *testing.T) {
		realInspect := inspect
		t.Cleanup(func() { inspect = realInspect })

		var asked string
		inspect = func(_ context.Context, cfgPath, _ string) (lifeops.Report, error) {
			asked = cfgPath

			return lifeops.Report{ConfigPath: cfgPath}, nil
		}

		if err := cmdLocalStatus(t.Context(), nil); err != nil {
			t.Fatalf("status: %v", err)
		}
		if asked != initconfig.ServiceConfigPathFor(hostOS) {
			t.Errorf("status with no --config inspected %q, want %q",
				asked, initconfig.ServiceConfigPathFor(hostOS))
		}
	})
}

// AN UNRESOLVABLE SERVICE ACCOUNT STOPS THE RUN BEFORE ANYTHING IS TOUCHED.
// Ownership is the first mutation, and it cannot be attempted against an
// identity billet could not resolve — chown reads -1 as "leave this alone", so
// the attempt would report success having changed nothing.
func TestUpStopsBeforeMutatingWhenTheIdentityIsUnresolvable(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan:     bothUnits(),
		identity: func() (int, int, error) { return -1, -1, errors.New("no billet account") },
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a host with no service account was converged anyway")
	}
	if !strings.Contains(err.Error(), "no billet account") {
		t.Errorf("the cause was lost: %v", err)
	}

	// The lock is in here because it is taken before the first mutation, which is
	// the point of it; it changes nothing about the host and is released on the
	// way out. Anything BEYOND these two would be a mutation made before billet
	// knew who to make it as.
	if strings.Join(f.trace, " ") != "plan lock" {
		t.Errorf("something was mutated before the identity was resolved: %v", f.trace)
	}
}

// AN ENABLE THAT FAILS UNWINDS WHAT CAME BEFORE IT. The server is committed to
// boot by then, and leaving it that way after the run failed is the state this
// command exists to avoid: a unit that starts at every boot with nothing having
// established it can run.
func TestUpUndoesEarlierEnablementWhenALaterEnableFails(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan:      bothUnits(),
		enableErr: map[string]error{deploy.NodeUnitName: errors.New("systemctl enable: read-only")},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a failed enable was reported as success")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the cause was lost: %v", err)
	}

	joined := strings.Join(f.trace, " → ")
	if !strings.Contains(joined, "disable "+deploy.ServerUnitName) {
		t.Errorf("the server was left enabled after the run failed: %s", joined)
	}
	// The node's own enable did not take, so there is nothing of its to undo.
	if strings.Contains(joined, "disable "+deploy.NodeUnitName) {
		t.Errorf("a unit whose enable failed was disabled anyway: %s", joined)
	}
}

// A STATE BILLET DID NOT PUT THE UNIT IN IS NOT BILLET'S TO UNDO. After a
// failed enable, only "enabled" shows the links this call wrote; anything else
// is a unit that changed underneath the run, and issuing `systemctl disable`
// against it would remove something this command cannot show it created.
func TestUpDoesNotDisableAStateItCannotShowItCreated(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: true, Enable: true},
		}},
		enableErr:    map[string]error{deploy.ServerUnitName: errors.New("enable failed")},
		enablePartly: map[string]bool{deploy.ServerUnitName: true},
		enableLeaves: map[string]string{deploy.ServerUnitName: "masked"},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a failed enable was reported as success")
	}

	if strings.Contains(strings.Join(f.trace, " → "), "disable ") {
		t.Errorf("billet disabled a unit it could not show it had enabled: %v", f.trace)
	}
	// AND IT SAYS SO, rather than leaving the operator to find the unit masked.
	if !strings.Contains(err.Error(), "will not undo") {
		t.Errorf("the uncertainty was not reported: %v", err)
	}
}

// STARTING ONE SERVICE MUST NOT DISTURB THE OTHER. billet refuses a unit that
// names the other one in its dependencies, but a transaction reaches further
// than that — a unit billet's own unit pulls in may itself conflict with the
// other service, and `Conflicts=` is a STOP. If the node was holding jobs, they
// are gone; the least this command can do is refuse to carry on as though
// nothing happened.
func TestUpRefusesWhenStartingOneServiceDisturbsTheOther(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		// The node is already running and is not being started; the server is.
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: true, Enable: true},
			{Name: deploy.NodeUnitName, Start: false, Enable: false},
		}},
		snapshots: map[string]string{
			deploy.NodeUnitName: "active/running (result success, pid 4242, 0 restarts)",
		},
		// Starting the server stopped it — which is what a Conflicts= reached
		// through a pulled-in unit does.
		snapshotAfter: map[string]string{
			deploy.NodeUnitName: "inactive/dead (result success, pid 0, 0 restarts)",
		},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a start that stopped the other service was reported as success")
	}

	for _, want := range []string{"disturbed", "they are gone", deploy.NodeUnitName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}

	// AND IT STOPPED THERE. Carrying on to enable things after a service was
	// destroyed is the opposite of what this check is for.
	if strings.Contains(strings.Join(f.trace, " → "), "enable ") {
		t.Errorf("the run continued after disturbing a service: %v", f.trace)
	}
}

// AND AN UNDISTURBED HOST IS NOT REFUSED, so the check above cannot be passing
// because every run now fails.
func TestUpAcceptsAStartThatDisturbsNothing(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: true, Enable: true},
			{Name: deploy.NodeUnitName, Start: false, Enable: false},
		}},
		snapshots: map[string]string{
			deploy.NodeUnitName: "active/running (result success, pid 4242, 0 restarts)",
		},
	}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("a run that disturbed nothing was refused: %v", err)
	}
	if !strings.Contains(strings.Join(f.trace, " → "), "start "+deploy.ServerUnitName) {
		t.Errorf("the server was never started: %v", f.trace)
	}
}

// AN `[Install] Also=` COMMITS A UNIT NOBODY NAMED. `systemctl enable
// billet-node.service` with `Also=billet-server.service` in its [Install]
// section enables the server too — on a node-only host, that is an unverified
// control plane committed to every future boot, and it is invisible in every
// property systemd reports about the node itself.
func TestUpRefusesWhenEnablingOneUnitEnablesTheOther(t *testing.T) {
	asLinux(t)

	cfg := nodeOnlyConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.NodeUnitName, Start: true, Enable: true},
		}},
		enabled: map[string]string{
			deploy.NodeUnitName:   "disabled",
			deploy.ServerUnitName: "disabled",
		},
		// Enabling the node also enabled the server, the way Also= does.
		alsoEnables: map[string]string{deploy.NodeUnitName: deploy.ServerUnitName},
	}, githubNotConfigured)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("an enable that committed the other unit was reported as success")
	}
	for _, want := range []string{"also made", deploy.ServerUnitName, "Also="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
	// THE HOST IS BACK AS IT WAS FOUND, which is the property — not that a
	// disable command was issued. Both units were disabled before the run.
	for unit, want := range map[string]string{
		deploy.NodeUnitName:   "disabled",
		deploy.ServerUnitName: "disabled",
	} {
		if f.enabled[unit] != want {
			t.Errorf("%s was left %q, want %q — the run did not put the host back",
				unit, f.enabled[unit], want)
		}
	}
}

// AND A TRANSITIVE START OF THE OTHER SERVICE IS CAUGHT ON A NODE-ONLY HOST,
// which is where it matters most: the GitHub proof was skipped on the grounds
// that no control plane was going to start, so a `Requires=` reaching the
// server through something the node pulls in starts one on an unverified
// credential. The server is not in this plan at all — looking only at planned
// units would miss it entirely.
func TestUpRefusesWhenStartingTheNodeStartsTheServer(t *testing.T) {
	asLinux(t)

	cfg := nodeOnlyConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.NodeUnitName, Start: true, Enable: true},
		}},
		snapshots: map[string]string{
			deploy.ServerUnitName: "inactive/dead (result success, pid 0, 0 restarts)",
		},
		snapshotAfter: map[string]string{
			deploy.ServerUnitName: "active/running (result success, pid 9001, 0 restarts)",
		},
	}, githubNotConfigured)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("starting the node started a control plane and the run reported success")
	}
	if !strings.Contains(err.Error(), deploy.ServerUnitName) {
		t.Errorf("the failure does not name what was started: %v", err)
	}
	if strings.Contains(strings.Join(f.trace, " → "), "enable ") {
		t.Errorf("the run continued after starting a service it never checked: %v", f.trace)
	}
}

// A CHECK THAT COULD NOT BE MADE IS NOT A CHECK THAT PASSED. Both bystander
// proofs rest on being able to ask systemd about a unit; if that fails, billet
// has no idea whether starting one service disturbed the other, and "carry on"
// is the one answer it must not give.
func TestUpStopsWhenItCannotTellWhetherItDisturbedAnything(t *testing.T) {
	asLinux(t)

	t.Run("the running state cannot be read", func(t *testing.T) {
		cfg := serviceConfig(t)
		// The BEFORE reading succeeds; the one after the start fails. Failing
		// the first call would stop the run before it ever reached the check
		// this subtest is named for.
		f := stageUp(t, &fakeConverger{
			plan:             bothUnits(),
			snapshotErr:      errors.New("systemctl: connection refused"),
			snapshotErrAfter: 1,
		}, githubVerified)

		err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
		if err == nil {
			t.Fatal("a run that could not look at the other service reported success")
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("the cause was lost: %v", err)
		}
		// AND IT STOPPED THERE: nothing was enabled after a check that could
		// not be made.
		if strings.Contains(strings.Join(f.trace, " → "), "enable ") {
			t.Errorf("it enabled a unit after failing to check the other: %v", f.trace)
		}
	})

	t.Run("the enablement cannot be read", func(t *testing.T) {
		cfg := serviceConfig(t)
		// Three readings happen before the one this is about: the enablement
		// the run arrived to, and the pair taken just before the first enable.
		f := stageUp(t, &fakeConverger{
			plan:            bothUnits(),
			enabledNowErr:   errors.New("systemctl: connection refused"),
			enabledErrAfter: 4,
		}, githubVerified)

		err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
		if err == nil {
			t.Fatal("a run that could not read enablement reported success")
		}
		if strings.Contains(strings.Join(f.trace, " → "), "enable ") {
			t.Errorf("it enabled a unit without being able to see the other: %v", f.trace)
		}
	})
}

// THE UNDOING IS CHECKED AGAINST THE HOST AS IT WAS FOUND. `systemctl disable`
// follows `[Install] Also=` too, so unwinding this run's own enablement can
// remove one that predates the command — a service an operator had committed to
// boot, gone because billet tidied up after itself. billet cannot stop that; it
// must not leave them unaware of it.
func TestUpReportsWhenItsOwnUndoingRemovedSomethingOlder(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.NodeUnitName, Start: true, Enable: true},
			{Name: deploy.ServerUnitName, Start: true, Enable: false},
		}},
		// The server was enabled before this command ran, and the node's
		// [Install] Also= names it — so disabling the node removes it.
		enabled: map[string]string{
			deploy.NodeUnitName:   "disabled",
			deploy.ServerUnitName: "enabled",
		},
		alsoEnables: map[string]string{deploy.NodeUnitName: deploy.ServerUnitName},
		// Something later fails, so the run unwinds what it committed.
		startErr: map[string]error{deploy.ServerUnitName: errors.New("the server never came up")},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a failed run was reported as success")
	}

	if !strings.Contains(err.Error(), "removed more than this run had committed") {
		t.Errorf("billet did not report that its undo took an older enablement: %v", err)
	}
	if !strings.Contains(err.Error(), deploy.ServerUnitName) {
		t.Errorf("the report does not name what was removed: %v", err)
	}
	// And it really was removed — the fake models what systemctl does.
	if f.enabled[deploy.ServerUnitName] != "disabled" {
		t.Errorf("the fixture did not reach the case: the server is %q",
			f.enabled[deploy.ServerUnitName])
	}
}

// AND A SUCCESSFUL `systemctl enable` THAT DID NOT ENABLE ANYTHING IS NOT A
// SUCCESS. A zero exit status is not the same fact as the unit being enabled.
func TestUpRefusesAnEnableThatSucceededWithoutEnabling(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{Units: []lifeops.UnitPlan{
			{Name: deploy.ServerUnitName, Start: true, Enable: true},
		}},
		// enable returns nil, and the unit is still disabled afterwards.
		enableLeaves: map[string]string{deploy.ServerUnitName: "disabled"},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("an enable that committed nothing was reported as success")
	}
	if !strings.Contains(err.Error(), "rather than enabled") {
		t.Errorf("the failure does not say what was wrong: %v", err)
	}
	if !strings.Contains(strings.Join(f.trace, " "), "enable ") {
		t.Errorf("the fixture did not reach the case: %v", f.trace)
	}
}

// A UNIT THAT CHANGED WHILE BILLET WAS CHECKING GITHUB IS NOT THE UNIT THE PLAN
// DECIDED ABOUT. The probe takes as long as the network takes, and in that
// window an edit plus a daemon-reload can turn the validated unit into one that
// runs a different command, as a different account — or a drain can begin,
// turning the inactive unit the plan saw into one that is deactivating.
func TestUpAsksAgainImmediatelyBeforeActing(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: bothUnits(),
		revalidateErr: map[string]error{
			deploy.ServerUnitName: errors.New("billet-server.service changed while billet was " +
				"checking GitHub"),
		},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a unit that changed under the run was started anyway")
	}
	if !strings.Contains(err.Error(), "changed while billet was checking GitHub") {
		t.Errorf("the cause was lost: %v", err)
	}

	// AND NOTHING WAS STARTED. The whole point of asking again is to not act.
	if strings.Contains(strings.Join(f.trace, " → "), "start ") {
		t.Errorf("it started the unit anyway: %v", f.trace)
	}
}

// THE COMMAND LAYER MUST NOT KNOW WHICH SERVICE MANAGER IT IS DRIVING, and this
// is the test that makes that real rather than aspirational.
//
// Every place `up` talks about "the other service" — the bystander snapshot it
// takes before starting one, the enablement it compares before and after, the
// command a refusal tells an operator to run — used to reach for deploy's
// systemd unit names. A converger for another service manager would then be
// handed `billet-server.service`, ask about a service that does not exist, find
// nothing, and report that nothing had changed: a bystander check that cannot
// see, and an enablement comparison that can never differ.
//
// So this drives the whole command with a backend that calls its services
// something else entirely, and asserts that NO systemd name appears anywhere in
// what it did. Reintroducing deploy.ServerUnitName at any one of those sites
// fails it.
func TestUpUsesTheBackendsOwnServiceNamesAndNoOthers(t *testing.T) {
	const (
		server = "sh.example.server"
		node   = "sh.example.node"
	)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		serverName: server,
		nodeName:   node,
		plan: lifeops.UpPlan{
			ServerState: "/var/lib/billet/server",
			Units: []lifeops.UnitPlan{
				{Name: server, Start: true, Enable: true},
				{Name: node, Start: true, Enable: true},
			},
		},
	}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("runLocalUp: %v", err)
	}

	// EVERYTHING IT DID AND EVERYTHING IT ASKED ABOUT. The mutations and the
	// queries are recorded separately by this fake, and the bug this test exists
	// for lives in the queries: a bystander snapshot of a service that does not
	// exist finds nothing and is read as "nothing was disturbed".
	joined := strings.Join(append(append([]string{}, f.trace...), f.asked...), " → ")

	// NOT ONE systemd NAME, anywhere. This is the assertion that fails if any
	// call site goes back to the constants.
	for _, systemd := range []string{deploy.ServerUnitName, deploy.NodeUnitName} {
		if strings.Contains(joined, systemd) {
			t.Errorf("the command named %s on a host whose backend does not have it: %s",
				systemd, joined)
		}
	}

	// AND IT REALLY DID THE WORK, rather than passing by doing nothing at all —
	// which is the way an assertion about absence goes vacuous. Each of these is
	// one of the sites that used to hard-code a unit name.
	for _, want := range []string{
		"snapshot " + node,   // the bystander check, taken while starting the server
		"snapshot " + server, // and again while starting the node
		"enabled " + server,  // the enablement comparison reads both services
		"enabled " + node,
		"start " + server,
		"enable " + node,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q is missing, so this test proves less than it looks: %s", want, joined)
		}
	}
}

// AND THE REMEDY IN A REFUSAL IS THE BACKEND'S COMMAND, not systemd's.
//
// A refusal an operator cannot act on is a dead end, and one that names a
// command their machine does not have is worse than none: it sends them to a
// binary that is not there. `systemctl is-enabled` on a Mac is exactly that.
func TestUpRefusesWithTheBackendsOwnCommand(t *testing.T) {
	const server = "sh.example.server"

	cfg := serviceConfig(t)
	stageUp(t, &fakeConverger{
		serverName: server,
		nodeName:   "sh.example.node",
		plan: lifeops.UpPlan{
			ServerState: "/var/lib/billet/server",
			Units:       []lifeops.UnitPlan{{Name: server, Start: true, Enable: true}},
		},
		// Somebody masks it between the plan and the enable, which is the
		// refusal that carries an enablement command.
		enabled: map[string]string{server: "masked"},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a masked service was enabled anyway")
	}

	if !strings.Contains(err.Error(), "some-other-manager is-enabled") {
		t.Errorf("the refusal names a command this host does not have: %v", err)
	}

	if strings.Contains(err.Error(), "systemctl") {
		t.Errorf("the refusal sends the operator to systemctl on a host without it: %v", err)
	}
}

// WHERE A MANAGER CANNOT START A DISABLED SERVICE, THE TWO STEPS SWAP — and
// this is the only test that runs that path.
//
// systemd can start a disabled unit, which is exactly what lets `up` prove a
// service runs BEFORE committing it to every future boot. launchd cannot: a
// label carrying a disabled override refuses to bootstrap at all. So on that
// manager the commitment precedes the proof, and what protects the host is the
// unwinding rather than the ordering.
//
// That is a real weakening of a guarantee, reached by a boolean the backend
// sets, and until now nothing exercised it: `EnableBeforeStart` appeared in two
// branches of startUnits, the field itself, and one launchd plan test asserting
// the flag was SET — never a run that took the branch.
func TestUpEnablesBeforeStartingWhereTheManagerRequiresIt(t *testing.T) {
	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{
			ServerState: "/var/lib/billet/server",
			Units: []lifeops.UnitPlan{{
				Name: deploy.NodeUnitName, Start: true, Enable: true,
				EnableBeforeStart: true,
				Detail:            "will start at login",
			}},
		},
	}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("runLocalUp: %v", err)
	}

	joined := strings.Join(f.trace, " → ")

	enable := strings.Index(joined, "enable "+deploy.NodeUnitName)
	start := strings.Index(joined, "start "+deploy.NodeUnitName)

	switch {
	case enable < 0:
		t.Fatalf("the service was never enabled: %s", joined)
	case start < 0:
		t.Fatalf("the service was never started: %s", joined)
	case enable > start:
		t.Errorf("enable came AFTER start on a manager that cannot start a disabled service, "+
			"so the bootstrap would have been refused: %s", joined)
	}

	// AND ONLY ONCE. The branch that enables early has to skip the ordinary one,
	// or a run commits, starts, and commits again — which on launchd means
	// installing an agent over itself.
	if n := strings.Count(joined, "enable "+deploy.NodeUnitName); n != 1 {
		t.Errorf("enable ran %d times, want once: %s", n, joined)
	}
}

// AND WHEN THE START THEN FAILS, THE COMMITMENT IS TAKEN BACK.
//
// This is the whole reason the inverted order is allowed at all. With the proof
// no longer preceding the commitment, a service that will not run has already
// been committed to the next boot — and the ONLY thing that undoes that is the
// unwinding. If it does not fire, the operator's machine starts a broken service
// at every login and nothing said so.
func TestUpUndoesAnEarlyEnableWhenTheStartFails(t *testing.T) {
	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{
		plan: lifeops.UpPlan{
			ServerState: "/var/lib/billet/server",
			Units: []lifeops.UnitPlan{{
				Name: deploy.NodeUnitName, Start: true, Enable: true,
				EnableBeforeStart: true,
			}},
		},
		startErr: map[string]error{
			deploy.NodeUnitName: errors.New("launchd refused to bootstrap it"),
		},
	}, githubVerified)

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a service that would not start was left committed to boot")
	}

	joined := strings.Join(f.trace, " → ")

	if !strings.Contains(joined, "disable "+deploy.NodeUnitName) {
		t.Errorf("the early enable was not undone, so this host starts a service nothing "+
			"proved at every login: %s", joined)
	}

	// THE UNDO COMES AFTER THE FAILED START, not instead of it.
	if strings.Index(joined, "disable ") < strings.Index(joined, "start ") {
		t.Errorf("the undo ran before the start it was undoing: %s", joined)
	}
}

// AND THE BACKEND'S OWN ACCOUNT OF WHAT IT PROVED IS PRINTED VERBATIM.
//
// systemd's units are Type=notify, so a successful start means billet's own
// process reached READY=1. launchd has no readiness notification at all, and
// saying "ready" there would tell an operator something nothing checked.
func TestUpPrintsTheBackendsOwnProof(t *testing.T) {
	const proof = "the same process is still running after the settle window — launchd has " +
		"no readiness notification"

	cfg := serviceConfig(t)
	stageUp(t, &fakeConverger{plan: bothUnits(), startProof: proof}, githubVerified)

	out := capture(t, func() {
		if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
			t.Fatalf("runLocalUp: %v", err)
		}
	})

	if !strings.Contains(out, proof) {
		t.Errorf("the backend's proof was not printed:\n%s", out)
	}

	// AND THE OTHER MANAGER'S SENTENCE IS NOT.
	if strings.Contains(out, "ready, and still running") {
		t.Errorf("a readiness this backend never established was reported:\n%s", out)
	}
}
