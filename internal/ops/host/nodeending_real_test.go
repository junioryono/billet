package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/nodeending"
)

// endingStandinEnv makes the test binary the witness's node: it records its
// process in the ending record under systemd's INVOCATION_ID, as app.OpenNode
// does, and on SIGTERM either drains (`drain`, writing its ending) or ignores
// the signal (`ignore`), so systemd kills it at TimeoutStopSec.
const endingStandinEnv = "BILLET_ENDING_NODE_STANDIN"

// endingStandinStateEnv names the state directory the stand-in records in.
const endingStandinStateEnv = "BILLET_ENDING_STATE_DIR"

func runEndingStandin(mode string) int {
	// IGNORED, NOT HANDLED: a later signal.Notify for SIGTERM would undo the
	// Ignore and drain after all, so the ignoring stand-in asks for nothing.
	ctx := context.Background()
	if mode == "ignore" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		var stop context.CancelFunc

		ctx, stop = signal.NotifyContext(ctx, syscall.SIGTERM)
		defer stop()
	}

	h, err := nodeending.Begin(os.Getenv(endingStandinStateEnv), nodeending.Entry{
		InvocationID: os.Getenv("INVOCATION_ID"), PID: os.Getpid(), BootID: nodeending.BootID(),
		Role: nodeending.RoleNode, RunsOnHost: true,
	}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	// SLEEPING, NOT BLOCKED: a goroutine waiting forever on nothing is a
	// deadlock the runtime ends at once, which is a crash, not the kill at
	// TimeoutStopSec this mode stands for.
	for mode == "ignore" {
		time.Sleep(time.Hour)
	}

	<-ctx.Done()

	if err := h.End(nodeending.Drained, false); err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	return 0
}

// TestRealSystemdNodeEndingProvesAStopByItsRecord measures #127 against real
// systemd: a node that drains on SIGTERM is proved; one systemd kills at
// TimeoutStopSec is not, though `systemctl stop` exits 0, and still is not once
// reset-failed has made the triple clean; a start over it is refused, and its
// acknowledgement admits one; and a check of an invocation a later start
// replaced proves nothing about the process the stop ended.
func TestRealSystemdNodeEndingProvesAStopByItsRecord(t *testing.T) {
	if os.Getenv("BILLET_TEST_SYSTEMD_OPERATION_EFFECTS") != "1" {
		t.Skip("requires BILLET_TEST_SYSTEMD_OPERATION_EFFECTS=1 in a disposable systemd host")
	}

	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("the disposable systemd host must run Linux as root")
	}

	if _, err := os.Lstat(repairUnitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists (%v); this witness installs its own and needs a host without billet", repairUnitPath, err)
	}

	version, err := exec.CommandContext(t.Context(), "systemctl", "--version").Output()
	mustOK(t, err)
	t.Logf("systemd measurement %s: %s", time.Now().UTC().Format(time.RFC3339),
		strings.SplitN(string(version), "\n", 2)[0])

	root := fmt.Sprintf("/var/lib/billet-ending-%d-%d", os.Getpid(), time.Now().UnixNano())
	mustOK(t, os.MkdirAll(root, 0o755))

	ctl := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
		defer cancel()

		out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
		}

		return nil
	}

	t.Cleanup(func() {
		if err := ctl("stop", "--", nodeUnit); err != nil {
			t.Error(err)
		}

		if err := ctl("reset-failed", "--", nodeUnit); err != nil {
			t.Logf("reset-failed after the witness: %v", err)
		}

		if err := os.Remove(repairUnitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}

		if err := ctl("daemon-reload"); err != nil {
			t.Error(err)
		}

		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})

	self, err := os.Executable()
	mustOK(t, err)

	body, err := os.ReadFile(self)
	mustOK(t, err)

	standin := filepath.Join(root, "standin")
	writeFile(t, standin, string(body), 0o755)

	stateDir := filepath.Join(root, "node-state")
	mustOK(t, os.MkdirAll(stateDir, 0o750))

	// install renders the witness's node unit in the given mode and loads it.
	install := func(mode string) {
		t.Helper()

		unit := strings.Join([]string{
			"[Unit]", "Description=billet ending witness (#127)", "",
			"[Service]", "Type=exec", "ExecStart=" + standin + " -test.run=^$",
			"Environment=" + endingStandinEnv + "=" + mode, "Environment=" + endingStandinStateEnv + "=" + stateDir,
			"KillMode=mixed", "TimeoutStopSec=2", "",
		}, "\n")
		writeFile(t, repairUnitPath, unit, 0o644)
		mustOK(t, ctl("daemon-reload"))
	}

	insp := lifeops.NewInspector()
	sample := func() nodeending.Unit { return observeNodeUnit(t.Context(), insp) }

	// started starts the unit and answers the invocation systemd gave it, once
	// the stand-in has recorded itself.
	started := func() string {
		t.Helper()

		mustOK(t, ctl("start", "--", nodeUnit))

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			props, err := insp.UnitProperties(t.Context(), nodeUnit, "InvocationID")
			mustOK(t, err)

			inv := firstProp(props, "InvocationID")

			rec, _, err := nodeending.Read(stateDir)
			if err == nil && inv != "" && len(rec.Entries) > 0 && rec.Entries[len(rec.Entries)-1].InvocationID == inv {
				return inv
			}

			time.Sleep(100 * time.Millisecond)
		}

		t.Fatal("the stand-in never recorded itself")

		return ""
	}

	// A DRAIN IS PROVED.
	install("drain")
	inv := started()
	mustOK(t, ctl("stop", "--", nodeUnit))

	if j, _ := nodeending.JudgeStop(stateDir, inv, true, true, sample, nil); j.Verdict != nodeending.Proved {
		t.Fatalf("a drained stop is %s (%s), want proved", j.Answer, j.Why)
	}

	// A STOP KILLED AT THE TIMEOUT: `systemctl stop` exits 0 (measured), and the
	// record says the process never wrote its ending.
	install("ignore")
	inv = started()

	stopAt := time.Now()
	mustOK(t, ctl("stop", "--", nodeUnit))

	// THE KILL, NOT A CRASH: systemd waited out TimeoutStopSec and says so.
	if killed := sample(); killed.ActiveState != "failed" || killed.Result != "timeout" ||
		time.Since(stopAt) < 2*time.Second {
		t.Fatalf("the ignoring stand-in's stop left %+v after %s, want failed with Result=timeout after "+
			"TimeoutStopSec=2", killed, time.Since(stopAt))
	}

	if j, _ := nodeending.JudgeStop(stateDir, inv, false, true, sample, nil); j.Verdict != nodeending.Unproved ||
		!strings.Contains(j.Why, "killed or crashed") {
		t.Fatalf("a stop killed at the timeout is %s (%s), want unproved", j.Answer, j.Why)
	}

	// reset-failed makes systemd's triple clean, and the stop is still unproved.
	mustOK(t, ctl("reset-failed", "--", nodeUnit))

	after := sample()
	if after.ActiveState != "inactive" || after.Result != "success" {
		t.Fatalf("after reset-failed the unit is %+v, want inactive with Result=success", after)
	}

	if j, _ := nodeending.JudgeStop(stateDir, inv, false, true, sample, nil); j.Verdict != nodeending.Unproved {
		t.Fatalf("after reset-failed the killed stop is %s (%s), want still unproved", j.Answer, j.Why)
	}

	// A START OVER IT IS REFUSED, and its acknowledgement admits one.
	if j := nodeending.JudgeStanding(stateDir, false, sample); j.Verdict != nodeending.Unproved {
		t.Fatalf("a start over the killed stop is %s (%s), want refused", j.Answer, j.Why)
	}

	if _, err := nodeending.Acknowledge(stateDir, inv, false, nil); err != nil {
		t.Fatal(err)
	}

	if j := nodeending.JudgeStanding(stateDir, false, sample); j.Verdict != nodeending.Proved {
		t.Fatalf("a start over the acknowledged stop is %s (%s), want admitted", j.Answer, j.Why)
	}

	// A LATER START REPLACES THE INVOCATION: a check of the one before proves
	// nothing about the process a stop then ends.
	install("drain")
	older := inv
	_ = started()
	mustOK(t, ctl("stop", "--", nodeUnit))

	if j, _ := nodeending.JudgeStop(stateDir, older, false, true, sample, nil); j.Verdict != nodeending.Unproved ||
		!strings.Contains(j.Why, "not the newest process") {
		t.Fatalf("a check of a replaced invocation is %s (%s), want unproved", j.Answer, j.Why)
	}
}
