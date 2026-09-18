package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
)

// The real manager supplies activity, process, job, invocation and reload
// observations. Durable retirement and effect-graph evidence use the command
// fixture. This does not claim a complete role or a real retained node workload.
func TestRealSystemdRetirementSettledEntryObservations(t *testing.T) {
	if os.Getenv("BILLET_TEST_SYSTEMD_OPERATION_EFFECTS") != "1" {
		t.Skip("requires a disposable systemd host and PostgreSQL")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || os.Getenv("BILLET_TEST_POSTGRES_DSN") == "" {
		t.Fatal("requires Linux root and BILLET_TEST_POSTGRES_DSN")
	}
	version, err := exec.CommandContext(t.Context(), "/usr/bin/systemctl", "--version").Output()
	mustOK(t, err)
	if !strings.HasPrefix(string(version), "systemd 255 ") && !strings.HasPrefix(string(version), "systemd 255\n") {
		t.Fatalf("this witness requires systemd 255: %s", version)
	}
	t.Logf("systemd measurement %s: %s", time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(string(version)))
	f, journal := settledNodeConfigFixture(t)
	for _, scenario := range []string{"active", "inactive", "failed", "activating", "deactivating", "reloading", "queued job", "unit mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			unit := fmt.Sprintf("billet-entry-%d-%d.service", os.Getpid(), time.Now().UnixNano())
			body := "[Unit]\nDescription=retirement entry observation witness\n[Service]\nType=exec\nExecStart=/bin/sleep 600\nRestart=no\nTimeoutStartSec=300\nTimeoutStopSec=2\n"
			switch scenario {
			case "activating":
				body = strings.Replace(body, "Type=exec", "Type=notify", 1)
			case "deactivating":
				body = strings.Replace(body, "TimeoutStopSec=2", "TimeoutStopSec=300\nExecStop=/bin/sleep 600", 1)
			case "reloading":
				body += "ExecReload=/bin/sleep 600\n"
			case "queued job":
				blocker := strings.TrimSuffix(unit, ".service") + "-blocker.service"
				installSettledObservationUnit(t, blocker, strings.Replace(body, "Type=exec", "Type=notify", 1))
				settledObservationCtl(t, "start", "--no-block", "--", blocker)
				awaitSettledObservation(t, blocker, "ActiveState", "activating")
				body = strings.Replace(body, "[Unit]\n", "[Unit]\nAfter="+blocker+"\n", 1)
			}
			path := installSettledObservationUnit(t, unit, body)
			switch scenario {
			case "active", "failed", "deactivating", "reloading":
				settledObservationCtl(t, "start", "--", unit)
				awaitSettledObservation(t, unit, "ActiveState", "active")
				switch scenario {
				case "failed":
					settledObservationCtl(t, "kill", "--kill-whom=main", "--signal=KILL", "--", unit)
					awaitSettledObservation(t, unit, "ActiveState", "failed")
				case "deactivating":
					settledObservationCtl(t, "stop", "--no-block", "--", unit)
					awaitSettledObservation(t, unit, "ActiveState", "deactivating")
				case "reloading":
					settledObservationCtl(t, "reload", "--no-block", "--", unit)
					awaitSettledObservation(t, unit, "ActiveState", "reloading")
				}
			case "activating", "queued job":
				settledObservationCtl(t, "start", "--no-block", "--", unit)
				if scenario == "activating" {
					awaitSettledObservation(t, unit, "ActiveState", "activating")
				} else {
					awaitSettledObservation(t, unit, "ActiveState", "inactive")
					if settledObservationProperty(t, unit, "Job") == "" {
						t.Fatal("the ordered start did not leave a queued job")
					}
				}
			case "unit mismatch":
				// MEASURED ON SYSTEMD 255, 2026-09-18: a property query does not
				// hold the unit loaded, so changing the fragment after one still
				// answers NeedDaemonReload=no forever, and cleanup reports the
				// unit was never loaded. Starting it loads the original
				// definition and keeps it loaded, and systemd then notices the
				// fragment change. The unit stays running for this case; the
				// entry command's node activity is fixture-owned.
				settledObservationCtl(t, "start", "--", unit)
				awaitSettledObservation(t, unit, "ActiveState", "active")
				awaitSettledObservation(t, unit, "NeedDaemonReload", "no")
				writeFile(t, path, body+"Environment=BILLET_ENTRY_WITNESS=changed\n", 0o644)
				awaitSettledObservation(t, unit, "NeedDaemonReload", "yes")
			case "inactive":
				awaitSettledObservation(t, unit, "ActiveState", "inactive")
			}
			if scenario == "active" {
				invocation := settledObservationProperty(t, unit, "InvocationID")
				if invocation == "" || settledObservationProperty(t, unit, "MainPID") == "0" {
					t.Fatal("active control has no process or invocation")
				}
				// Registration is fixture-owned; only its invocation comes from
				// this independent process. No real registration is claimed here.
				before := mustRead(t, registrationRecordPath)
				t.Cleanup(func() { writeFile(t, registrationRecordPath, before, 0o600) })
				writeRegistrationRecord(t, registrationRecordPath, f.identity, retainedEndpoint, invocation)
			}
			observations := forwardSettledManagerObservations(t, unit)
			forbidSettledWrites(t, f)
			out, code := f.runRaw(t, "", settledCheckArgs(t, f, journal, retirement.PurposeSettledEntry)...)
			switch scenario {
			case "active", "inactive", "failed":
				verdict, err := retirement.DecodeSettledVerdict([]byte(out), code,
					settledExpectation(t, f, journal, retirement.PurposeSettledEntry))
				mustOK(t, err)
				want := scenario
				if scenario != "active" {
					want = "quiet-" + scenario
				}
				if verdict.NodeActivity != want {
					t.Fatalf("wrong activity: %s", out)
				}
				closing, closingCode := f.runRaw(t, "", settledCheckArgs(t, f, journal, retirement.PurposeSettledClosing)...)
				if scenario == "active" {
					_, err := retirement.DecodeSettledVerdict([]byte(closing), closingCode,
						settledExpectation(t, f, journal, retirement.PurposeSettledClosing))
					mustOK(t, err)
				} else {
					refusal, err := retirement.DecodeSettledRefusal([]byte(closing), closingCode, retirement.PurposeSettledClosing)
					if err != nil || refusal.Reason != retireReasonPostcondition || !strings.Contains(refusal.Why, "node") {
						t.Fatalf("quiet systemd state passed strict closing or failed elsewhere: %s (%v)", closing, err)
					}
				}
			default:
				want := "settled-entry-node-" + scenario
				switch scenario {
				case "queued job":
					want = "settled-entry-node-queued-job"
				case "unit mismatch":
					want = "settled-entry-node-unit-mismatch"
				}
				refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, retirement.PurposeSettledEntry)
				if err != nil || code != exitRefused || refusal.Reason != want {
					t.Fatalf("expected %s, got %s (%v)", want, out, err)
				}
			}
			if *observations == 0 {
				t.Fatal("production command never read the real manager")
			}
		})
	}
}

func installSettledObservationUnit(t *testing.T, unit, body string) string {
	t.Helper()
	path := filepath.Join(runtimeUnitDirectory, unit)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	mustOK(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Second)
		defer cancel()
		// Cancel pending jobs before stopping; the queued-job control must not
		// wait for its deliberately unready predecessor during cleanup.
		job, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--property=Job", "--value", "--", unit).Output()
		if err != nil {
			t.Errorf("cleanup job observation: %v", err)
		} else if fields := strings.Fields(string(job)); len(fields) > 0 {
			out, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "cancel", fields[0]).CombinedOutput()
			if err != nil {
				t.Errorf("cleanup cancel: %v: %s", err, out)
			}
		}
		// Reissue kill while a pending stop can still launch its ExecStop.
		// Removing the fragment is allowed only after the real manager is quiet.
		for {
			for _, args := range [][]string{{"kill", "--kill-whom=all", "--signal=KILL", "--", unit}, {"stop", "--no-block", "--", unit}} {
				out, err := exec.CommandContext(ctx, "/usr/bin/systemctl", args...).CombinedOutput()
				if err != nil {
					t.Logf("cleanup %v: %v: %s", args, err, out)
				}
			}
			out, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--property=ActiveState,MainPID,ControlPID,Job", "--", unit).Output()
			if err != nil {
				t.Fatalf("cleanup could not observe termination: %v", err)
			}
			lines := strings.Split(string(out), "\n")
			if (slices.Contains(lines, "ActiveState=inactive") || slices.Contains(lines, "ActiveState=failed")) &&
				slices.Contains(lines, "MainPID=0") && slices.Contains(lines, "ControlPID=0") && slices.Contains(lines, "Job=") {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		mustOK(t, os.Remove(path))
		out, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "daemon-reload").CombinedOutput()
		if err != nil {
			t.Errorf("cleanup reload: %v: %s", err, out)
		}
	})
	_, writeErr := file.WriteString(body)
	closeErr := file.Close()
	mustOK(t, writeErr)
	mustOK(t, closeErr)
	settledObservationCtl(t, "daemon-reload")
	return path
}

func settledObservationCtl(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/systemctl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl %v: %v: %s", args, err, out)
	}
}

func settledObservationProperty(t *testing.T, unit, property string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/usr/bin/systemctl", "show", "--property="+property, "--value", "--", unit).Output()
	mustOK(t, err)
	return strings.TrimSuffix(string(out), "\n")
}

func awaitSettledObservation(t *testing.T, unit, property, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := settledObservationProperty(t, unit, property)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s=%q, wanted %q", unit, property, got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Replace only properties the production query asked for. Returning a complete
// real snapshot would conceal a missing property in that query.
func forwardSettledManagerObservations(t *testing.T, unit string) *int {
	t.Helper()
	saved := managerCommandRunner
	t.Cleanup(func() { managerCommandRunner = saved })
	observations := 0
	names := []string{"ActiveState", "MainPID", "ControlPID", "Job", "InvocationID", "NeedDaemonReload"}
	managerCommandRunner = func(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error {
		if filepath.Base(bin) != "systemctl" || len(args) < 2 || args[0] != "show" || args[len(args)-1] != nodeUnit {
			return saved(ctx, bin, args, stdout, stderr)
		}
		var fake bytes.Buffer
		if err := saved(ctx, bin, args, &fake, stderr); err != nil {
			return err
		}
		requested := []string{}
		for _, arg := range args {
			if value, ok := strings.CutPrefix(arg, "--property="); ok {
				for _, name := range strings.Split(value, ",") {
					if slices.Contains(names, name) {
						requested = append(requested, name)
					}
				}
			}
		}
		// `show --all` asks for the whole inventory, which production does for
		// the retained node. Without this the fixture would answer every bridged
		// property there while explicit reads saw the real manager.
		if len(requested) == 0 && !slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--property=") }) {
			requested = slices.Clone(names)
		}
		if len(requested) == 0 {
			_, err := io.Copy(stdout, &fake)
			return err
		}
		realArgs := []string{"show", "--property=" + strings.Join(requested, ",")}
		if slices.Contains(args, "--all") {
			realArgs = append(realArgs, "--all")
		}
		realArgs = append(realArgs, "--", unit)
		cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", realArgs...)
		cmd.Stderr = stderr
		answer, err := cmd.Output()
		if err != nil {
			return err
		}
		observations++
		for _, line := range strings.SplitAfter(fake.String(), "\n") {
			key, _, _ := strings.Cut(line, "=")
			if !slices.Contains(requested, key) {
				if _, err := io.WriteString(stdout, line); err != nil {
					return err
				}
			}
		}
		_, err = stdout.Write(answer)
		return err
	}
	return &observations
}
