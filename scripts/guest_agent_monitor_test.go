package scripts_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// monitorMode is what the stand-in for `billet guest monitor` does once started.
type monitorMode int

const (
	// monitorFlushes takes a second over its final batch on SIGTERM, then exits 0.
	monitorFlushes monitorMode = iota
	// monitorFails exits 1 at once.
	monitorFails
	// monitorHangs ignores SIGTERM.
	monitorHangs
)

// THE GUEST MONITOR STARTS ONLY WHEN THE NODE ASKS, LEARNS ITS ENDPOINT AND BEARER
// FROM ITS ENVIRONMENT, AND NEVER CHANGES WHAT THE AGENT EXITS WITH.
//
// The agent's start block, the block that names the job-completed hook, and
// everything from the stop block to the agent's own `exit` are run as the image
// carries them, against a stand-in monitor, the real billet-exec-env, a recording
// `env` ahead of the real one, and stand-ins for systemctl and the image-store
// helper. What the agent exits with is what the image-store helper's
// service-status answers for the job's status, so a monitor that fails, hangs or
// never starts must leave that number exactly as the runner left it.
func TestTheGuestMonitorStartsOnlyWhenAskedAndNeverChangesTheJob(t *testing.T) {
	t.Parallel()

	// THE GUEST'S BASH IS 5.2, whose EPOCHREALTIME both the stop block's deadline
	// and this test's clock read. An older bash on PATH (macOS ships 3.2) runs the
	// block's poll-count backstop instead, which these bounds do not describe.
	clock, err := exec.CommandContext(t.Context(), "bash", "-c", `printf '%s' "${EPOCHREALTIME:-}"`).Output()
	if err != nil {
		t.Fatalf("ask bash for its clock: %v", err)
	}
	if len(clock) == 0 {
		t.Skip("the bash on PATH has no EPOCHREALTIME; the guest's bash 5.2 does")
	}

	startBlock := agentBlock(t, "BILLET_GUEST_MONITOR_START")
	hookBlock := agentBlock(t, "BILLET_GUEST_MONITOR_HOOK")
	tail := monitorStopTail(t)

	// THE REAL HELPER, installed executable as the image installs it.
	helper, err := os.ReadFile(filepath.Join("..", "internal", "guestassets", "exec-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	execEnv := filepath.Join(t.TempDir(), "billet-exec-env")
	if err := forkSafeWriteFile(execEnv, helper, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		endpoint = "http://172.31.0.1:7718"
		token    = "guest-bearer-s3cr3t"
	)

	for _, tc := range []struct {
		name       string
		key        string // the fetch stub's answer for guest-monitor: a value, "absent" or "unreadable"
		endpoint   string
		token      string
		mode       monitorMode
		notExec    bool // the guest's billet is not executable
		noHook     bool // the image carries no hook script
		sleepFails bool // the agent's sleep fails while it waits
		jobStatus  int
		started    bool
		// The seconds the agent spends in its stop block.
		atLeast, atMost float64
		flushed         bool // the monitor's final batch was finished before the helper ran
	}{
		{name: "asked, and it flushes on SIGTERM", key: "1", endpoint: endpoint, token: token,
			mode: monitorFlushes, jobStatus: 0, started: true, atLeast: 0.9, atMost: 2.5, flushed: true},
		{name: "asked, for a failed job", key: "1", endpoint: endpoint, token: token,
			mode: monitorFlushes, jobStatus: 3, started: true, atLeast: 0.9, atMost: 2.5, flushed: true},
		{name: "asked, and it fails at once", key: "1", endpoint: endpoint, token: token,
			mode: monitorFails, jobStatus: 3, started: true, atMost: 1},
		{name: "asked, and it ignores SIGTERM", key: "1", endpoint: endpoint, token: token,
			mode: monitorHangs, jobStatus: 0, started: true, atLeast: 2.9, atMost: 3.8},
		{name: "asked, but the image carries no hook", key: "1", endpoint: endpoint, token: token,
			mode: monitorFlushes, noHook: true, jobStatus: 0, started: true, atLeast: 0.9, atMost: 2.5,
			flushed: true},
		// A FAILED SLEEP UNDER set -e would otherwise end the agent before the image
		// store is settled; it ends the wait instead, and the monitor is killed.
		{name: "asked, and the agent's sleep fails", key: "1", endpoint: endpoint, token: token,
			mode: monitorHangs, sleepFails: true, jobStatus: 3, started: true, atMost: 1},
		{name: "not asked", key: "absent", endpoint: endpoint, token: token, jobStatus: 0, atMost: 1},
		{name: "the key could not be read", key: "unreadable", endpoint: endpoint, token: token,
			jobStatus: 3, atMost: 1},
		{name: "the key says something other than 1", key: "0", endpoint: endpoint, token: token,
			jobStatus: 0, atMost: 1},
		{name: "asked, with no cache endpoint", key: "1", token: token, jobStatus: 3, atMost: 1},
		{name: "asked, with no bearer", key: "1", endpoint: endpoint, jobStatus: 0, atMost: 1},
		{name: "asked, with no billet in the image", key: "1", endpoint: endpoint, token: token,
			notExec: true, jobStatus: 0, atMost: 1},
		// A NEWLINE WOULD BE A SECOND VARIABLE in the monitor's environment.
		{name: "asked, with a bearer holding a newline", key: "1", endpoint: endpoint,
			token: token + "\nINJECTED=1", jobStatus: 0, atMost: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			record := filepath.Join(dir, "record")
			if err := os.Mkdir(record, 0o755); err != nil {
				t.Fatal(err)
			}
			at := func(name string) string { return filepath.Join(record, name) }

			billet := filepath.Join(dir, "billet")
			mode := os.FileMode(0o755)
			if tc.notExec {
				mode = 0o644
			}
			if err := forkSafeWriteFile(billet, []byte(fakeMonitor(record, tc.mode)), mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { killRecordedMonitor(t, at("monitor.pid")) })

			hook := filepath.Join(dir, "job-completed-hook.sh")
			if !tc.noHook {
				if err := forkSafeWriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			fakes := writeFakeServiceManagers(t)
			if tc.sleepFails {
				sleep := filepath.Join(fakes, "sleep")
				if err := forkSafeWriteFile(sleep, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				// RUN ONCE BEFORE IT IS TIMED: the first exec of a freshly written file
				// can take seconds on a Mac, which would read as the agent waiting.
				var exit *exec.ExitError
				if err := exec.CommandContext(t.Context(), sleep).Run(); !errors.As(err, &exit) {
					t.Fatalf("the failing sleep did not fail: %v", err)
				}
			}
			// EVERY ARGUMENT THE START BLOCK'S `env` RECEIVES, before the real one
			// runs, so a value put on its command line is seen.
			if err := forkSafeWriteFile(filepath.Join(fakes, "env"), []byte("#!/bin/sh\n"+
				`printf '%s\n' "$@" >>`+shellQuote(at("env.argv"))+"\n"+
				`exec /usr/bin/env "$@"`+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			dockerCache := filepath.Join(fakes, "billet-docker-cache")
			if err := forkSafeWriteFile(dockerCache, []byte("#!/usr/bin/env bash\n"+
				`case "$1" in`+"\n"+
				`complete) flushed=no; if [ -e `+shellQuote(at("monitor.flushed"))+` ]; then flushed=yes; fi`+"\n"+
				`  printf 'complete %s flushed=%s\n' "$2" "$flushed" >>`+
				shellQuote(at("docker-cache"))+" ;;\n"+
				`service-status) printf '%s\n' "$2" ;;`+"\n"+
				"*) exit 2 ;;\nesac\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			var answer string
			switch tc.key {
			case "absent":
				answer = "return 3"
			case "unreadable":
				answer = "return 1"
			default:
				answer = "printf '%s' " + shellQuote(tc.key)
			}

			script := strings.Join([]string{
				"set -euo pipefail",
				`PATH="` + fakes + `:$PATH"`,
				"export BILLET_FAKE_RECORD=" + shellQuote(at("service")),
				`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
				`fetch() { case "$1" in guest-monitor) ` + answer + ` ;; *) return 3 ;; esac; }`,
				// WHAT THE AGENT HOLDS AND THE MONITOR MUST NEVER INHERIT.
				"export ACTIONS_RUNNER_INPUT_JITCONFIG=poisoned-registration",
				"cache_endpoint=" + shellQuote(tc.endpoint),
				"cache_token=" + shellQuote(tc.token),
				"GUEST_BILLET=" + shellQuote(billet),
				"EXEC_ENV=" + shellQuote(execEnv),
				"GUEST_MONITOR_HOOK=" + shellQuote(hook),
				startBlock,
				"runner_env=()",
				hookBlock,
				`for entry in ${runner_env[@]+"${runner_env[@]}"}; do printf 'env=%s\n' "$entry"; done`,
				// THE RUNNER RAN FOR A WHILE: the monitor is up before it is stopped.
				`if [ -n "$guest_monitor_pid" ]; then`,
				`  for _ in $(seq 1 1500); do if [ -e ` + shellQuote(at("monitor.ready")) + ` ]; then break; fi; /bin/sleep 0.01; done`,
				`  if [ ! -e ` + shellQuote(at("monitor.ready")) + ` ]; then echo "FIXTURE: the monitor never became ready" >&2; exit 97; fi`,
				"fi",
				"job_status=" + strconv.Itoa(tc.jobStatus),
				`printf 'stop_begin=%s\n' "$EPOCHREALTIME"`,
				strings.ReplaceAll(tail, "/usr/local/bin/billet-docker-cache", dockerCache),
			}, "\n")

			// A MONITOR THE AGENT WAITS ON FOREVER FAILS HERE rather than holding the suite.
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			run := exec.CommandContext(ctx, "bash", "-c", script)
			// A monitor left running holds the output pipes open after bash exits.
			run.WaitDelay = 2 * time.Second
			output, err := run.CombinedOutput()
			if strings.Contains(string(output), "FIXTURE:") {
				t.Fatalf("the stand-in monitor never set itself up, so nothing was proved:\n%s", output)
			}

			// THE AGENT'S EXIT STATUS IS THE JOB'S, through the image-store helper.
			status := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("the agent's tail did not run: %v\n%s", err, output)
				}
				status = exit.ExitCode()
			}
			if status != tc.jobStatus {
				t.Errorf("the agent exited %d for a job that exited %d:\n%s", status, tc.jobStatus, output)
			}

			// THE TAIL RAN TO THE END: the helper was told the job's status, once.
			completed := readArgv(t, at("docker-cache"))
			if len(completed) != 1 || !strings.HasPrefix(completed[0], "complete "+strconv.Itoa(tc.jobStatus)+" ") {
				t.Fatalf("the image-store helper was told %q, want one complete %d:\n%s",
					completed, tc.jobStatus, output)
			}

			argv := readArgv(t, at("monitor.argv"))
			if (argv != nil) != tc.started {
				t.Fatalf("the monitor started=%v, want %v:\n%s", argv != nil, tc.started, output)
			}

			hookEntry := "env=ACTIONS_RUNNER_HOOK_JOB_COMPLETED=" + hook + "\n"
			wantHook := tc.started && !tc.noHook
			if got := strings.Contains(string(output), hookEntry); got != wantHook {
				t.Errorf("the runner names the job-completed hook=%v, want %v:\n%s", got, wantHook, output)
			}
			if strings.Count(string(output), "ACTIONS_RUNNER_HOOK_JOB_COMPLETED=") > 1 {
				t.Errorf("the runner names the job-completed hook more than once:\n%s", output)
			}

			stopped := stopDuration(t, string(output))
			if stopped < tc.atLeast || stopped > tc.atMost {
				t.Errorf("the stop took %.2fs, want between %.2fs and %.2fs:\n%s",
					stopped, tc.atLeast, tc.atMost, output)
			}
			if got := strings.Contains(completed[0], "flushed=yes"); got != tc.flushed {
				t.Errorf("the monitor's final batch was finished before the image store was "+
					"settled=%v, want %v: %q", got, tc.flushed, completed[0])
			}

			if !tc.started {
				return
			}

			// ITS ARGUMENTS ARE FLAGS, AND THE VALUES ARE ITS ENVIRONMENT.
			want := []string{"guest", "monitor", "--junit-root", "/home/runner/runner/_work"}
			if !slices.Equal(argv, want) {
				t.Errorf("the monitor's argv is %q, want %q", argv, want)
			}
			for _, recorded := range []string{"monitor.argv", "env.argv"} {
				body, err := os.ReadFile(at(recorded))
				if err != nil {
					t.Fatalf("read %s: %v", recorded, err)
				}
				for _, value := range []string{tc.token, tc.endpoint, "poisoned-registration"} {
					if strings.Contains(string(body), value) {
						t.Errorf("%q is in an argument list (%s):\n%s", value, recorded, body)
					}
				}
			}
			for file, want := range map[string]string{
				"monitor.endpoint": tc.endpoint,
				"monitor.token":    tc.token,
				"monitor.jit":      "unset",
				"monitor.injected": "unset",
			} {
				got, err := os.ReadFile(at(file))
				if err != nil {
					t.Fatalf("read %s: %v", file, err)
				}
				if string(got) != want {
					t.Errorf("the monitor's environment gives %s=%q, want %q", file, got, want)
				}
			}

			// A MONITOR THAT IGNORED SIGTERM IS KILLED, and one that flushed was asked to.
			switch tc.mode {
			case monitorHangs:
				if !monitorGone(at("monitor.pid")) {
					t.Error("a monitor that ignored SIGTERM is still running after the agent's tail")
				}
			case monitorFlushes:
				if _, err := os.Stat(at("monitor.terminated")); err != nil {
					t.Errorf("the monitor was never sent SIGTERM: %v", err)
				}
			case monitorFails:
			}
		})
	}
}

// THE STOP COMES AFTER THE RUNNER AND BEFORE THE IMAGE STORE, and the hook is
// named before the launch that hands runner_env over. The executed test above
// runs the tail from the stop block on, so it cannot see where that tail sits.
func TestTheGuestMonitorIsStoppedAfterTheRunnerAndItsHookNamedBeforeTheLaunch(t *testing.T) {
	t.Parallel()

	agent := guestAgentBody(t)
	order := []string{
		"BILLET_GUEST_MONITOR_START_END",
		"BILLET_GUEST_MONITOR_HOOK_END",
		"BILLET_AGENT_LAUNCH_BEGIN",
		"BILLET_AGENT_LAUNCH_END",
		"BILLET_GUEST_MONITOR_STOP_BEGIN",
		"billet-docker-cache complete",
	}
	previous := -1
	for _, marker := range order {
		if strings.Count(agent, marker) != 1 {
			t.Fatalf("the agent carries %q %d times, want once", marker, strings.Count(agent, marker))
		}
		at := strings.Index(agent, marker)
		if at < previous {
			t.Errorf("%q comes before the step it must follow in %q", marker, order)
		}
		previous = at
	}
}

// THE HOOK RUNS INSIDE THE JOB, so it never fails it and never writes into its log,
// and it waits a bounded time whatever the binary does. It is run as installed,
// with the binary it names replaced by a stand-in, under sh and under bash -e.
func TestTheJobCompletedHookIsBoundedSilentAndNeverFails(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout on PATH; the hook bounds itself with coreutils' timeout, which every guest has")
	}

	raw, err := os.ReadFile(filepath.Join("..", "internal", "guestassets", "job-completed-hook.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const installed = "/opt/billet/bin/billet"
	if strings.Count(string(raw), installed) != 1 {
		t.Fatalf("the hook names %s %d times; this test replaces it once", installed,
			strings.Count(string(raw), installed))
	}

	for _, tc := range []struct {
		name     string
		body     string        // the stand-in billet; empty means there is none
		atMost   time.Duration // 0 leaves the time unchecked
		finishes bool          // the stand-in leaves <billet>.done once it has finished
	}{
		{name: "the monitor answers", body: "exit 0\n"},
		// A FLUSH THAT TAKES MOST OF ITS 2 S IS LET FINISH: the hook's own bound
		// is outside the flush's, never inside it.
		{name: "the monitor answers late", body: "/bin/sleep 2\n: >\"$0.done\"\nexit 0\n", finishes: true},
		{name: "no monitor answers", body: "echo 'no monitor answered' >&2\nexit 1\n"},
		{name: "the binary talks", body: "echo out\necho err >&2\nexit 0\n"},
		// timeout -k 1 3 ends it at 4 s. The stand-in is run once before it is timed,
		// because the first exec of a freshly written file can take seconds on a Mac.
		{name: "the binary hangs through SIGTERM",
			body: "trap '' TERM\nwhile :; do /bin/sleep 0.05; done\n", atMost: 5 * time.Second},
		{name: "there is no binary"},
	} {
		for _, shell := range [][]string{{"sh"}, {"bash", "-e"}} {
			t.Run(tc.name+" under "+strings.Join(shell, " "), func(t *testing.T) {
				t.Parallel()

				dir := t.TempDir()
				billet := filepath.Join(dir, "billet")
				argv := filepath.Join(dir, "argv")
				if tc.body != "" {
					body := "#!/bin/sh\nif [ \"${1-}\" = warm ]; then exit 0; fi\n" +
						"printf '%s\\n' \"$@\" >" + shellQuote(argv) + "\n" + tc.body
					if err := forkSafeWriteFile(billet, []byte(body), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := exec.CommandContext(t.Context(), billet, "warm").Run(); err != nil {
						t.Fatalf("run the stand-in once before timing it: %v", err)
					}
				}
				hook := filepath.Join(dir, "job-completed-hook.sh")
				if err := forkSafeWriteFile(hook,
					[]byte(strings.Replace(string(raw), installed, billet, 1)), 0o755); err != nil {
					t.Fatal(err)
				}

				// AN UNBOUNDED HOOK FAILS HERE rather than holding the suite.
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				run := exec.CommandContext(ctx, shell[0], append(shell[1:], hook)...)
				run.WaitDelay = 2 * time.Second
				began := time.Now()
				output, err := run.CombinedOutput()
				took := time.Since(began)

				if err != nil {
					t.Errorf("the hook failed, which fails the job: %v\n%s", err, output)
				}
				if len(output) != 0 {
					t.Errorf("the hook wrote %q into the job's log", output)
				}
				if tc.atMost > 0 && took > tc.atMost {
					t.Errorf("the hook took %s, want at most %s", took, tc.atMost)
				}
				if tc.body != "" {
					if got := readArgv(t, argv); !slices.Equal(got, []string{"guest", "flush"}) {
						t.Errorf("the hook ran billet %q, want guest flush", got)
					}
				}
				if tc.finishes {
					if _, err := os.Stat(billet + ".done"); err != nil {
						t.Errorf("the hook returned before the flush finished: %v", err)
					}
				}
			})
		}
	}
}

// THE PUBLICATION GATE REFUSES AN IMAGE WHOSE HOOK IS NOT THIS TREE'S. The gate's
// own block is run against fixture images.
func TestThePublicationGateRefusesAJobCompletedHookThatIsNotThisTrees(t *testing.T) {
	t.Parallel()

	gate := readScriptFile(t, "check-guest-image.sh")
	const begin = "# THE JOB-COMPLETED HOOK the agent names"
	_, after, found := strings.Cut(gate, begin)
	_, after, _ = strings.Cut(after, "\n")
	block, _, closed := strings.Cut(after, "\nfi\n")
	if !found || !closed {
		t.Fatal("check-guest-image.sh no longer has the job-completed hook block")
	}
	if !strings.Contains(block, `internal/guestassets/job-completed-hook.sh}"`) {
		t.Fatal("the gate does not default its source to internal/guestassets/job-completed-hook.sh")
	}

	source, err := filepath.Abs(filepath.Join("..", "internal", "guestassets", "job-completed-hook.sh"))
	if err != nil {
		t.Fatal(err)
	}
	hook, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		hook []byte
		mode os.FileMode
		pass bool
	}{
		{name: "this tree's hook", hook: hook, mode: 0o755, pass: true},
		{name: "a hook with other bytes", hook: append(append([]byte{}, hook...), "echo hello\n"...), mode: 0o755},
		{name: "a hook that is not executable", hook: hook, mode: 0o644},
		{name: "no hook at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mnt := t.TempDir()
			lib := filepath.Join(mnt, "usr", "local", "lib", "billet")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.hook != nil {
				if err := os.WriteFile(filepath.Join(lib, "job-completed-hook.sh"), tc.hook, tc.mode); err != nil {
					t.Fatal(err)
				}
			}

			// THE GATE'S OWN VERDICT: fail() sets FAILED, as the real one does, and
			// the run says which it was, so a block that said nothing is no verdict.
			script := "set -euo pipefail\nFAILED=0\n" +
				"pass() { echo \"PASS $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
				"MNT=" + shellQuote(mnt) + "\n" + block + "\nfi\n" + `echo "FAILED=$FAILED"` + "\n"
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "JOB_COMPLETED_HOOK_SOURCE=" + source}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the gate block did not run: %v\n%s", err, out)
			}
			want := "FAIL no executable /usr/local/lib/billet/job-completed-hook.sh identical to"
			verdict := "FAILED=1\n"
			if tc.pass {
				want, verdict = "PASS the job-completed hook asks the guest monitor", "FAILED=0\n"
			}
			if !strings.HasPrefix(string(out), want) || !strings.HasSuffix(string(out), verdict) {
				t.Errorf("the gate said %q, want it to begin %q and end %q", out, want, verdict)
			}
		})
	}
}

// monitorStopTail returns the agent from its monitor stop block to its own exit,
// as installed.
func monitorStopTail(t *testing.T) string {
	t.Helper()

	_, tail, found := strings.Cut(guestAgentBody(t), "BILLET_GUEST_MONITOR_STOP_BEGIN\n")
	if !found {
		t.Fatal("the guest agent no longer marks BILLET_GUEST_MONITOR_STOP")
	}
	// The stop block, then the image store settled and the agent's exit with it.
	if strings.Count(tail, "/usr/local/bin/billet-docker-cache") != 2 ||
		!strings.Contains(tail, `exit "$service_status"`) {
		t.Fatal("the agent's tail no longer runs from the monitor's stop to the image store and its exit")
	}
	// THE STOP BLOCK IS TIMED ON ITS OWN, at its end marker (a comment, so the
	// agent's text is otherwise unchanged): the first exec of a freshly written
	// stand-in can take seconds on a loaded Mac, and that is not the agent waiting.
	const end = "# BILLET_GUEST_MONITOR_STOP_END\n"
	if strings.Count(tail, end) != 1 {
		t.Fatal("the guest agent's monitor stop block has no single end marker")
	}

	return strings.Replace(tail, end, end+`printf 'stop_end=%s\n' "$EPOCHREALTIME"`+"\n", 1)
}

// fakeMonitor is a stand-in for `billet guest monitor` that records its argv and
// the environment the agent gave it under record. Its environment is only what
// billet-exec-env exports, so every path it writes is spelled out here.
func fakeMonitor(record string, mode monitorMode) string {
	file := func(name string) string { return shellQuote(filepath.Join(record, name)) }

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$@" >` + file("monitor.argv") + "\n")
	b.WriteString(`printf '%s' "${BILLET_CACHE_ENDPOINT-unset}" >` + file("monitor.endpoint") + "\n")
	b.WriteString(`printf '%s' "${BILLET_CACHE_TOKEN-unset}" >` + file("monitor.token") + "\n")
	b.WriteString(`printf '%s' "${ACTIONS_RUNNER_INPUT_JITCONFIG-unset}" >` + file("monitor.jit") + "\n")
	b.WriteString(`printf '%s' "${INJECTED-unset}" >` + file("monitor.injected") + "\n")
	b.WriteString(`printf '%s' "$$" >` + file("monitor.pid") + "\n")

	switch mode {
	case monitorFlushes:
		b.WriteString("trap ': >" + strings.ReplaceAll(file("monitor.terminated"), "'", `'\''`) +
			"; /bin/sleep 1; : >" + strings.ReplaceAll(file("monitor.flushed"), "'", `'\''`) +
			"; exit 0' TERM\n")
		b.WriteString(": >" + file("monitor.ready") + "\n")
		b.WriteString("while :; do /bin/sleep 0.05; done\n")
	case monitorFails:
		b.WriteString(": >" + file("monitor.ready") + "\nexit 1\n")
	case monitorHangs:
		b.WriteString("trap '' TERM\n")
		b.WriteString(": >" + file("monitor.ready") + "\n")
		b.WriteString("while :; do /bin/sleep 0.05; done\n")
	}

	return b.String()
}

// stopDuration is the seconds the agent spent in its monitor stop block.
func stopDuration(t *testing.T, output string) float64 {
	t.Helper()

	clocks := map[string]float64{}
	for _, line := range strings.Split(output, "\n") {
		for _, mark := range []string{"stop_begin", "stop_end"} {
			if value, ok := strings.CutPrefix(line, mark+"="); ok {
				seconds, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(value), ",", "."), 64)
				if err != nil {
					t.Fatalf("read the clock %q: %v", value, err)
				}
				clocks[mark] = seconds
			}
		}
	}
	if len(clocks) != 2 {
		t.Fatalf("the agent's tail did not mark both ends of its stop block:\n%s", output)
	}

	return clocks["stop_end"] - clocks["stop_begin"]
}

// monitorGone reports whether the process whose pid the stand-in recorded has
// ended, waiting up to two seconds for the kernel to say so.
func monitorGone(pidFile string) bool {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}

	return false
}

// killRecordedMonitor ends a stand-in a failing case left running.
func killRecordedMonitor(t *testing.T, pidFile string) {
	t.Helper()

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Logf("could not end the stand-in monitor %d: %v", pid, err)
	}
}
