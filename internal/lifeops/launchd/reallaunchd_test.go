package launchd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/lifeops"
)

// Real launch agents, bootstrapped into this user's own GUI domain.
//
// THE STUB TESTS ASSERT WHAT BILLET SAYS; THESE ASSERT THAT WHAT IT SAYS MATCHES
// WHAT launchd DOES. Every rule this package is built on came from running these
// rather than from launchd.plist(5), which is wrong about at least one of them —
// and each one, read the other way, produces a node SIGKILLed mid-drain or a
// host reported down with a job still running on it.
//
// They use labels billet never ships, clean up after themselves, and skip
// anywhere launchctl is not the macOS one.
//
// EACH TEST GETS ITS OWN LABEL, derived from its name. Sharing one made every
// test after the first depend on the previous one's agent being gone — and a
// launchd agent is NOT gone when its test ends, because a drain outlives the
// bootout that asked for it. The failures that produced were all of the "this
// test measured the last test's leftovers" kind, which is worse than a plain
// failure because they move around when the order changes.
const labelPrefix = "sh.billet.realtest."

var unsafeInLabel = regexp.MustCompile(`[^A-Za-z0-9]+`)

func realLaunchd(t *testing.T) (*Converger, string) {
	t.Helper()

	label := labelPrefix + unsafeInLabel.ReplaceAllString(t.Name(), "")

	if _, err := exec.LookPath("launchctl"); err != nil {
		t.Skip("launchctl is not installed")
	}

	// Linux has no `launchctl`, and a stray binary by that name is not one.
	help, err := exec.CommandContext(t.Context(), "launchctl", "help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), "bootstrap") {
		t.Skip("launchctl is not the macOS service manager")
	}

	c := New(WithAgentsDir(t.TempDir()))

	// LEFTOVERS ARE REPORTED, not discarded. These run against a real launchd on
	// somebody's own Mac, and a cleanup that quietly failed would leave an agent
	// loaded — which the next run of the same test then has to wait out.
	//
	// NOTHING IS ENABLED HERE. Measured on macOS 26 with two labels that had
	// never existed:
	//
	//	bootstrap + bootout          -> no row in the override database
	//	bootstrap + bootout + enable -> a row, value false, permanently
	//
	// launchd's database has no "forget": `launchctl enable` clears a disable by
	// WRITING the label, so the row outlives the plist, the bootout and the test
	// that made it. A cleanup running it unconditionally therefore added a
	// permanent row to a ROOT-OWNED SYSTEM DATABASE for every test in this file,
	// on every run — eleven had accumulated before anybody looked. The two tests
	// that deliberately disable a label register their own undo, at the point
	// they do it, through disableLabel below.
	//
	// Removing rows afterwards does not work while launchd is running: it holds
	// the map in memory and rewrites the whole file on the next enable or disable
	// of ANY service, which puts every row back. Not creating them is the only
	// remedy that holds.
	t.Cleanup(func() {
		// context.Background() DELIBERATELY, not t.Context(): that one is
		// cancelled just BEFORE cleanups run, so every launchctl call here would
		// fail on a context that ended for the ordinary reason this cleanup
		// exists. Same shape as the rollback in `billet local up`, which runs on
		// a context that outlives the cancellation.
		ctx := context.Background() //nolint:usetesting // t.Context() is already cancelled here

		// A NON-ZERO EXIT IS NOT AN ERROR TO `run`, deliberately — launchctl
		// answers questions with its exit status — so a cleanup checking only
		// err reports nothing when the bootout is refused.
		//
		// `nothingToBoot` is the ordinary case here, not a failure: most of these
		// tests boot their own agent out before they finish. It is 3, where the
		// same "no such service" from `print` is 113 — measured, and assuming one
		// number covered both made this report a leftover agent on every run.
		out, code, err := c.run(ctx, []string{"bootout", c.target(label)})
		if err != nil {
			t.Errorf("cleanup: launchctl bootout %s: %v", c.target(label), err)
		} else if code != 0 && code != nothingToBoot {
			t.Errorf("cleanup: launchctl bootout %s exited %d (%s); the agent may still be "+
				"loaded", c.target(label), code, strings.TrimSpace(out))
		}
	})

	return c, label
}

// disableLabel writes a disabled override AND registers the undo, in one place.
//
// THE UNDO BELONGS WHERE THE MUTATION IS, which is the only way it can be sure
// what to undo. An earlier version put it in the shared cleanup and worked out
// afterwards, from the database, whether an override was there to clear —
// which meant a read that failed, or a parse this build could not manage, left
// the label DISABLED. The next run of that same test then failed at bootstrap
// with launchd's `5: Input/output error`, which says nothing about an override,
// and a `t.Logf` nobody sees without -v was the only trace.
//
// It also keeps the row count honest: only a test that actually disabled
// something ever calls `enable`, and `enable` is what creates a permanent row.
func disableLabel(t *testing.T, c *Converger, label string) {
	t.Helper()

	if _, code, err := c.run(t.Context(), []string{"disable", c.target(label)}); err != nil ||
		code != 0 {
		t.Fatalf("launchctl disable %s: exit %d %v", c.target(label), code, err)
	}

	t.Cleanup(func() {
		//nolint:usetesting // t.Context() is already cancelled by the time cleanups run
		out, code, err := c.run(context.Background(), []string{"enable", c.target(label)})

		switch {
		case err != nil:
			t.Errorf("cleanup: launchctl enable %s: %v — the label is left DISABLED, and the "+
				"next run of this test will fail to bootstrap with launchd's uninformative "+
				"exit 5", c.target(label), err)
		case code != 0:
			t.Errorf("cleanup: launchctl enable %s exited %d (%s) — the label is left "+
				"DISABLED, and the next run of this test will fail to bootstrap with "+
				"launchd's uninformative exit 5", c.target(label), code, strings.TrimSpace(out))
		}
	})
}

// writeAgent writes an agent that announces its pid and, on SIGTERM, drains for
// drain seconds before exiting 0.
//
// `wait` is what makes the trap fire promptly: a foreground `sleep` would not
// run the handler until it returned, and the agent would look like one that
// ignores SIGTERM.
func writeAgent(t *testing.T, label, dir string, exitTimeout, drain int) string {
	t.Helper()

	script := filepath.Join(dir, "agent.sh")
	log := filepath.Join(dir, "agent.log")

	body := "#!/bin/sh\n" +
		"echo \"start pid=$$\" >> " + log + "\n" +
		"term() { echo term >> " + log + "; sleep " + fmt.Sprint(drain) + "; " +
		"echo exit >> " + log + "; exit 0; }\n" +
		"trap term TERM\n" +
		"while :; do sleep 1 & wait $!; done\n"

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the agent script: %v", err)
	}

	// A ZERO exitTimeout OMITS THE KEY, so launchd's own default can be measured
	// with the same agent every other test uses. Writing a second, simpler agent
	// for that measurement is what made it the one test whose fixture had no log
	// to wait for.
	timeout := ""
	if exitTimeout > 0 {
		timeout = fmt.Sprintf("    <key>ExitTimeOut</key><integer>%d</integer>\n", exitTimeout)
	}

	plist := filepath.Join(dir, label+".plist")
	if err := os.WriteFile(plist, []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key><array><string>/bin/sh</string><string>%s</string></array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
%s</dict>
</plist>
`, label, script, timeout)), 0o600); err != nil {
		t.Fatalf("write the plist: %v", err)
	}

	return plist
}

// waitGone waits until launchd no longer has the label at all.
//
// A BOOTOUT IS A REQUEST, NOT A COMPLETION — measured, it returns while the
// process is still draining and the service is still in the domain. A test that
// bootstraps straight afterwards gets `Bootstrap failed: 5`, which is the same
// error launchd gives for a DISABLED label — so a test about disabling would
// pass for entirely the wrong reason.
func waitGone(t *testing.T, c *Converger, label string) {
	t.Helper()

	for range 100 {
		if _, loaded, err := c.job(t.Context(), label); err == nil && !loaded {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("%s was still in its domain after 10s", label)
}

// agentLog reads what the test agent wrote about itself, which is the only
// account of what actually happened inside it.
func agentLog(t *testing.T, dir string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(dir, "agent.log"))
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}

	return strings.TrimSpace(string(body))
}

func bootstrap(t *testing.T, c *Converger, label, plist string) Job {
	t.Helper()

	if _, _, err := c.run(t.Context(), []string{"bootout", c.target(label)}); err != nil {
		t.Logf("clearing %s before bootstrapping it: %v", label, err)
	}

	// A bootout is not instant, and the label cannot be re-claimed while its
	// process is still going.
	for range 40 {
		if _, loaded, err := c.job(t.Context(), label); err == nil && !loaded {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	out, code, err := c.run(t.Context(), []string{"bootstrap", c.domain(), plist})
	if err != nil || code != 0 {
		t.Fatalf("bootstrap: exit %d %v: %s", code, err, out)
	}

	// THE LAST ANSWER IS KEPT, so a failure here says what launchd was actually
	// reporting. "The agent never started" with nothing behind it is a message
	// that sends the next person to the wrong place.
	var (
		last Job
		seen bool
		why  error
	)

	dir := filepath.Dir(plist)

	for range 40 {
		job, loaded, err := c.job(t.Context(), label)

		// A PID IS NOT A RUNNING FIXTURE. launchd reports one for a process it
		// has just spawned, and a script that dies immediately — unreadable,
		// wrong interpreter, a directory nothing can reach — is respawned by
		// KeepAlive, so `print` shows a pid over and over for an agent that has
		// never executed a line. A test built on that measures respawn timing
		// and calls it a drain.
		//
		// The agent announces itself in its own log, so waiting for THAT is
		// waiting for the fixture rather than for launchd's bookkeeping.
		if err == nil && loaded && job.Running() && strings.Contains(agentLog(t, dir), "start") {
			return job
		}

		last, seen, why = job, loaded, err

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("the agent never started: loaded=%v err=%v state=%q pid=%d (known %v) lastExit=%d",
		seen, why, last.State, last.PID, last.PIDKnown, last.LastExit)

	return Job{}
}

// A LOADED JOB IS NOT ITS PLIST, and this is the measurement the whole design
// turns on.
//
// launchd reads a plist ONCE, at bootstrap, and keeps what it read. So a node
// can be running a stale ExitTimeOut while the file on disk is byte-identical to
// the one billet ships — and `up` comparing the FILE would certify it. At the
// next logout launchd SIGKILLs that node five seconds into a drain it was
// allowed 88200 for, leaving guests running with their leases renewed by nobody.
//
// `launchctl print` reports the LOADED values, which is what makes the loaded
// job checkable at all.
func TestARealLoadedJobKeepsItsPropertiesWhenThePlistIsReplaced(t *testing.T) {
	c, label := realLaunchd(t)
	dir := t.TempDir()

	plist := writeAgent(t, label, dir, 20, 1)
	before := bootstrap(t, c, label, plist)

	if !before.ExitTimeoutKnown || before.ExitTimeout != 20 {
		t.Fatalf("ExitTimeout = %d (known %v), want the 20 the plist declared",
			before.ExitTimeout, before.ExitTimeoutKnown)
	}

	// The same label, a different timeout, written over the file launchd loaded.
	writeAgent(t, label, dir, 99, 1)

	after, loaded, err := c.job(t.Context(), label)
	if err != nil || !loaded {
		t.Fatalf("job after replacing the plist: loaded=%v err=%v", loaded, err)
	}

	if after.ExitTimeout != 20 {
		t.Errorf("ExitTimeout = %d after the plist was replaced; if launchd had re-read the "+
			"file this measurement would be 99 and the design that rests on it would be "+
			"unnecessary", after.ExitTimeout)
	}

	if after.PID != before.PID {
		t.Errorf("the job restarted on its own (%d -> %d), so this measured something else",
			before.PID, after.PID)
	}
}

// A PLAIN bootout RETURNS WHILE THE PROCESS IS STILL DRAINING.
//
// Zero seconds, measured. The service stays in the domain reporting `state =
// SIGTERMed` WITH ITS PID, and the record disappears only when the process
// finally exits. So neither the command's return nor `state` is a stop: billet's
// proof has to be the process itself.
func TestARealBootoutReturnsBeforeTheProcessIsGone(t *testing.T) {
	c, label := realLaunchd(t)

	plist := writeAgent(t, label, t.TempDir(), 120, 6)
	job := bootstrap(t, c, label, plist)

	start := time.Now()
	if _, code, err := c.run(t.Context(), []string{"bootout", c.target(label)}); err != nil ||
		code != 0 {
		t.Fatalf("bootout: exit %d %v", code, err)
	}

	elapsed := time.Since(start)

	// It returned long before the six-second drain could have finished.
	if elapsed > 3*time.Second {
		t.Fatalf("bootout took %s against a 6s drain, so it waited and this measures nothing",
			elapsed)
	}

	// AND THE PROCESS IS STILL THERE, which is the point.
	if !processAlive(job.PID) {
		t.Fatalf("pid %d was already gone %s after bootout returned; the drain should still "+
			"have been running. The agent's own log says: %q",
			job.PID, elapsed, agentLog(t, filepath.Dir(plist)))
	}

	// The service is still in the domain, and launchd still names the pid.
	during, loaded, err := c.job(t.Context(), label)
	if err != nil {
		t.Fatalf("job during the drain: %v", err)
	}

	if !loaded {
		raw, code, runErr := c.run(t.Context(), []string{"print", c.target(label)})
		t.Fatalf("the service left the domain before its process did; the stop proof assumes "+
			"the opposite ordering. print exited %d (err %v) saying %q; pid %d alive=%v",
			code, runErr, strings.TrimSpace(raw), job.PID, processAlive(job.PID))
	}

	if during.State != "SIGTERMed" {
		t.Errorf("state = %q during a drain, want SIGTERMed — the launchd analogue of "+
			"systemd's `deactivating`, which must never be read as stopped", during.State)
	}
}

// AND BILLET'S OWN STOP WAITS FOR IT.
//
// The two facts above, put together: StopAndProve must not come back until the
// process is gone, however long the drain takes.
func TestStopAndProveWaitsOutARealDrain(t *testing.T) {
	c, label := realLaunchd(t)

	plist := writeAgent(t, label, t.TempDir(), 120, 5)
	job := bootstrap(t, c, label, plist)

	start := time.Now()

	got, err := c.StopAndProve(t.Context(), label)
	if err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	elapsed := time.Since(start)

	if got.Gone != lifeops.Yes {
		t.Errorf("Gone = %v, want yes", got.Gone)
	}

	// IT WAITED. A stop that returned immediately would be reporting launchd's
	// answer rather than the process's.
	if elapsed < 4*time.Second {
		t.Errorf("StopAndProve returned after %s against a 5s drain, so it did not wait for "+
			"the process", elapsed)
	}

	if processAlive(job.PID) {
		t.Errorf("pid %d is still alive after a stop reported gone", job.PID)
	}
}

// ExitTimeOut IS REAL, AND launchd's DEFAULT IS FIVE SECONDS.
//
// The second half is the one that was wrong in billet's own shipped comments,
// which said twenty on the strength of the man page. Measured here instead: an
// agent that declares none reports `exit timeout = 5`. Five seconds into a
// drain is nowhere, which is why both plists set it explicitly.
func TestARealAgentsDefaultExitTimeoutIsFiveSeconds(t *testing.T) {
	c, label := realLaunchd(t)

	// Zero omits the key, so this is launchd's own default rather than a number
	// billet chose.
	job := bootstrap(t, c, label, writeAgent(t, label, t.TempDir(), 0, 1))

	if !job.ExitTimeoutKnown {
		t.Fatal("launchctl reported no exit timeout at all")
	}

	if job.ExitTimeout != 5 {
		t.Errorf("the default ExitTimeOut is %d, and billet's plists, their test and "+
			"docs/reference/reference-hardware.md are all written around it being 5; "+
			"re-measure and "+
			"update them together", job.ExitTimeout)
	}
}

// THE DISABLED-OVERRIDE DATABASE OUTLIVES EVERYTHING BILLET INSTALLS.
//
// Durable, keyed by LABEL, and it survives both a bootout and the removal of the
// plist. That is why an uninstall must clear it: a label left disabled with no
// plist to explain it means a later reinstall bootstraps a service launchd
// silently refuses to run.
func TestTheRealOverrideDatabaseSurvivesBootoutAndPlistRemoval(t *testing.T) {
	c, label := realLaunchd(t)
	dir := t.TempDir()

	plist := writeAgent(t, label, dir, 20, 1)
	bootstrap(t, c, label, plist)

	// launchctl DIRECTLY, not billet's Enable/Disable: what this measures is the
	// database's own behaviour, and billet's verbs do more than touch it -- Enable
	// installs the agent billet ships, which a label invented by a test has none
	// of. Measuring through them would test the wrong thing.
	disableLabel(t, c, label)

	if _, _, err := c.run(t.Context(), []string{"bootout", c.target(label)}); err != nil {
		t.Fatalf("bootout: %v", err)
	}

	if err := os.Remove(plist); err != nil {
		t.Fatalf("remove the plist: %v", err)
	}

	disabled, err := c.disabledLabels(t.Context())
	if err != nil {
		t.Fatalf("disabledLabels: %v", err)
	}

	if !disabled[label] {
		t.Error("the override did not survive the bootout and the plist removal; if that is " +
			"now true, the uninstall's ordering can be simplified")
	}

	// AND `enable` CLEARS IT, which is what uninstall relies on.
	if _, code, err := c.run(t.Context(), []string{"enable", c.target(label)}); err != nil ||
		code != 0 {
		t.Fatalf("launchctl enable: exit %d %v", code, err)
	}

	cleared, err := c.disabledLabels(t.Context())
	if err != nil {
		t.Fatalf("disabledLabels: %v", err)
	}

	if cleared[label] {
		t.Error("`launchctl enable` did not clear the override, so an uninstall cannot " +
			"remove the landmine it leaves")
	}
}

// A DISABLED LABEL REFUSES TO BOOTSTRAP, with an error that says nothing.
//
// `Bootstrap failed: 5: Input/output error` is what launchd returns for a
// disabled label, for one that is already loaded, and for one still draining.
// Three different situations, one useless message — which is why billet
// diagnoses a failed bootstrap by RE-READING rather than by interpreting it.
func TestARealDisabledLabelRefusesToBootstrap(t *testing.T) {
	c, label := realLaunchd(t)

	plist := writeAgent(t, label, t.TempDir(), 20, 1)
	bootstrap(t, c, label, plist)

	if _, _, err := c.run(t.Context(), []string{"bootout", c.target(label)}); err != nil {
		t.Fatalf("bootout: %v", err)
	}

	// THE LABEL MUST BE FREE BEFORE THIS MEANS ANYTHING. A still-draining label
	// refuses a bootstrap with the SAME `5: Input/output error` a disabled one
	// does, so without this the test passes whether or not disabling does
	// anything at all.
	waitGone(t, c, label)

	disableLabel(t, c, label)

	_, code, err := c.run(t.Context(), []string{"bootstrap", c.domain(), plist})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	if code == 0 {
		t.Fatal("a disabled label bootstrapped, so billet need not read the override database")
	}

	// AND IT REALLY DID NOT START, which is the half that would otherwise be
	// silent.
	if _, loaded, err := c.job(t.Context(), label); err == nil && loaded {
		t.Error("the bootstrap failed and the service loaded anyway")
	}
}

// AN ORDINARY BOOTSTRAP AND BOOTOUT LEAVE NO ROW BEHIND.
//
// This is the measurement the whole cleanup rests on, and it was made by hand
// before it was a test. launchd's override database has no "forget": `launchctl
// enable` clears a disable by WRITING the label, so a row created by a test is
// permanent — in a ROOT-OWNED system database on somebody's own Mac. Eleven had
// accumulated here from a cleanup that ran `enable` whether or not anything had
// been disabled.
//
// MEMBERSHIP, NOT VALUE. disabledLabels answers false both for a label that is
// absent and for one present with the value `enabled`, and the difference is the
// entire point: the second is the litter.
func TestARealBootstrapAndBootoutLeaveNoOverrideRow(t *testing.T) {
	c, label := realLaunchd(t)

	before, err := c.disabledLabels(t.Context())
	if err != nil {
		t.Fatalf("disabledLabels: %v", err)
	}

	if _, exists := before[label]; exists {
		t.Skipf("%s already has a row from an earlier run, so this cannot tell whether THIS "+
			"run creates one", label)
	}

	bootstrap(t, c, label, writeAgent(t, label, t.TempDir(), 20, 1))

	if _, _, err := c.run(t.Context(), []string{"bootout", c.target(label)}); err != nil {
		t.Fatalf("bootout: %v", err)
	}

	waitGone(t, c, label)

	after, err := c.disabledLabels(t.Context())
	if err != nil {
		t.Fatalf("disabledLabels: %v", err)
	}

	if _, exists := after[label]; exists {
		t.Errorf("a bootstrap and bootout left a row for %s in launchd's override database. "+
			"That database is root-owned and has no forget, so every run of every test in "+
			"this file would add one permanently", label)
	}
}
