package actions_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// THE ACTION OWNS THE CONVERGE GUARD FOR THE LIFE OF ITS RUN.
//
// The role holds every host under a name and deliberately never releases: the
// name is what a takeover, a release and a recovery are addressed to, and the
// role cannot know when the converge driving it has ended. Its default says the
// action supplies that name — and until #147 the action supplied none, so a
// converge from CI refused before it classified anything, and any guard a run
// did take would have stood until a person removed it.
//
// These execute the scripts against the fixture's fakes, as the rest of this
// file does.

// prepareRun is one invocation of prepare.sh.
type prepareRun struct {
	mode, reach, repository, runID, attempt, holder string
}

func (f *convergeFixture) prepare(t *testing.T, r prepareRun) (string, string, error) {
	t.Helper()

	if r.mode == "" {
		r.mode = "converge"
	}

	if r.repository == "" {
		r.repository = "acme/platform"
	}

	if r.runID == "" {
		r.runID = "17"
	}

	if r.attempt == "" {
		r.attempt = "2"
	}

	githubEnv := filepath.Join(f.dir, "github-env")
	if err := os.WriteFile(githubEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "prepare.sh"))
	cmd.Env = []string{
		"PATH=" + f.bin + ":" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"RUNNER_TEMP=" + f.runnerTm,
		"GITHUB_OUTPUT=" + f.output,
		"GITHUB_ENV=" + githubEnv,
		"GITHUB_REPOSITORY=" + r.repository,
		"GITHUB_RUN_ID=" + r.runID,
		"GITHUB_RUN_ATTEMPT=" + r.attempt,
		"BILLET_MODE=" + r.mode,
		"BILLET_REACH=" + r.reach,
		"BILLET_ACTION_REF=v9.9.9",
		"BILLET_CONVERGE_GUARD_HOLDER=" + r.holder,
	}

	out, err := cmd.CombinedOutput()

	written, readErr := os.ReadFile(githubEnv)
	if readErr != nil {
		t.Fatalf("read GITHUB_ENV: %v", readErr)
	}

	return string(out), string(written), err
}

// holderFrom reads the holder out of what a script wrote to GITHUB_ENV.
func holderFrom(t *testing.T, env string) string {
	t.Helper()

	for _, line := range strings.Split(env, "\n") {
		if rest, ok := strings.CutPrefix(line, "BILLET_CONVERGE_GUARD_HOLDER="); ok {
			return rest
		}
	}

	return ""
}

// A RUN NAMES ITSELF, and the name is legal for the command that records it.
//
// `checkHolder` refuses a holder carrying whitespace, a control character or a
// SLASH, and `owner/repo` is the obvious thing to build a name from, so the one
// that reads best is also the one that would be refused on every host after the
// enrolment and the install had already happened.
func TestTheActionNamesTheGuardAfterItsOwnRun(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)

	out, env, err := f.prepare(t, prepareRun{repository: "acme/platform", runID: "17", attempt: "2"})
	if err != nil {
		t.Fatalf("prepare.sh: %v\n%s", err, out)
	}

	holder := holderFrom(t, env)
	if holder == "" {
		t.Fatalf("prepare.sh exported no BILLET_CONVERGE_GUARD_HOLDER, so the role has no name to "+
			"hold under and a converge refuses before it classifies anything:\n%s", env)
	}

	for _, r := range holder {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' {
			t.Fatalf("holder %q carries %q, which `converge-guard` refuses", holder, r)
		}
	}

	// THE RUN, NOT THE REPOSITORY ALONE: two runs of one workflow must not share
	// a name, or the second would read the first's guard as its own and release
	// it while it is still converging. The attempt is in it for the same reason.
	for _, want := range []string{"17", "2"} {
		if !strings.Contains(holder, want) {
			t.Errorf("holder %q does not name the run (%s); two runs would share a guard", holder, want)
		}
	}

	if !strings.Contains(holder, "platform") {
		t.Errorf("holder %q does not name the repository, so a stuck guard says nothing about "+
			"what left it", holder)
	}
}

// AN OPERATOR'S OWN NAME WINS. A person converging one host from a laptop
// exports the name they will release under; a run that overwrote it would hold
// the fleet under a name that person never sees.
func TestAnExportedHolderIsKept(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)

	out, env, err := f.prepare(t, prepareRun{holder: "junior-laptop"})
	if err != nil {
		t.Fatalf("prepare.sh: %v\n%s", err, out)
	}

	if got := holderFrom(t, env); got != "" && got != "junior-laptop" {
		t.Errorf("prepare.sh replaced the exported holder with %q", got)
	}
}

// THE HOLDER REACHES ANSIBLE, which is the only thing the role can read. The
// role takes it from the controller's environment (`lookup('env', ...)`), so a
// name that never leaves the step is a name no host is held under.
func TestTheHolderReachesTheAnsibleChild(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{holder: "gha-acme-platform-17-2"})
	if err != nil {
		t.Fatalf("converge.sh: %v\n%s", err, out)
	}

	if got := recordedEnv(t, f.pbEnv)["BILLET_CONVERGE_GUARD_HOLDER"]; got != "gha-acme-platform-17-2" {
		t.Errorf("ansible-playbook inherited BILLET_CONVERGE_GUARD_HOLDER=%q, so the role holds "+
			"every host under that name and not this run's", got)
	}
}

// releaseRun is one invocation of release-guard.sh.
type releaseRun struct {
	inventory, limit, holder string
	held                     bool
}

func (f *convergeFixture) release(t *testing.T, r releaseRun) (string, error) {
	t.Helper()

	if r.inventory == "" {
		r.inventory = filepath.Join(f.dir, "inventory.yml")
		if err := os.WriteFile(r.inventory, []byte("all:\n  hosts:\n    cp-1: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if r.holder == "" {
		r.holder = "gha-acme-platform-17-2"
	}

	marker := filepath.Join(f.runnerTm, "billet-guard-held")
	if r.held {
		if err := os.WriteFile(marker, []byte(r.holder+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = os.Remove(marker)
	}

	_ = os.Remove(f.calls)
	_ = os.Remove(f.pbArgs)

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "release-guard.sh"))
	cmd.Env = []string{
		"PATH=" + f.bin + ":" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"RUNNER_TEMP=" + f.runnerTm,
		"GITHUB_ACTION_PATH=" + actionDir(t),
		"BILLET_FAKE_CALLS=" + f.calls,
		"BILLET_FAKE_PB_ARGS=" + f.pbArgs,
		"BILLET_FAKE_PB_ENV=" + f.pbEnv,
		"BILLET_FAKE_PASS=" + filepath.Join(f.dir, "pass"),
		"BILLET_FAKE_LIST_HOSTS=" + filepath.Join(f.dir, "list-hosts"),
		"BILLET_FAKE_RECAP_1=" + filepath.Join(f.dir, "recap1"),
		"BILLET_FAKE_RECAP_2=" + filepath.Join(f.dir, "recap2"),
		"BILLET_INVENTORY=" + r.inventory,
		"BILLET_LIMIT=" + r.limit,
		"BILLET_CONVERGE_GUARD_HOLDER=" + r.holder,
	}

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// THE GUARD IS RELEASED WHEN THE RUN ENDS, over the same hosts it was held on.
func TestTheRunReleasesTheGuardItHeld(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)
	for _, name := range []string{"recap1", "recap2", "list-hosts"} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(cleanRecap("cp-1")), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := f.release(t, releaseRun{held: true, limit: "cp-1"})
	if err != nil {
		t.Fatalf("release-guard.sh: %v\n%s", err, out)
	}

	args := strings.Join(f.callsOf(t, "ansible-playbook"), "\n")
	if !strings.Contains(args, "junioryono.billet.release_guard") {
		t.Fatalf("the release step ran no release playbook, so every host stays held until a "+
			"person removes the guard:\n%s\n%s", args, out)
	}

	// THE SAME HOSTS. A release that walked the whole inventory would refuse on
	// every host this run never held, and report a failure nobody can act on.
	if !strings.Contains(args, "--limit cp-1") {
		t.Errorf("the release did not carry the run's --limit:\n%s", args)
	}

	if got := recordedEnv(t, f.pbEnv)["BILLET_CONVERGE_GUARD_HOLDER"]; got != "gha-acme-platform-17-2" {
		t.Errorf("the release carried holder %q, so it cannot tell its own guard from another's", got)
	}
}

// AND NOTHING IS RELEASED WHEN NOTHING WAS HELD.
//
// The step runs on every path, including one where the converge never reached
// the hosts — a refused mode, a failed WARP enrolment, an install that could not
// finish. Running a playbook then would fail the job on a fleet this run never
// touched, which is a red build that means nothing.
func TestAReleaseIsNotAttemptedWhenTheRunHeldNothing(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)

	out, err := f.release(t, releaseRun{held: false})
	if err != nil {
		t.Fatalf("release-guard.sh: %v\n%s", err, out)
	}

	if calls := f.callsOf(t, "ansible-playbook"); len(calls) != 0 {
		t.Errorf("the release ran %v although this run held nothing", calls)
	}
}

// THE PLAYBOOK THE SCRIPT CALLS IS THE ONE THE COLLECTION SHIPS.
//
// Ansible resolves `junioryono.billet.release_guard` to
// playbooks/release_guard.yml at run time, so a rename on either side is a
// failure that appears only on a real fleet, at the end of a job that has
// already done its work \u2014 the moment when a red step is least likely to be
// read as "every host is still held".
func TestTheReleasePlaybookTheScriptNamesExists(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(filepath.Join("converge-fleet", "release-guard.sh"))
	if err != nil {
		t.Fatal(err)
	}

	const named = "junioryono.billet.release_guard"

	if !strings.Contains(string(script), named) {
		t.Fatalf("release-guard.sh does not run %s", named)
	}

	playbook := filepath.Join("..", "ansible_collections", "junioryono", "billet",
		"playbooks", "release_guard.yml")
	if _, err := os.Stat(playbook); err != nil {
		t.Fatalf("the collection ships no %s for %s: %v", playbook, named, err)
	}
}

// A CHECK HOLDS NOTHING, SO IT RELEASES NOTHING.
//
// The role takes no guard in check mode — its holder is not even required
// there — so a dry run has nothing to release, and a release play run anyway
// reaches every host to ask a question whose answer is always "nothing held".
// MEASURED: the first dry run of a consumer's fleet failed on exactly that. One
// host was unreachable, the release could not ask it, and the run went red at
// the very end over a guard that was never taken. A check that cannot fail for
// what it did not do is the whole point of having one.
func TestACheckRunLeavesNoGuardMarker(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{mode: "check", holder: "gha-acme-platform-17-2"})
	if err != nil {
		t.Fatalf("converge.sh in check mode: %v\n%s", err, out)
	}

	marker := filepath.Join(f.runnerTm, "billet-guard-held")
	if _, err := os.Stat(marker); err == nil {
		t.Error("a check run left the guard marker, so the release step will walk the fleet " +
			"to release a guard no host was ever held under")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat the marker: %v", err)
	}
}

// AND A CONVERGE LEAVES ONE, which is the other half: the marker is what the
// release step reads, so a converge that wrote none would hold every host and
// release nothing.
func TestAConvergeRunLeavesTheGuardMarker(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{mode: "converge", holder: "gha-acme-platform-17-2"})
	if err != nil {
		t.Fatalf("converge.sh: %v\n%s", err, out)
	}

	body, err := os.ReadFile(filepath.Join(f.runnerTm, "billet-guard-held"))
	if err != nil {
		t.Fatalf("a converge left no guard marker, so nothing releases what it held: %v", err)
	}

	if got := strings.TrimSpace(string(body)); got != "gha-acme-platform-17-2" {
		t.Errorf("the marker names %q, want the holder this run converged under", got)
	}
}
