package launchd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A CHILD IN ITS OWN SESSION OUTLIVES THE AGENT THAT STARTED IT, ACROSS A
// BOOTOUT.
//
// This is the fact the whole Mac upgrade rests on. The node agent starts
// `billet host-upgrade` detached with Setsid, exactly as it starts a `tart run`
// guest, and the first thing that updater does is boot the node agent out. If
// launchd took the child with the agent, every Mac upgrade would kill itself
// the moment it began — after the ack, with the node stopped — and read as a
// host that went quiet. Measured here rather than assumed from the guest case,
// because the guest case is measured on a node that was KILLED, not booted out.
func TestASetsidChildSurvivesItsAgentsBootout(t *testing.T) {
	c, label := realLaunchd(t)

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	// THE AGENT SPAWNS THE CHILD THE WAY THE NODE DOES: a Go process with the
	// Setsid attribute, which is this very test binary re-entered as a helper.
	// A shell `nohup` or `&` would measure a different mechanism.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// THE AGENT ANNOUNCES ITSELF THE WAY EVERY REAL FIXTURE HERE DOES, so the
	// shared bootstrap helper waits for the script rather than for launchd's
	// bookkeeping about a pid.
	script := filepath.Join(dir, "agent.sh")
	body := "#!/bin/sh\n" +
		"echo \"start pid=$$\" >> " + filepath.Join(dir, "agent.log") + "\n" +
		"BILLET_SETSID_HELPER_PID_FILE=" + pidFile + " " +
		self + " -test.run '^TestSetsidHelperSpawnsAndExits$' >/dev/null 2>&1\n" +
		"while :; do sleep 1 & wait $!; done\n"

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the agent script: %v", err)
	}

	plist := filepath.Join(dir, label+".plist")
	if err := os.WriteFile(plist, []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key><array><string>/bin/sh</string><string>%s</string></array>
    <key>RunAtLoad</key><true/>
    <key>ExitTimeOut</key><integer>30</integer>
</dict>
</plist>
`, label, script)), 0o600); err != nil {
		t.Fatalf("write the plist: %v", err)
	}

	bootstrap(t, c, label, plist)

	child := waitForPidFile(t, pidFile)

	t.Cleanup(func() {
		// THE CHILD IS THIS TEST'S TO END; a kill that fails because it is already
		// gone is the only failure worth nothing, and it is logged rather than
		// asserted because the measurement above is what this test is about.
		if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
			t.Logf("cleanup: kill the detached child %d: %v", child, err)
		}
	})

	if !processAlive(child) {
		t.Fatalf("the detached child %d was not alive before the bootout, so nothing here "+
			"can be measured", child)
	}

	if _, code, err := c.run(t.Context(), []string{"bootout", c.target(label)}); err != nil ||
		code != 0 {
		t.Fatalf("bootout: exit %d %v", code, err)
	}

	waitGone(t, c, label)

	// THE AGENT IS GONE AND THE CHILD IS NOT, which is the measurement. Polled
	// briefly rather than sampled once, because a launchd that reaped the child
	// would do so moments after the agent, not in the same instant.
	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		if !processAlive(child) {
			t.Fatalf("the detached child %d died with its agent's bootout; a Mac upgrade "+
				"started by the node would kill itself the moment it booted the node out",
				child)
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Logf("the setsid child %d outlived its agent's bootout", child)
}

// TestSetsidHelperSpawnsAndExits is the helper the agent above runs. It starts
// a long sleep in its own session, writes the sleep's pid, and exits — which is
// the node's shape: the updater outlives the process that started it.
func TestSetsidHelperSpawnsAndExits(t *testing.T) {
	pidFile := os.Getenv("BILLET_SETSID_HELPER_PID_FILE")
	if pidFile == "" {
		t.Skip("not running as the setsid helper")
	}

	cmd := exec.Command("/bin/sleep", "600") //nolint:noctx // the child must outlive this process
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the detached child: %v", err)
	}

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write the child's pid: %v", err)
	}

	// NOT WAITED FOR, deliberately: the helper exits and the child stays.
	if err := cmd.Process.Release(); err != nil {
		t.Fatalf("release the detached child: %v", err)
	}
}

func waitForPidFile(t *testing.T, path string) int {
	t.Helper()

	for range 100 {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
			if err != nil {
				t.Fatalf("the pid file holds %q", body)
			}

			return pid
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("the agent never wrote %s", path)

	return 0
}

// A ONESHOT WITH A StartInterval LOADS INTO THE DOMAIN, RUNS AT LOAD, AND STAYS
// LOADED AFTER ITS PROGRAM EXITS.
//
// This is what makes the scheduled agents a schedule rather than a single run:
// a plist with no KeepAlive is not restarted when its program exits 0, and a
// label that left the domain on exit would run once at login and never again.
// Measured because `up` proves a scheduled agent by asking launchd whether it
// still holds the job after loading it, and that question is only meaningful if
// this holds.
func TestAnIntervalAgentRunsAtLoadAndStaysLoaded(t *testing.T) {
	c, label := realLaunchd(t)

	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	script := filepath.Join(dir, "oneshot.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ran >> "+log+"\nexit 0\n"),
		0o700); err != nil {
		t.Fatalf("write the oneshot: %v", err)
	}

	plist := filepath.Join(dir, label+".plist")
	if err := os.WriteFile(plist, []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key><array><string>/bin/sh</string><string>%s</string></array>
    <key>RunAtLoad</key><true/>
    <key>StartInterval</key><integer>3600</integer>
</dict>
</plist>
`, label, script)), 0o600); err != nil {
		t.Fatalf("write the plist: %v", err)
	}

	if _, code, err := c.run(t.Context(), []string{"bootstrap", c.domain(), plist}); err != nil ||
		code != 0 {
		t.Fatalf("bootstrap: exit %d %v", code, err)
	}

	t.Cleanup(func() {
		//nolint:usetesting // t.Context() is already cancelled by the time cleanups run
		if _, _, err := c.run(context.Background(), []string{"bootout", c.target(label)}); err != nil {
			t.Errorf("cleanup: bootout %s: %v", c.target(label), err)
		}
	})

	ran := false

	for range 100 {
		if body, err := os.ReadFile(log); err == nil && strings.Contains(string(body), "ran") {
			ran = true

			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	if !ran {
		t.Fatal("the oneshot never ran at load")
	}

	// STILL LOADED, WITH NO PROCESS: launchd holds the schedule.
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		job, loaded, err := c.job(t.Context(), label)
		if err != nil {
			t.Fatalf("job: %v", err)
		}

		if !loaded {
			t.Fatal("the oneshot left the domain after exiting, so its schedule is gone")
		}

		if !job.Running() {
			t.Logf("the oneshot ran, exited and stays loaded on its interval")

			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatal("the oneshot's process never exited")
}
